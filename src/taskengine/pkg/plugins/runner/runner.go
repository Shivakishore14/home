package runner

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"taskengine/pkg/models"
	"taskengine/pkg/plugin"
)

func init() {
	plugin.RegisterWorker(&CommandRunnerPlugin{})
}

// CommandRunnerParams configures command execution, generic prerequisites, and task assets.
type CommandRunnerParams struct {
	Command       string                    `json:"command"`
	Shell         string                    `json:"shell,omitempty"`
	WorkingDir    string                    `json:"working_dir,omitempty"`
	Env           map[string]string         `json:"env,omitempty"`
	TaskName      string                    `json:"task_name,omitempty"`
	Prerequisites *models.TaskPrerequisites `json:"prerequisites,omitempty"`
}

// CommandRunnerPlugin executes arbitrary system commands/scripts with asset syncing and prerequisites.
type CommandRunnerPlugin struct {
	defaultShell string
	httpClient   *http.Client
}

// Name returns the plugin identifier.
func (p *CommandRunnerPlugin) Name() string {
	return "command-runner"
}

// Init configures default parameters for the runner.
func (p *CommandRunnerPlugin) Init(ctx context.Context, config json.RawMessage) error {
	p.httpClient = &http.Client{Timeout: 60 * time.Second}
	if len(config) > 0 {
		var cfg struct {
			Shell string `json:"shell"`
		}
		_ = json.Unmarshal(config, &cfg)
		if cfg.Shell != "" {
			p.defaultShell = cfg.Shell
		}
	}
	if p.defaultShell == "" {
		p.defaultShell = "/bin/sh"
	}
	return nil
}

var (
	progressRegex1 = regexp.MustCompile(`(?i)(?:progress|status):\s*([0-9]+(?:\.[0-9]+)?)\s*%?`)
	progressRegex2 = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)\s*%\s*$`)
)

// Execute handles asset synchronization, prerequisite validation, and command execution.
func (p *CommandRunnerPlugin) Execute(ctx context.Context, payload plugin.TaskPayload, reporter plugin.ProgressReporter) error {
	var params CommandRunnerParams
	if len(payload.Params) > 0 {
		if err := json.Unmarshal(payload.Params, &params); err != nil {
			return fmt.Errorf("invalid command params: %w", err)
		}
	}

	if params.Command == "" {
		return fmt.Errorf("command parameter is empty")
	}

	shell := params.Shell
	if shell == "" {
		shell = p.defaultShell
	}
	if shell == "" {
		shell = "/bin/sh"
	}

	serverURL := os.Getenv("SERVER_URL")
	workerID := os.Getenv("WORKER_ID")

	// Base task cache directory: ~/.taskengine
	homeDir, err := os.UserHomeDir()
	if err != nil {
		homeDir = "/tmp"
	}
	baseCacheDir := filepath.Join(homeDir, ".taskengine")

	taskName := params.TaskName
	if taskName == "" && payload.TargetFile != "" {
		taskName = payload.TargetFile
	}
	if taskName == "" {
		taskName = "default"
	}

	taskDir := filepath.Join(baseCacheDir, "tasks", taskName)
	assetsDir := filepath.Join(taskDir, "assets")
	_ = os.MkdirAll(assetsDir, 0755)

	// 1. Sync Task-Level Assets if serverURL is available
	if serverURL != "" && taskName != "default" {
		if err := p.syncTaskAssets(ctx, serverURL, taskName, taskDir, assetsDir, reporter); err != nil {
			_ = reporter.Report(ctx, plugin.ProgressReport{
				LogChunk: fmt.Sprintf("[Asset Sync Warning] %v\n", err),
			})
		}
	}

	// 2. Generic Task Prerequisites Check
	if params.Prerequisites != nil && params.Prerequisites.CheckCommand != "" {
		_ = reporter.Report(ctx, plugin.ProgressReport{
			LogChunk: fmt.Sprintf("[Prerequisites] Running check command: %s\n", params.Prerequisites.CheckCommand),
		})
		if err := p.runCheckCommand(ctx, shell, params.Prerequisites.CheckCommand, taskDir, assetsDir, params.Env, reporter); err != nil {
			return fmt.Errorf("prerequisites check failed: %w", err)
		}
		_ = reporter.Report(ctx, plugin.ProgressReport{
			LogChunk: "[Prerequisites] Check passed successfully.\n",
		})
	}

	// 3. Execute Main Command
	cmd := exec.CommandContext(ctx, shell, "-c", params.Command)
	if params.WorkingDir != "" {
		cmd.Dir = params.WorkingDir
	} else {
		cmd.Dir = taskDir
	}

	// Injected Environment variables
	env := cmd.Environ()
	env = append(env,
		fmt.Sprintf("TASK_ID=%s", payload.ID),
		fmt.Sprintf("TASK_PLUGIN=%s", payload.PluginName),
		fmt.Sprintf("TASK_NAME=%s", taskName),
		fmt.Sprintf("TASK_TARGET_FILE=%s", payload.TargetFile),
		fmt.Sprintf("TASK_PARAMS_JSON=%s", string(payload.Params)),
		fmt.Sprintf("TASK_DIR=%s", taskDir),
		fmt.Sprintf("TASK_ASSETS_DIR=%s", assetsDir),
		fmt.Sprintf("TASK_CACHE_DIR=%s", baseCacheDir),
		fmt.Sprintf("SERVER_URL=%s", serverURL),
		fmt.Sprintf("WORKER_ID=%s", workerID),
	)
	for k, v := range params.Env {
		env = append(env, fmt.Sprintf("%s=%s", k, v))
	}
	cmd.Env = env

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to create stdout pipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("failed to create stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start command: %w", err)
	}

	// Process output reader
	scanOutput := func(r io.Reader, prefix string) {
		scanner := bufio.NewScanner(r)
		for scanner.Scan() {
			line := scanner.Text()
			logLine := line + "\n"
			if prefix != "" {
				logLine = fmt.Sprintf("[%s] %s\n", prefix, line)
			}

			// Check for JSON progress
			var jsonProg struct {
				Progress float64 `json:"progress"`
				Speed    string  `json:"speed"`
				Message  string  `json:"message"`
			}
			if strings.HasPrefix(strings.TrimSpace(line), "{") && json.Unmarshal([]byte(line), &jsonProg) == nil && jsonProg.Progress > 0 {
				_ = reporter.Report(ctx, plugin.ProgressReport{
					Progress: jsonProg.Progress,
					Speed:    jsonProg.Speed,
					Message:  jsonProg.Message,
					LogChunk: logLine,
				})
				continue
			}

			// Check for regex progress
			var prog float64 = -1
			if matches := progressRegex1.FindStringSubmatch(line); len(matches) > 1 {
				prog, _ = strconv.ParseFloat(matches[1], 64)
			} else if matches := progressRegex2.FindStringSubmatch(strings.TrimSpace(line)); len(matches) > 1 {
				prog, _ = strconv.ParseFloat(matches[1], 64)
			}

			if prog >= 0 {
				_ = reporter.Report(ctx, plugin.ProgressReport{
					Progress: prog,
					Message:  strings.TrimSpace(line),
					LogChunk: logLine,
				})
			} else {
				_ = reporter.Report(ctx, plugin.ProgressReport{
					LogChunk: logLine,
				})
			}
		}
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		scanOutput(stdoutPipe, "")
	}()
	go func() {
		defer wg.Done()
		scanOutput(stderrPipe, "stderr")
	}()

	err = cmd.Wait()
	wg.Wait()

	if err != nil {
		return fmt.Errorf("command execution failed: %w", err)
	}

	return nil
}

// syncTaskAssets queries server for asset versions, downloads changed files, and writes .version.
func (p *CommandRunnerPlugin) syncTaskAssets(ctx context.Context, serverURL, taskName, taskDir, assetsDir string, reporter plugin.ProgressReporter) error {
	metaURL := fmt.Sprintf("%s/api/v1/tasks/%s/assets", serverURL, taskName)
	req, err := http.NewRequestWithContext(ctx, "GET", metaURL, nil)
	if err != nil {
		return err
	}

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil // No assets declared for this task
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("failed to fetch asset metadata: %d", resp.StatusCode)
	}

	var assets models.TaskAssets
	if err := json.NewDecoder(resp.Body).Decode(&assets); err != nil {
		return err
	}

	versionFile := filepath.Join(taskDir, ".version")
	currentVersionBytes, _ := os.ReadFile(versionFile)
	currentVersion := strings.TrimSpace(string(currentVersionBytes))

	allFilesExist := true
	for _, fileItem := range assets.Files {
		destPath := filepath.Join(assetsDir, filepath.Base(fileItem.Path))
		if _, err := os.Stat(destPath); os.IsNotExist(err) {
			allFilesExist = false
			break
		}
	}

	if currentVersion == assets.Version && len(assets.Files) > 0 && allFilesExist {
		// Assets up to date and all files present
		return nil
	}

	_ = reporter.Report(ctx, plugin.ProgressReport{
		LogChunk: fmt.Sprintf("[Asset Sync] Refreshing %d assets for task %s (Version: %s)\n", len(assets.Files), taskName, assets.Version),
	})

	for _, fileItem := range assets.Files {
		fileURL := fmt.Sprintf("%s/api/v1/files/%s", serverURL, fileItem.Path)
		fileReq, err := http.NewRequestWithContext(ctx, "GET", fileURL, nil)
		if err != nil {
			continue
		}

		fileResp, err := p.httpClient.Do(fileReq)
		if err != nil || fileResp.StatusCode != http.StatusOK {
			if fileResp != nil {
				fileResp.Body.Close()
			}
			continue
		}

		destPath := filepath.Join(assetsDir, filepath.Base(fileItem.Path))
		out, err := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
		if err == nil {
			_, _ = io.Copy(out, fileResp.Body)
			out.Close()
		}
		fileResp.Body.Close()
	}

	_ = os.WriteFile(versionFile, []byte(assets.Version), 0644)
	return nil
}

// runCheckCommand executes the prerequisite check command and verifies exit code 0.
func (p *CommandRunnerPlugin) runCheckCommand(ctx context.Context, shell, checkCmd, taskDir, assetsDir string, customEnv map[string]string, reporter plugin.ProgressReporter) error {
	cmd := exec.CommandContext(ctx, shell, "-c", checkCmd)
	cmd.Dir = taskDir

	env := cmd.Environ()
	env = append(env,
		fmt.Sprintf("TASK_DIR=%s", taskDir),
		fmt.Sprintf("TASK_ASSETS_DIR=%s", assetsDir),
	)
	for k, v := range customEnv {
		env = append(env, fmt.Sprintf("%s=%s", k, v))
	}
	cmd.Env = env

	output, err := cmd.CombinedOutput()
	if err != nil {
		_ = reporter.Report(ctx, plugin.ProgressReport{
			LogChunk: fmt.Sprintf("[Prerequisites Error Output]\n%s\n", string(output)),
		})
		return fmt.Errorf("%s: %w", string(output), err)
	}

	return nil
}
