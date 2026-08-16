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

	"taskengine/pkg/plugin"
)

const TranscodeCacheFileName = ".transcode_cache"

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

// copyFile copies a file from src to dst sequentially in 2MB chunks.
func copyFile(ctx context.Context, src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("failed to open source file %s: %w", src, err)
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("failed to create destination file %s: %w", dst, err)
	}
	defer out.Close()

	buf := make([]byte, 2*1024*1024) // 2MB buffer
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
}

// VideoTranscoderWorkerPlugin executes video transcoding jobs with optional local SSD scratch buffering.
type VideoTranscoderWorkerPlugin struct {
	defaultFFmpeg  string
	defaultFFprobe string
}

func (w *VideoTranscoderWorkerPlugin) Name() string {
	return "video-transcoder"
}

func (w *VideoTranscoderWorkerPlugin) Init(ctx context.Context, config json.RawMessage) error {
	if len(config) > 0 {
		var cfg struct {
			FFmpegBinary  string `json:"ffmpeg_binary"`
			FFprobeBinary string `json:"ffprobe_binary"`
		}
		_ = json.Unmarshal(config, &cfg)
		if cfg.FFmpegBinary != "" {
			w.defaultFFmpeg = cfg.FFmpegBinary
		}
		if cfg.FFprobeBinary != "" {
			w.defaultFFprobe = cfg.FFprobeBinary
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

	if _, err := os.Stat(remoteInputFile); err != nil {
		return fmt.Errorf("input video file not found: %s: %w", remoteInputFile, err)
	}

	dir := filepath.Dir(remoteInputFile)
	base := strings.TrimSuffix(filepath.Base(remoteInputFile), filepath.Ext(remoteInputFile))
	finalCleanRemoteFile := filepath.Join(dir, fmt.Sprintf("%s.mp4", base))

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
		_ = reporter.Report(ctx, plugin.ProgressReport{
			Progress: 0.5,
			Message:  fmt.Sprintf("Copying %s to local SSD scratch...", filepath.Base(remoteInputFile)),
			LogChunk: fmt.Sprintf("[Scratch Buffer] Pulling %s to local SSD (%s)\n", remoteInputFile, encodeInputPath),
		})

		if err := copyFile(ctx, remoteInputFile, encodeInputPath); err != nil {
			return fmt.Errorf("failed to copy source to scratch: %w", err)
		}
	} else {
		// Direct in-place encoding
		encodeInputPath = remoteInputFile
		encodeOutputPath = filepath.Join(dir, fmt.Sprintf(".temp.%s.mp4", base))
	}

	// 2. Probe input duration
	durationSec := getDuration(ctx, ffprobeBin, encodeInputPath)

	// 3. Build FFmpeg command for 1080p Universal Direct Play
	args := []string{
		"-nostdin",
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
		// Apple VideoToolbox hardware encoder
		args = append(args, "-q:v", strconv.Itoa(params.CRF*2), "-pix_fmt", "yuv420p")
	} else if strings.Contains(params.TargetCodec, "nvenc") {
		// NVIDIA NVENC hardware encoder
		args = append(args, "-cq", strconv.Itoa(params.CRF), "-preset", params.Preset, "-pix_fmt", "yuv420p")
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
		scanner := bufio.NewScanner(stderrPipe)
		for scanner.Scan() {
			_ = reporter.Report(ctx, plugin.ProgressReport{
				LogChunk: scanner.Text() + "\n",
			})
		}
	}()

	// Parse progress from stdout (-progress pipe:1)
	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(stdoutPipe)
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
		_ = reporter.Report(ctx, plugin.ProgressReport{
			Progress: 99.0,
			Message:  "Uploading transcoded MP4 to storage...",
			LogChunk: fmt.Sprintf("[Scratch Finalize] Uploading %s to %s\n", localFinalOutput, finalCleanRemoteFile),
		})

		if err := copyFile(ctx, localFinalOutput, finalCleanRemoteFile); err != nil {
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
