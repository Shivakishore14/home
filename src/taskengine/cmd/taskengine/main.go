package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"taskengine/pkg/config"
	"taskengine/pkg/db"
	"taskengine/pkg/plugin"
	_ "taskengine/pkg/plugins/runner"
	_ "taskengine/pkg/plugins/transcoder"
	"taskengine/pkg/server"
	"taskengine/pkg/worker"
)

const version = "1.0.0"

func printUsage() {
	fmt.Println(`TaskEngine - Distributed Task Orchestration System

Usage:
  taskengine <command> [arguments]

Commands:
  server    Start the TaskEngine server (API, SQLite, Scheduler, Web UI)
  worker    Start a TaskEngine worker node
  reload    Trigger hot configuration reload on a running server
  status    Check the status and stats of a running server
  version   Display version information

Run 'taskengine <command> --help' for details on each command.`)
}

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	cmd := os.Args[1]
	switch cmd {
	case "server":
		runServer(os.Args[2:])
	case "worker":
		runWorker(os.Args[2:])
	case "reload":
		runReload(os.Args[2:])
	case "retry-failed":
		runRetryFailed(os.Args[2:])
	case "status":
		runStatus(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Printf("TaskEngine v%s\n", version)
	case "help", "--help", "-h":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n\n", cmd)
		printUsage()
		os.Exit(1)
	}
}

func runServer(args []string) {
	fs := flag.NewFlagSet("server", flag.ExitOnError)
	port := fs.Int("port", 0, "Server HTTP port (overrides tasks/config.yaml)")
	tasksDir := fs.String("tasks-dir", "tasks", "Path to tasks configuration directory")
	dbPath := fs.String("db-path", "", "Path to SQLite database file (overrides tasks/config.yaml)")

	_ = fs.Parse(args)

	// If tasksDir does not exist at relative path, try checking parent directories
	resolvedTasksDir := *tasksDir
	if _, err := os.Stat(resolvedTasksDir); os.IsNotExist(err) {
		if _, err := os.Stat("../../tasks"); err == nil {
			resolvedTasksDir = "../../tasks"
		} else if _, err := os.Stat("../tasks"); err == nil {
			resolvedTasksDir = "../tasks"
		}
	}

	log.Printf("[TaskEngine Server] Loading configuration from: %s", resolvedTasksDir)
	cfgMgr, err := config.NewManager(resolvedTasksDir)
	if err != nil {
		log.Fatalf("Failed to initialize configuration manager: %v", err)
	}

	global := cfgMgr.GetGlobal()
	serverPort := global.Server.Port
	if *port > 0 {
		serverPort = *port
	}
	if serverPort <= 0 {
		serverPort = 8080
	}

	resolvedDBPath := global.Server.DBPath
	if *dbPath != "" {
		resolvedDBPath = *dbPath
	}
	if resolvedDBPath == "" {
		resolvedDBPath = "data/taskengine.db"
	}

	log.Printf("[TaskEngine Server] Opening database at: %s", resolvedDBPath)
	database, err := db.Open(resolvedDBPath)
	if err != nil {
		log.Fatalf("Failed to open SQLite database: %v", err)
	}
	defer database.Close()

	srv := server.NewServer(cfgMgr, database, plugin.DefaultRegistry)

	// Listen for shutdown signals
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	addr := fmt.Sprintf("0.0.0.0:%d", serverPort)
	go func() {
		if err := srv.Start(addr); err != nil {
			log.Fatalf("Server stopped: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("[TaskEngine Server] Shutting down gracefully...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	log.Println("[TaskEngine Server] Server stopped cleanly.")
}

func runWorker(args []string) {
	fs := flag.NewFlagSet("worker", flag.ExitOnError)
	serverURL := fs.String("server-url", "", "TaskEngine server base URL (e.g. http://192.168.1.100:8080) [Required]")
	serverShort := fs.String("server", "", "Alias for --server-url")
	workerID := fs.String("worker-id", "", "Unique worker identifier (defaults to hostname)")
	concurrency := fs.Int("concurrency", 0, "Max concurrent task executions (overrides server defaults)")
	plugins := fs.String("plugins", "", "Comma-separated list of enabled plugins (e.g. video-transcoder,command-runner)")
	pollInterval := fs.Duration("poll-interval", 2*time.Second, "Task polling interval")

	_ = fs.Parse(args)

	targetURL := *serverURL
	if targetURL == "" {
		targetURL = *serverShort
	}
	if targetURL == "" {
		fmt.Fprintln(os.Stderr, "Error: --server-url is required (e.g. taskengine worker --server-url http://localhost:8080)")
		fs.Usage()
		os.Exit(1)
	}

	var enabledPlugins []string
	if *plugins != "" {
		for _, p := range strings.Split(*plugins, ",") {
			trimmed := strings.TrimSpace(p)
			if trimmed != "" {
				enabledPlugins = append(enabledPlugins, trimmed)
			}
		}
	}

	w := worker.NewWorker(worker.Config{
		ServerURL:      targetURL,
		WorkerID:       *workerID,
		Concurrency:    *concurrency,
		EnabledPlugins: enabledPlugins,
		PollInterval:   *pollInterval,
	}, plugin.DefaultRegistry)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := w.Start(ctx); err != nil {
		log.Fatalf("Failed to start worker: %v", err)
	}

	<-ctx.Done()
	log.Println("[TaskEngine Worker] Shutting down worker...")
	w.Stop()
	log.Println("[TaskEngine Worker] Worker stopped cleanly.")
}

func runReload(args []string) {
	fs := flag.NewFlagSet("reload", flag.ExitOnError)
	serverURL := fs.String("server-url", "http://localhost:8080", "Server base URL")
	_ = fs.Parse(args)

	resp, err := http.Post(fmt.Sprintf("%s/api/v1/config/reload", *serverURL), "application/json", nil)
	if err != nil {
		log.Fatalf("Failed to connect to server: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusOK {
		fmt.Println("Config reloaded successfully.")
	} else {
		fmt.Printf("Config reload failed (%d): %s\n", resp.StatusCode, string(body))
		os.Exit(1)
	}
}

func runStatus(args []string) {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	serverURL := fs.String("server-url", "http://localhost:8080", "Server base URL")
	_ = fs.Parse(args)

	resp, err := http.Get(fmt.Sprintf("%s/api/v1/stats", *serverURL))
	if err != nil {
		log.Fatalf("Failed to connect to server: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	fmt.Printf("Server Status (%s):\n%s\n", *serverURL, string(body))
}

func runRetryFailed(args []string) {
	fs := flag.NewFlagSet("retry-failed", flag.ExitOnError)
	serverURL := fs.String("server-url", "http://localhost:8080", "Server base URL")
	_ = fs.Parse(args)

	resp, err := http.Post(fmt.Sprintf("%s/api/v1/tasks/retry-failed", *serverURL), "application/json", nil)
	if err != nil {
		log.Fatalf("Failed to connect to server: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusOK {
		fmt.Printf("Retry result: %s\n", string(body))
	} else {
		fmt.Printf("Retry failed (%d): %s\n", resp.StatusCode, string(body))
		os.Exit(1)
	}
}
