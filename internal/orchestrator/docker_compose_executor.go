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
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"
)

type dockerComposeExecutor interface {
	PullImage(ctx context.Context, service string) error

	// PruneDanglingImages removes dangling images — images with no tag and no
	// container referencing them. These accumulate after force-recreate deploys
	// where the old image layer is superseded by a new pull of the same tag.
	//
	// This is preferable to targeted RemoveImages because:
	//   - No need to track which refs to remove
	//   - Safe by definition: dangling = unreferenced, so nothing running can be affected
	//   - Handles the mutable-tag case (:dev-latest) where the old layers have
	//     no tag anymore after the new pull, making them unaddressable by ref anyway
	PruneDanglingImages(ctx context.Context) error
	RunMigrations(ctx context.Context, migrationService string) error
	RestartApp(ctx context.Context, appService string) error
	StopAll(ctx context.Context) error

	// CurrentRuntimeState implements RuntimeObserver.
	//
	// Strategy: two-phase inspection.
	//
	//  1. `docker compose ps --format json` — determines whether the app
	//     service container exists and is in a running state.
	//  2. `docker inspect <container-id>` — extracts the image digest the
	//     running container was actually started from.
	//
	// The digest comes from inspect's Image field (the image ID / digest the
	// daemon resolved at container-create time), not from the compose file or
	// the image tag — tags are mutable and cannot be trusted here.
	CurrentRuntimeState(ctx context.Context) (RuntimeState, error)
}

// dockerComposeExecutor runs docker compose commands for a single environment.
type dockerComposeExecutorImpl struct {
	// composeFile is the absolute path to the environment's docker-compose.yml.
	composeFile string
	// envFiles are sourced as --env-file arguments.
	envFiles []string

	appService string

	// logger is pre-configured with the environment field.
	logger *slog.Logger
}

// newDockerComposeExecutor creates an executor for the given compose file.
func newDockerComposeExecutor(composeFile string, envFiles []string, appService string, logger *slog.Logger) dockerComposeExecutor {
	return &dockerComposeExecutorImpl{
		composeFile: composeFile,
		envFiles:    envFiles,
		logger:      logger,
		appService:  appService,
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

func (e *dockerComposeExecutorImpl) PruneDanglingImages(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "docker", "image", "prune", "--force")
	cmd.Env = os.Environ()

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	e.logger.Debug("pruning dangling images", "phase", "image_prune")

	start := time.Now()
	err := cmd.Run()
	duration := time.Since(start)

	if err != nil {
		e.logger.Error("docker image prune failed",
			"phase", "image_prune",
			"duration_ms", duration.Milliseconds(),
			"stderr", strings.TrimSpace(stderr.String()),
			"error", err,
		)
		return fmt.Errorf("docker image prune: %w\nstderr: %s",
			err, strings.TrimSpace(stderr.String()))
	}

	e.logger.Info("dangling images pruned",
		"phase", "image_prune",
		"duration_ms", duration.Milliseconds(),
		"reclaimed", strings.TrimSpace(stdout.String()),
	)
	return nil
}

// isAllNoSuchImage reports whether every ref in refs appears in the stderr
// output as "No such image". docker rmi exits 1 when any image is missing,
// but we only want to suppress the error when ALL images were absent — a
// partial failure (some existed, some didn't) still warrants an error.
func isAllNoSuchImage(stderr string, refs []string) bool {
	for _, ref := range refs {
		// docker rmi outputs: Error: No such image: <ref>
		if !strings.Contains(stderr, "No such image: "+ref) {
			return false
		}
	}
	return true
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

func (e *dockerComposeExecutorImpl) CurrentRuntimeState(ctx context.Context) (RuntimeState, error) {
	containerID, running, err := e.getServiceState(ctx)
	if err != nil {
		return RuntimeState{}, err
	}
	if !running {
		return RuntimeState{Running: false}, nil
	}

	digest, err := e.inspectImageDigest(ctx, containerID)
	if err != nil {
		return RuntimeState{}, err
	}

	return RuntimeState{
		Running:        true,
		ManifestDigest: digest,
	}, nil
}

// composePSEntry is the JSON shape emitted by `docker compose ps --format json`.
// Only the fields we need are mapped; unknown fields are silently ignored.
type composePSEntry struct {
	// ID is the short container ID.
	ID string `json:"ID"`
	// Service is the compose service name (e.g. "app").
	Service string `json:"Service"`
	// State is the container state string: "running", "exited", "paused", etc.
	State string `json:"State"`
}

// getServiceState runs `docker compose ps --format json` for the configured
// app service and reports whether a container exists and is in state "running".
// Returns the container ID on success.
//
// `compose ps` output is one JSON object per line (NDJSON), not a JSON array.
// Each line represents one replica of a service.
// getServiceState — handle both JSON array and NDJSON from compose ps
func (e *dockerComposeExecutorImpl) getServiceState(ctx context.Context) (containerID string, running bool, err error) {
	args := e.buildArgs(
		"ps",
		"--format", "json",
		"--status", "running",
		"--status", "exited",
		"--status", "paused",
		"--status", "dead",
		e.appService,
	)

	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Env = os.Environ()

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", false, fmt.Errorf("docker compose ps: %w\nstderr: %s",
			err, strings.TrimSpace(stderr.String()))
	}

	entries, err := parseComposePSOutput(stdout.Bytes())
	if err != nil {
		return "", false, fmt.Errorf("parse compose ps output: %w", err)
	}

	for _, entry := range entries {
		if entry.Service != e.appService {
			continue
		}
		return entry.ID, entry.State == "running", nil
	}

	return "", false, nil
}

// parseComposePSOutput handles both output shapes docker compose ps emits:
//   - JSON array:  [{"ID":"abc","Service":"app","State":"running"}, ...]
//   - NDJSON:       {"ID":"abc","Service":"app","State":"running"}\n...
//
// Compose v2 switched from array to NDJSON at some point and the version in
// the wild varies by distro/install method, so we must tolerate both.
func parseComposePSOutput(data []byte) ([]composePSEntry, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, nil
	}

	// A JSON array starts with '['; NDJSON starts with '{'.
	if trimmed[0] == '[' {
		var entries []composePSEntry
		if err := json.Unmarshal(trimmed, &entries); err != nil {
			return nil, fmt.Errorf("parse JSON array: %w", err)
		}
		return entries, nil
	}

	// NDJSON: one JSON object per line.
	var entries []composePSEntry
	scanner := bufio.NewScanner(bytes.NewReader(trimmed))
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var entry composePSEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			return nil, fmt.Errorf("parse NDJSON line %q: %w", line, err)
		}
		entries = append(entries, entry)
	}
	return entries, scanner.Err()
}

// inspectImageDigest runs `docker inspect` on the container and returns the
// RepoDigest (sha256:...) of the image it was started from.
//
// `docker inspect` returns a JSON array; we always take index [0].
//
// The digest is extracted from Image.RepoDigests rather than Image.ID because:
//   - Image.ID is the config digest (sha256 of the image config blob), which
//     differs from the manifest digest that CI publishes.
//   - RepoDigests contains the registry manifest digest in the form
//     "registry/image@sha256:..." — this matches what GitHub Actions captures
//     via `docker buildx build --iidfile` or the push output.
func (e *dockerComposeExecutorImpl) inspectImageDigest(ctx context.Context, containerID string) (string, error) {
	// --format with a Go template avoids parsing the full 200-line inspect blob.
	const tmpl = `{{index (index .RepoDigests 0) | printf "%s"}}`

	cmd := exec.CommandContext(ctx,
		"docker", "inspect",
		"--format", `{{index .Image}}`,
		containerID,
	)
	cmd.Env = os.Environ()

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("docker inspect %s: %w\nstderr: %s",
			containerID, err, strings.TrimSpace(stderr.String()))
	}

	// `docker inspect --format '{{.Image}}'` returns the image content-
	// addressable ID: sha256:<config-digest>. We then resolve its RepoDigest.
	imageID := strings.TrimSpace(stdout.String())
	if imageID == "" {
		return "", fmt.Errorf("docker inspect %s: empty image ID", containerID)
	}

	return e.resolveRepoDigest(ctx, imageID)
}

// resolveRepoDigest — use a conditional template so Docker never indexes
// into an empty RepoDigests slice
func (e *dockerComposeExecutorImpl) resolveRepoDigest(ctx context.Context, imageID string) (string, error) {
	// {{if .RepoDigests}} guards the index call inside Docker's template engine.
	// Without it, {{index .RepoDigests 0}} causes a template execution error
	// when the slice is empty (e.g. image loaded via docker load), and the
	// process exits non-zero before our fallback logic can run.
	const tmpl = `{{if .RepoDigests}}{{index .RepoDigests 0}}{{end}}`

	cmd := exec.CommandContext(ctx,
		"docker", "image", "inspect",
		"--format", tmpl,
		imageID,
	)
	cmd.Env = os.Environ()

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("docker image inspect %s: %w\nstderr: %s",
			imageID, err, strings.TrimSpace(stderr.String()))
	}

	raw := strings.TrimSpace(stdout.String())
	if raw == "" {
		// Empty output means RepoDigests was empty — image has no registry
		// provenance (loaded from tarball, built locally, etc.).
		// Fall back to the image config ID; it won't match a CI manifest
		// digest so drift will always be detected — the safe default.
		e.logger.Warn("image has no RepoDigest, falling back to image ID",
			"phase", "runtime_observe",
			"image_id", imageID,
		)
		return imageID, nil
	}

	// RepoDigests[0] is "ghcr.io/toughbred/gymportal@sha256:abc123..."
	// Strip the registry/name@ prefix; keep only the sha256:... portion.
	if _, digest, found := strings.Cut(raw, "@"); found {
		return digest, nil
	}

	e.logger.Warn("unexpected RepoDigest format, returning raw value",
		"phase", "runtime_observe",
		"raw", raw,
	)
	return raw, nil
}
