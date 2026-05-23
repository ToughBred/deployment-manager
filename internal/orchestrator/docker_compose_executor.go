// Package orchestrator implements the deployment execution layer.
// It translates high-level deployment operations into docker compose commands
// executed via exec.Command.
//
// Architecture note: We intentionally shell out to `docker compose` rather
// than using the Docker SDK. This approach:
//  1. Keeps the agent simple and auditable — any operator can understand what
//     it does by reading the commands it runs.
//  2. Avoids Docker SDK version coupling — compose file compatibility is
//     Docker's problem, not ours.
//  3. Makes it easy to test manually by running the same commands by hand.
//
// Future: Replace exec-based compose calls with Docker SDK if finer-grained
// control (streaming logs, event subscription) becomes necessary.
package orchestrator

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"
)

type dockerComposeExecutor interface {
	PullImage(ctx context.Context, service string) error
	RunMigrations(ctx context.Context, migrationService string) error
	RestartApp(ctx context.Context, appService string) error
	StopAll(ctx context.Context) error
}

// dockerComposeExecutor runs docker compose commands for a single environment.
type dockerComposeExecutorImpl struct {
	// composeFile is the absolute path to the environment's docker-compose.yml.
	composeFile string
	// envFiles are sourced as --env-file arguments.
	envFiles []string
	// logger is pre-configured with the environment field.
	logger *slog.Logger
}

// newDockerComposeExecutor creates an executor for the given compose file.
func newDockerComposeExecutor(composeFile string, envFiles []string, logger *slog.Logger) dockerComposeExecutor {
	return &dockerComposeExecutorImpl{
		composeFile: composeFile,
		envFiles:    envFiles,
		logger:      logger,
	}
}

// PullImage pulls the specified image using docker compose pull.
// This ensures the image is available locally before restarting services.
// Using `compose pull` instead of `docker pull` respects compose service
// configuration (e.g. platform, pull policy).
func (e *dockerComposeExecutorImpl) PullImage(ctx context.Context, service string) error {
	return e.run(ctx, "image_pull", "pull", "--quiet", service)
}

// RunMigrations runs the migration service to completion.
// It uses `compose run --rm` which:
//   - Starts only the migration service (and its dependencies)
//   - Removes the container after it exits
//   - Returns the migration exit code as the command exit code
//
// A non-zero exit code from the migration service fails the deployment.
func (e *dockerComposeExecutorImpl) RunMigrations(ctx context.Context, migrationService string) error {
	return e.run(ctx, "migration",
		"run",
		"--rm",
		"--no-deps", // Don't restart app container during migration
		migrationService,
	)
}

// RestartApp restarts the application service with the updated image.
// Uses `up --force-recreate` to ensure containers are recreated even if
// the compose configuration hasn't changed (image digest changed, tag same).
func (e *dockerComposeExecutorImpl) RestartApp(ctx context.Context, appService string) error {
	return e.run(ctx, "restart",
		"up",
		"--detach",
		"--force-recreate",
		"--no-build",      // Never build — we only use pre-built images.
		"--pull", "never", // Image was already pulled explicitly.
		appService,
	)
}

// StopAll stops all services in the compose file.
// Used during rollback to ensure a clean slate.
func (e *dockerComposeExecutorImpl) StopAll(ctx context.Context) error {
	return e.run(ctx, "stop", "stop")
}

// run executes a docker compose command with the configured files and env files.
// It captures stdout and stderr for logging and error reporting.
func (e *dockerComposeExecutorImpl) run(ctx context.Context, phase string, args ...string) error {
	fullArgs := e.buildArgs(args...)

	cmd := exec.CommandContext(ctx, "docker", fullArgs...)
	cmd.Env = os.Environ() // Inherit the agent's environment variables.

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	e.logger.Debug("executing docker compose",
		"phase", phase,
		"args", strings.Join(fullArgs, " "),
	)

	start := time.Now()
	err := cmd.Run()
	duration := time.Since(start)

	if err != nil {
		e.logger.Error("docker compose command failed",
			"phase", phase,
			"duration_ms", duration.Milliseconds(),
			"stdout", strings.TrimSpace(stdout.String()),
			"stderr", strings.TrimSpace(stderr.String()),
			"error", err,
		)
		return fmt.Errorf("docker compose %s failed: %w\nstderr: %s",
			phase, err, strings.TrimSpace(stderr.String()))
	}

	e.logger.Info("docker compose command succeeded",
		"phase", phase,
		"duration_ms", duration.Milliseconds(),
	)
	return nil
}

// buildArgs constructs the full argument list for the docker compose invocation.
// Result: ["compose", "-f", "<file>", "--env-file", "<f1>", ..., <user-args>...]
func (e *dockerComposeExecutorImpl) buildArgs(userArgs ...string) []string {
	args := []string{"compose", "-f", e.composeFile}
	for _, ef := range e.envFiles {
		if ef != "" {
			args = append(args, "--env-file", ef)
		}
	}
	args = append(args, userArgs...)
	return args
}
