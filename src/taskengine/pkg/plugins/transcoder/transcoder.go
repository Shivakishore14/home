package transcoder

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"taskengine/pkg/plugin"
)

func init() {
	plugin.RegisterServer(&VideoTranscoderServerPlugin{})
	plugin.RegisterWorker(&VideoTranscoderWorkerPlugin{})
}

// VideoTranscoderServerPlugin scans directories and enqueues transcoding tasks.
type VideoTranscoderServerPlugin struct {
	Directory        string   `json:"directory"`
	TargetExtensions []string `json:"target_extensions"`
	ExcludePatterns  []string `json:"exclude_patterns"`
	TargetCodec      string   `json:"target_codec"`
	TargetHeight     int      `json:"target_height"`
	CRF              int      `json:"crf"`
	Preset           string   `json:"preset"`
	AudioBitrate     string   `json:"audio_bitrate"`
	Container        string   `json:"container"`
	ReplaceOriginal  bool     `json:"replace_original"`
}

func (s *VideoTranscoderServerPlugin) Name() string {
	return "video-transcoder"
}

func (s *VideoTranscoderServerPlugin) Init(ctx context.Context, config json.RawMessage) error {
	if len(config) > 0 {
		_ = json.Unmarshal(config, s)
	}
	if len(s.TargetExtensions) == 0 {
		s.TargetExtensions = []string{".mkv", ".mp4", ".mov", ".avi", ".ts", ".m2ts"}
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
	if s.Directory == "" {
		return nil, nil
	}

	var payloads []plugin.TaskPayload

	err := filepath.Walk(s.Directory, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}

		ext := strings.ToLower(filepath.Ext(path))
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

		// Skip if filename contains .transcoded.
		if strings.Contains(filepath.Base(path), ".transcoded.") {
			return nil
		}

		// Check if transcoded sibling already exists
		baseName := strings.TrimSuffix(filepath.Base(path), ext)
		transcodedSibling := filepath.Join(filepath.Dir(path), fmt.Sprintf("%s.transcoded.%s", baseName, s.Container))
		if _, err := os.Stat(transcodedSibling); err == nil {
			return nil // Already transcoded
		}

		params := map[string]interface{}{
			"target_codec":     s.TargetCodec,
			"target_height":    s.TargetHeight,
			"crf":              s.CRF,
			"preset":           s.Preset,
			"audio_bitrate":    s.AudioBitrate,
			"container":        s.Container,
			"replace_original": s.ReplaceOriginal,
		}
		paramsJSON, _ := json.Marshal(params)

		payloads = append(payloads, plugin.TaskPayload{
			PluginName: s.Name(),
			TargetFile: path,
			Params:     paramsJSON,
		})

		return nil
	})

	return payloads, err
}

// TranscodeParams configures 1080p video encoding for universal Jellyfin Direct Play.
type TranscodeParams struct {
	TargetCodec     string `json:"target_codec"`
	TargetHeight    int    `json:"target_height"`
	CRF             int    `json:"crf"`
	Preset          string `json:"preset"`
	AudioBitrate    string `json:"audio_bitrate"`
	Container       string `json:"container"`
	FFmpegBinary    string `json:"ffmpeg_binary"`
	FFprobeBinary   string `json:"ffprobe_binary"`
	ReplaceOriginal bool   `json:"replace_original"`
}

// VideoTranscoderWorkerPlugin executes video transcoding jobs.
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

	ffmpegBin := params.FFmpegBinary
	if ffmpegBin == "" {
		ffmpegBin = w.defaultFFmpeg
	}
	ffprobeBin := params.FFprobeBinary
	if ffprobeBin == "" {
		ffprobeBin = w.defaultFFprobe
	}

	inputFile := payload.TargetFile
	if inputFile == "" {
		return fmt.Errorf("target file is empty")
	}

	if _, err := os.Stat(inputFile); err != nil {
		return fmt.Errorf("input video file not found: %s: %w", inputFile, err)
	}

	// 1. Probe input duration
	durationSec := getDuration(ctx, ffprobeBin, inputFile)

	// 2. Determine output path
	dir := filepath.Dir(inputFile)
	base := strings.TrimSuffix(filepath.Base(inputFile), filepath.Ext(inputFile))
	outputFile := filepath.Join(dir, fmt.Sprintf("%s.transcoded.%s", base, params.Container))

	// 3. Build FFmpeg command for 1080p Universal Direct Play
	args := []string{
		"-nostdin",
		"-y",
		"-i", inputFile,
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
	if params.Container == "mp4" {
		args = append(args, "-movflags", "+faststart")
	}

	args = append(args, "-progress", "pipe:1", outputFile)

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
					pct := (currentSec / durationSec) * 100.0
					if pct > 100.0 {
						pct = 100.0
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
						Progress: 100.0,
						Speed:    currentSpeed,
						Message:  "Transcoding completed",
					})
				}
			}
		}
	}()

	err = cmd.Wait()
	wg.Wait()

	if err != nil {
		return fmt.Errorf("ffmpeg execution failed: %w", err)
	}

	// If replace_original is true, atomically replace
	if params.ReplaceOriginal {
		if err := os.Rename(outputFile, inputFile); err != nil {
			return fmt.Errorf("failed to replace original video file: %w", err)
		}
	}

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
