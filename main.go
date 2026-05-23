// Package main is the entry point for the deployment-manager.
//
// Startup sequence:
//  1. Configure structured logging
//  2. Load and validate configuration
//  3. Construct shared dependencies (GitHub client)
//  4. For each environment: construct state manager and reconciler
//  5. Launch each reconciler in its own goroutine
//  6. Block on SIGTERM/SIGINT for graceful shutdown
//
// Shutdown sequence:
//  1. Cancel the root context
//  2. All reconcilers detect cancellation and exit their loops
//  3. Wait for all goroutines to finish
//  4. Exit 0
//
// The agent is designed to be managed by systemd with Restart=on-failure.
// It exits non-zero if configuration is invalid or state directories cannot
// be created (operator action required), but recovers from transient errors
// (network failures, Docker unavailable) via the reconcile loop's retry logic.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/toughbred/deployment-manager/internal/notifier"

	"github.com/toughbred/deployment-manager/internal/config"
	"github.com/toughbred/deployment-manager/internal/git_provider"
	"github.com/toughbred/deployment-manager/internal/logger"
	"github.com/toughbred/deployment-manager/internal/reconciler"
	"github.com/toughbred/deployment-manager/internal/state"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "agent error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	// --- Flags ---
	configPath := flag.String("config", "missing config file", "path to agent config file")
	flag.Parse()
	if *configPath == "" {
		return fmt.Errorf("missing config path")
	}

	// --- Logging ---
	// Set up structured logging first so all subsequent errors are structured.
	log := logger.Setup()
	log.Info("Deployment manager starting", "config", *configPath)

	// --- Configuration ---
	cfg, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("load config error %q: %w", *configPath, err)
	}
	log.Info("configuration loaded",
		"environments", environmentNames(cfg.Environments),
		"poll_interval", cfg.PollIntervalInSeconds,
		"github_owner", cfg.GitHub.Owner,
		"github_repo", cfg.GitHub.Repo,
	)

	// --- GitHub client (shared across all reconcilers) ---
	// A single client is safe for concurrent use since it holds no per-request state.
	ghClient := git_provider.NewGithubClient(cfg.GitHub.Owner, cfg.GitHub.Repo, cfg.GitHub.Token)

	// --- Root context with signal cancellation ---
	// The context is cancelled on SIGTERM or SIGINT, which propagates to all
	// reconciler goroutines and their in-flight HTTP requests and exec calls.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// --- Launch reconcilers ---
	var wg sync.WaitGroup

	for _, env := range cfg.Environments {
		env := env // capture loop variable

		envLog := logger.WithEnvironment(log, env.Name)

		// Create state manager — this also creates the state directory.
		stateMgr, err := state.NewManagerWithLocalFileSystem(env.StateDir)
		if err != nil {
			// State directory creation failure is a hard error:
			// the agent cannot safely operate without persistence.
			return fmt.Errorf("failed to initialize state manager for %q environment:  %w", env.Name, err)
		}

		pollInterval := time.Duration(cfg.PollIntervalInSeconds) * time.Second
		notifier := notifier.NewSlackNotifier(env.NotificationUrl, log)
		rec := reconciler.New(env, pollInterval, ghClient, stateMgr, notifier, envLog)

		wg.Add(1)
		go func() {
			defer wg.Done()
			// The reconciler blocks until ctx is cancelled.
			// Panics from individual reconcilers are NOT recovered here by design —
			// a panic indicates a programming error and should crash the agent so
			// systemd can restart it and the on-call gets an alert.
			rec.StartPeriodicReconciliation(ctx)
			envLog.Info("reconciler exited", "environment", env.Name)
		}()

		slog.Info("reconciler launched", "environment", env.Name)
	}

	// Block until a signal is received.
	<-ctx.Done()
	log.Info("shutdown signal received, waiting for reconcilers to finish")

	// Wait for all reconciler goroutines to return.
	// Each reconciler will exit its loop on the next iteration after ctx is cancelled.
	wg.Wait()

	log.Info("agent shutdown complete")
	return nil
}

// environmentNames extracts environment name strings for logging.
func environmentNames(envs []config.EnvironmentConfig) []string {
	names := make([]string, len(envs))
	for i, e := range envs {
		names[i] = e.Name
	}
	return names
}
