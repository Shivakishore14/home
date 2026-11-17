package transcoder

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"taskengine/pkg/plugin"
)

const TranscodeCacheFileName = ".transcode_cache"

// incomingSettleTime is how long a file in the incoming staging area must be
// unmodified before the scanner queues it.
var incomingSettleTime = 5 * time.Minute

func init() {
	plugin.RegisterServer(&VideoTranscoderServerPlugin{})
	plugin.RegisterWorker(&VideoTranscoderWorkerPlugin{})
}

// readTranscodeCache reads the .transcode_cache in a directory into a lookup set.
func readTranscodeCache(dir string) map[string]bool {
	cachePath := filepath.Join(dir, TranscodeCacheFileName)
	entries := make(map[string]bool)
	f, err := os.Open(cachePath)
	if err != nil {
		return entries
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" && !strings.HasPrefix(line, "#") {
			entries[line] = true
		}
	}
	return entries
}

// appendTranscodeCache adds a completed filename to .transcode_cache in the directory.
func appendTranscodeCache(dir, filename string) error {
	existing := readTranscodeCache(dir)
	if existing[filename] {
		return nil
	}

	cachePath := filepath.Join(dir, TranscodeCacheFileName)
	f, err := os.OpenFile(cachePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = f.WriteString(filename + "\n")
	return err
}

// copyFileWithProgress copies a file from src to dst sequentially and emits progress reports.
func copyFileWithProgress(ctx context.Context, src, dst, action string, reporter plugin.ProgressReporter) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("failed to open source file %s: %w", src, err)
	}
	defer in.Close()

	fi, err := in.Stat()
	if err != nil {
		return err
	}
	totalBytes := fi.Size()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("failed to create destination file %s: %w", dst, err)
	}
	defer out.Close()

	buf := make([]byte, 2*1024*1024) // 2MB buffer
	var copied int64
	lastReport := time.Now()
	startTime := time.Now()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		n, rErr := in.Read(buf)
		if n > 0 {
			if _, wErr := out.Write(buf[:n]); wErr != nil {
				return fmt.Errorf("write error: %w", wErr)
			}
			copied += int64(n)

			if (time.Since(lastReport) >= 1500*time.Millisecond || copied == totalBytes) && totalBytes > 0 {
				lastReport = time.Now()
				elapsed := time.Since(startTime).Seconds()
				var speedMBps float64
				if elapsed > 0 {
					speedMBps = (float64(copied) / (1024 * 1024)) / elapsed
				}
				pct := float64(copied) / float64(totalBytes) * 100.0
				copiedMB := float64(copied) / (1024 * 1024)
				totalMB := float64(totalBytes) / (1024 * 1024)

				_ = reporter.Report(ctx, plugin.ProgressReport{
					Progress: 0.5,
					Speed:    fmt.Sprintf("%.1f MB/s", speedMBps),
					Message:  fmt.Sprintf("%s: %.1f%% (%.1f MB / %.1f MB) @ %.1f MB/s", action, pct, copiedMB, totalMB, speedMBps),
				})
			}
		}
		if rErr != nil {
			if rErr == io.EOF {
				break
			}
			return fmt.Errorf("read error: %w", rErr)
		}
	}

	return out.Sync()
}

// VideoTranscoderServerPlugin scans directories and enqueues transcoding tasks.
type VideoTranscoderServerPlugin struct {
	Directory        string   `json:"directory"`
	Directories      []string `json:"directories"`
	TargetExtensions []string `json:"target_extensions"`
	ExcludePatterns  []string `json:"exclude_patterns"`
	TargetCodec      string   `json:"target_codec"`
	TargetHeight     int      `json:"target_height"`
	CRF              int      `json:"crf"`
	Preset           string   `json:"preset"`
	AudioBitrate     string   `json:"audio_bitrate"`
	Container        string   `json:"container"`
	// IncomingDir, if set, is a path segment (e.g. "_incoming") marking a staging
	// area. Files under <root>/<IncomingDir>/... are written to <root>/... once
	// transcoded, so media servers only ever see finished files.
	IncomingDir string `json:"incoming_dir"`
}

func (s *VideoTranscoderServerPlugin) Name() string {
	return "video-transcoder"
}

func (s *VideoTranscoderServerPlugin) Init(ctx context.Context, config json.RawMessage) error {
	if len(config) > 0 {
		_ = json.Unmarshal(config, s)
	}
	if len(s.TargetExtensions) == 0 {
		s.TargetExtensions = []string{".mkv", ".mp4", ".mov", ".avi", ".ts", ".m2ts", ".wmv", ".flv"}
	}
	if s.TargetCodec == "" {
		s.TargetCodec = "libx264"
	}
	if s.TargetHeight <= 0 {
		s.TargetHeight = 1080
	}
	if s.Preset == "" {
		s.Preset = "medium"
	}
	if s.AudioBitrate == "" {
		s.AudioBitrate = "192k"
	}
	if s.Container == "" {
		s.Container = "mp4"
	}
	if s.CRF <= 0 {
		s.CRF = 21
	}
	return nil
}

func (s *VideoTranscoderServerPlugin) GenerateTasks(ctx context.Context) ([]plugin.TaskPayload, error) {
	var targetDirs []string
	if len(s.Directories) > 0 {
		targetDirs = append(targetDirs, s.Directories...)
	} else if s.Directory != "" {
		targetDirs = append(targetDirs, s.Directory)
	}

	if len(targetDirs) == 0 {
		return nil, nil
	}

	var payloads []plugin.TaskPayload
	dirCacheMap := make(map[string]map[string]bool)

	for _, rootDir := range targetDirs {
		if _, err := os.Stat(rootDir); os.IsNotExist(err) {
			continue
		}

		err := filepath.WalkDir(rootDir, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}

			filename := d.Name()
			// Ignore hidden files and temporary encodings
			if strings.HasPrefix(filename, ".") ||
				strings.HasSuffix(filename, ".temp.mp4") ||
				strings.HasSuffix(filename, ".transcoded.mp4") ||
				strings.HasSuffix(filename, ".part") {
				return nil
			}

			ext := strings.ToLower(filepath.Ext(filename))
			matchedExt := false
			for _, targetExt := range s.TargetExtensions {
				if ext == strings.ToLower(targetExt) {
					matchedExt = true
					break
				}
			}
			if !matchedExt {
				return nil
			}

			// In staging mode, leave files alone until they've stopped changing
			// (still being downloaded/copied in).
			if s.IncomingDir != "" {
				if info, ierr := d.Info(); ierr == nil && time.Since(info.ModTime()) < incomingSettleTime {
					return nil
				}
			}

			dir := filepath.Dir(path)
			cache, ok := dirCacheMap[dir]
			if !ok {
				cache = readTranscodeCache(dir)
				dirCacheMap[dir] = cache
			}

			// Check if file itself or clean target .mp4 is in .transcode_cache
			baseName := strings.TrimSuffix(filename, ext)
			cleanMP4 := fmt.Sprintf("%s.mp4", baseName)

			if cache[filename] || cache[cleanMP4] {
				return nil // Already transcoded and cached locally
			}

			params := map[string]interface{}{
				"target_codec":  s.TargetCodec,
				"target_height": s.TargetHeight,
				"crf":           s.CRF,
				"preset":        s.Preset,
				"audio_bitrate": s.AudioBitrate,
				"container":     s.Container,
			}
			if s.IncomingDir != "" {
				params["incoming_dir"] = s.IncomingDir
			}
			paramsJSON, _ := json.Marshal(params)

			payloads = append(payloads, plugin.TaskPayload{
				PluginName: s.Name(),
				TargetFile: path,
				Params:     paramsJSON,
			})

			return nil
		})

		if err != nil {
			log.Printf("[Scanner Error %s] %v", rootDir, err)
		}
	}

	return payloads, nil
}

// TranscodeParams configures 1080p video encoding for universal Jellyfin Direct Play.
type TranscodeParams struct {
	TargetCodec   string `json:"target_codec"`
	TargetHeight  int    `json:"target_height"`
	CRF           int    `json:"crf"`
	Preset        string `json:"preset"`
	AudioBitrate  string `json:"audio_bitrate"`
	Container     string `json:"container"`
	ScratchDir    string `json:"scratch_dir"`
	FFmpegBinary  string `json:"ffmpeg_binary"`
	FFprobeBinary string `json:"ffprobe_binary"`
	IncomingDir   string `json:"incoming_dir"`
}

// VideoTranscoderWorkerPlugin executes video transcoding jobs with optional local SSD scratch buffering.
type VideoTranscoderWorkerPlugin struct {
	defaultFFmpeg  string
	defaultFFprobe string
	defaultCodec   string
	defaultPreset  string
	defaultCRF     int
}

func (w *VideoTranscoderWorkerPlugin) Name() string {
	return "video-transcoder"
}

func (w *VideoTranscoderWorkerPlugin) Init(ctx context.Context, config json.RawMessage) error {
	if len(config) > 0 {
		var cfg struct {
			FFmpegBinary  string `json:"ffmpeg_binary"`
			FFprobeBinary string `json:"ffprobe_binary"`
			TargetCodec   string `json:"target_codec"`
			Preset        string `json:"preset"`
			CRF           int    `json:"crf"`
		}
		_ = json.Unmarshal(config, &cfg)
		if cfg.FFmpegBinary != "" {
			w.defaultFFmpeg = cfg.FFmpegBinary
		}
		if cfg.FFprobeBinary != "" {
			w.defaultFFprobe = cfg.FFprobeBinary
		}
		if cfg.TargetCodec != "" {
			w.defaultCodec = cfg.TargetCodec
		}
		if cfg.Preset != "" {
			w.defaultPreset = cfg.Preset
		}
		if cfg.CRF > 0 {
			w.defaultCRF = cfg.CRF
		}
	}
	if w.defaultFFmpeg == "" {
		w.defaultFFmpeg = "ffmpeg"
	}
	if w.defaultFFprobe == "" {
		w.defaultFFprobe = "ffprobe"
	}
	return nil
}

func resolveHomePath(p string) string {
	if p == "" {
		return ""
	}
	if strings.HasPrefix(p, "~/") || p == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			trimmed := strings.TrimPrefix(p, "~")
			trimmed = strings.TrimPrefix(trimmed, "/")
			return filepath.Join(home, trimmed)
		}
	}
	return p
}

func (w *VideoTranscoderWorkerPlugin) Execute(ctx context.Context, payload plugin.TaskPayload, reporter plugin.ProgressReporter) error {
	var params TranscodeParams
	if len(payload.Params) > 0 {
		_ = json.Unmarshal(payload.Params, &params)
	}

	// Worker-level hardware encoder configuration overrides task-level defaults
	if w.defaultCodec != "" {
		params.TargetCodec = w.defaultCodec
	}
	if w.defaultPreset != "" {
		params.Preset = w.defaultPreset
	}
	if w.defaultCRF > 0 {
		params.CRF = w.defaultCRF
	}

	if params.TargetCodec == "" {
		params.TargetCodec = "libx264"
	}
	if params.TargetHeight <= 0 {
		params.TargetHeight = 1080
	}
	if params.Preset == "" {
		params.Preset = "medium"
	}
	if params.AudioBitrate == "" {
		params.AudioBitrate = "192k"
	}
	if params.Container == "" {
		params.Container = "mp4"
	}
	if params.CRF <= 0 {
		params.CRF = 21
	}

	scratchDir := params.ScratchDir
	if scratchDir == "" {
		scratchDir = os.Getenv("SCRATCH_DIR")
	}

	ffmpegBin := params.FFmpegBinary
	if ffmpegBin == "" {
		ffmpegBin = w.defaultFFmpeg
	}
	ffprobeBin := params.FFprobeBinary
	if ffprobeBin == "" {
		ffprobeBin = w.defaultFFprobe
	}

	remoteInputFile := resolveHomePath(payload.TargetFile)
	if remoteInputFile == "" {
		return fmt.Errorf("target file is empty")
	}

	srcDir := filepath.Dir(remoteInputFile)
	base := strings.TrimSuffix(filepath.Base(remoteInputFile), filepath.Ext(remoteInputFile))
	// dir is where the finished file lands: srcDir itself, or the library
	// equivalent of srcDir when the source sits in the incoming staging area.
	dir, staged := libraryDir(srcDir, params.IncomingDir)
	finalCleanRemoteFile := filepath.Join(dir, fmt.Sprintf("%s.mp4", base))
	if staged {
		if err := os.MkdirAll(dir, 0775); err != nil {
			return fmt.Errorf("failed to create library directory %s: %w", dir, err)
		}
	}

	if err := checkInputFile(remoteInputFile, finalCleanRemoteFile); err != nil {
		return err
	}

	var encodeInputPath string
	var encodeOutputPath string
	var localFinalOutput string
	var taskScratchDir string

	if scratchDir != "" {
		resolvedScratch := resolveHomePath(scratchDir)
		taskScratchDir = filepath.Join(resolvedScratch, payload.ID)
		_ = os.MkdirAll(taskScratchDir, 0755)
		defer os.RemoveAll(taskScratchDir)

		encodeInputPath = filepath.Join(taskScratchDir, filepath.Base(remoteInputFile))
		encodeOutputPath = filepath.Join(taskScratchDir, fmt.Sprintf(".temp.%s.mp4", base))
		localFinalOutput = filepath.Join(taskScratchDir, fmt.Sprintf("%s.mp4", base))

		// 1. Buffer source to local scratch SSD
		if err := copyFileWithProgress(ctx, remoteInputFile, encodeInputPath, "Buffering source to local SSD", reporter); err != nil {
			return fmt.Errorf("failed to copy source to scratch: %w", err)
		}
	} else {
		// Direct in-place encoding
		encodeInputPath = remoteInputFile
		encodeOutputPath = filepath.Join(srcDir, fmt.Sprintf(".temp.%s.mp4", base))
	}

	// 2. Probe input duration
	durationSec := getDuration(ctx, ffprobeBin, encodeInputPath)

	_ = reporter.Report(ctx, plugin.ProgressReport{
		Progress: 1.0,
		Message:  fmt.Sprintf("Starting encode with [%s] (Preset: %s, CRF/CQ: %d)", params.TargetCodec, params.Preset, params.CRF),
		LogChunk: fmt.Sprintf("[Transcoder] Launching FFmpeg with encoder %s (Preset: %s, CRF: %d)\n", params.TargetCodec, params.Preset, params.CRF),
	})

	// 3. Build FFmpeg command for 1080p Universal Direct Play
	args := []string{
		"-nostdin",
		"-nostats", // progress comes from -progress; the \r-redrawn stats line is noise
		"-y",
		"-i", encodeInputPath,
	}

	// 1080p Downscale filter (max width 1920, preserve aspect ratio, even height)
	maxWidth := params.TargetHeight * 16 / 9
	scaleFilter := fmt.Sprintf("scale='min(%d,iw)':-2", maxWidth)
	args = append(args, "-vf", scaleFilter)

	// Video Codec & Quality
	args = append(args, "-c:v", params.TargetCodec)

	if strings.Contains(params.TargetCodec, "videotoolbox") {
		// Apple VideoToolbox hardware encoder (macOS)
		args = append(args, "-q:v", strconv.Itoa(params.CRF*2), "-pix_fmt", "yuv420p")
	} else if strings.Contains(params.TargetCodec, "nvenc") {
		// NVIDIA NVENC hardware encoder (Linux / Windows)
		args = append(args, "-cq", strconv.Itoa(params.CRF), "-preset", params.Preset, "-pix_fmt", "yuv420p")
	} else if strings.Contains(params.TargetCodec, "vaapi") {
		// Intel / AMD VAAPI hardware encoder (Linux)
		args = append(args, "-qp", strconv.Itoa(params.CRF), "-pix_fmt", "yuv420p")
	} else if strings.Contains(params.TargetCodec, "qsv") {
		// Intel QuickSync Video encoder (Linux / Windows)
		args = append(args, "-global_quality", strconv.Itoa(params.CRF), "-preset", params.Preset, "-pix_fmt", "yuv420p")
	} else if strings.Contains(params.TargetCodec, "amf") {
		// AMD AMF hardware encoder (Linux / Windows)
		args = append(args, "-rc", "cqp", "-qp_p", strconv.Itoa(params.CRF), "-qp_i", strconv.Itoa(params.CRF), "-pix_fmt", "yuv420p")
	} else {
		// Standard libx264 (8-bit SDR, High Profile Level 4.1 for 100% universal hardware support)
		args = append(args, "-crf", strconv.Itoa(params.CRF), "-preset", params.Preset, "-pix_fmt", "yuv420p", "-profile:v", "high", "-level", "4.1")
	}

	// Audio: AAC Stereo 2.0 @ 192k (Zero audio transcoding on phones/tablets/TVs/browsers)
	args = append(args, "-c:a", "aac", "-b:a", params.AudioBitrate, "-ac", "2")

	// MP4 faststart for instant streaming
	args = append(args, "-movflags", "+faststart")

	args = append(args, "-progress", "pipe:1", encodeOutputPath)

	cmd := exec.CommandContext(ctx, ffmpegBin, args...)

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to create stdout pipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("failed to create stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start ffmpeg: %w", err)
	}

	var wg sync.WaitGroup
	wg.Add(2)

	// Read stderr for general logs
	go func() {
		defer wg.Done()
		scanner := newLineScanner(stderrPipe)
		for scanner.Scan() {
			_ = reporter.Report(ctx, plugin.ProgressReport{
				LogChunk: scanner.Text() + "\n",
			})
		}
		// Never stop draining: an unread pipe blocks ffmpeg forever.
		_, _ = io.Copy(io.Discard, stderrPipe)
	}()

	// Parse progress from stdout (-progress pipe:1)
	go func() {
		defer wg.Done()
		scanner := newLineScanner(stdoutPipe)
		defer io.Copy(io.Discard, stdoutPipe) // keep draining if Scan stops early
		var currentSpeed string
		for scanner.Scan() {
			line := scanner.Text()
			parts := strings.SplitN(line, "=", 2)
			if len(parts) != 2 {
				continue
			}
			key := strings.TrimSpace(parts[0])
			val := strings.TrimSpace(parts[1])

			switch key {
			case "speed":
				currentSpeed = val
			case "out_time_us":
				us, err := strconv.ParseInt(val, 10, 64)
				if err == nil && durationSec > 0 {
					currentSec := float64(us) / 1000000.0
					pct := (currentSec / durationSec) * 98.0
					if pct > 98.0 {
						pct = 98.0
					}
					_ = reporter.Report(ctx, plugin.ProgressReport{
						Progress: pct,
						Speed:    currentSpeed,
						Message:  fmt.Sprintf("Encoding 1080p: %.1f%% (%.1fs / %.1fs)", pct, currentSec, durationSec),
					})
				}
			case "progress":
				if val == "end" {
					_ = reporter.Report(ctx, plugin.ProgressReport{
						Progress: 98.0,
						Speed:    currentSpeed,
						Message:  "Transcoding completed on local SSD",
					})
				}
			}
		}
	}()

	err = cmd.Wait()
	wg.Wait()

	if err != nil {
		_ = os.Remove(encodeOutputPath) // Clean up temp file on failure
		return fmt.Errorf("ffmpeg execution failed: %w", err)
	}

	// Verify temp output file exists and is non-empty
	fi, err := os.Stat(encodeOutputPath)
	if err != nil || fi.Size() == 0 {
		_ = os.Remove(encodeOutputPath)
		return fmt.Errorf("ffmpeg produced empty or missing output file: %w", err)
	}

	// Finalization Phase
	if scratchDir != "" {
		// Rename temp output to local clean file
		if err := os.Rename(encodeOutputPath, localFinalOutput); err != nil {
			return fmt.Errorf("failed to finalize scratch file: %w", err)
		}

		// Upload back to remote storage (SMB/HDD)
		if err := copyFileWithProgress(ctx, localFinalOutput, finalCleanRemoteFile, "Uploading transcoded MP4 to storage", reporter); err != nil {
			return fmt.Errorf("failed to upload transcoded file to storage: %w", err)
		}
	} else {
		// Atomically rename local temp file to clean final .mp4
		if err := os.Rename(encodeOutputPath, finalCleanRemoteFile); err != nil {
			return fmt.Errorf("failed to finalize transcoded file: %w", err)
		}
	}

	// Clean replacement: remove original input file if extension differed
	if remoteInputFile != finalCleanRemoteFile {
		_ = os.Remove(remoteInputFile)
	}

	if staged {
		moveSidecars(srcDir, dir, base)
		pruneEmptyDirs(srcDir, params.IncomingDir)
	}

	// Append to directory's local .transcode_cache
	finalFileName := filepath.Base(finalCleanRemoteFile)
	if err := appendTranscodeCache(dir, finalFileName); err != nil {
		_ = reporter.Report(ctx, plugin.ProgressReport{
			LogChunk: fmt.Sprintf("[Warning] Failed to write .transcode_cache: %v\n", err),
		})
	}

	_ = reporter.Report(ctx, plugin.ProgressReport{
		Progress: 100.0,
		Message:  "Transcode completed successfully",
		LogChunk: fmt.Sprintf("[Complete] File %s is ready for Jellyfin Direct Play\n", finalFileName),
	})

	return nil
}

// getDuration uses ffprobe to determine video length in seconds.
func getDuration(ctx context.Context, ffprobeBin, filePath string) float64 {
	cmd := exec.CommandContext(ctx, ffprobeBin,
		"-v", "error",
		"-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1",
		filePath,
	)
	out, err := cmd.Output()
	if err != nil {
		return 0
	}
	sec, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil {
		return 0
	}
	return sec
}

// checkInputFile decides what to do when a task's source file may be gone.
//   - source exists: nil (proceed)
//   - source gone, but the transcoded output exists next to it: skip (already done)
//   - source gone, parent dir present: permanent failure (file was removed/renamed)
//   - parent dir also gone: retryable error (share may be unmounted / offline)
func checkInputFile(input, output string) error {
	_, err := os.Stat(input)
	if err == nil {
		return nil
	}
	if !os.IsNotExist(err) {
		return fmt.Errorf("cannot access input video file: %s: %w", input, err)
	}
	if input != output {
		if _, oerr := os.Stat(output); oerr == nil {
			return plugin.Skip("source already transcoded: %s", output)
		}
	}
	if _, derr := os.Stat(filepath.Dir(input)); derr == nil {
		return plugin.Permanent(fmt.Errorf("input video file not found (directory exists, file removed): %s", input))
	}
	return fmt.Errorf("input video file not found: %s: %w", input, err)
}

// libraryDir maps a directory inside the incoming staging area to its library
// equivalent by dropping the last path segment equal to incoming.
// e.g. /m/_incoming/Movies/X -> /m/Movies/X. Returns (dir, false) if not staged.
func libraryDir(dir, incoming string) (string, bool) {
	if incoming == "" {
		return dir, false
	}
	parts := strings.Split(filepath.Clean(dir), string(filepath.Separator))
	for i := len(parts) - 1; i >= 0; i-- {
		if parts[i] == incoming {
			out := append(append([]string{}, parts[:i]...), parts[i+1:]...)
			joined := strings.Join(out, string(filepath.Separator))
			if joined == "" {
				joined = string(filepath.Separator)
			}
			return joined, true
		}
	}
	return dir, false
}

// moveSidecars moves files named "<base>.*" (subtitles, .nfo, artwork) from
// src to dst alongside the transcoded video. Failures are non-fatal.
func moveSidecars(src, dst, base string) {
	entries, err := os.ReadDir(src)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, base+".") {
			continue
		}
		from, to := filepath.Join(src, name), filepath.Join(dst, name)
		if err := os.Rename(from, to); err != nil {
			// Cross-device fallback.
			if data, rerr := os.ReadFile(from); rerr == nil && os.WriteFile(to, data, 0664) == nil {
				_ = os.Remove(from)
			}
		}
	}
}

// pruneEmptyDirs removes dir and its empty parents, keeping the incoming
// staging root and its direct children (the per-library drop folders, e.g.
// _incoming/Movies).
func pruneEmptyDirs(dir, incoming string) {
	for {
		if filepath.Base(dir) == incoming || filepath.Base(filepath.Dir(dir)) == incoming || dir == filepath.Dir(dir) {
			return
		}
		if err := os.Remove(dir); err != nil { // fails if not empty
			return
		}
		dir = filepath.Dir(dir)
	}
}

// newLineScanner returns a Scanner that treats \r, \n and \r\n as line ends
// (ffmpeg redraws status lines with \r) and tolerates long lines.
func newLineScanner(r io.Reader) *bufio.Scanner {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	sc.Split(scanCRLF)
	return sc
}

func scanCRLF(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	for i, b := range data {
		if b == '\n' {
			return i + 1, data[:i], nil
		}
		if b == '\r' {
			if i+1 < len(data) && data[i+1] == '\n' {
				return i + 2, data[:i], nil
			}
			if i+1 == len(data) && !atEOF {
				return 0, nil, nil // need more data to tell \r from \r\n
			}
			return i + 1, data[:i], nil
		}
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}
