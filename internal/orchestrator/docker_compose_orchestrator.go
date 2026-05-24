package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/toughbred/deployment-manager/internal/config"
	"github.com/toughbred/deployment-manager/internal/git_provider"
	"github.com/toughbred/deployment-manager/internal/health"
	"github.com/toughbred/deployment-manager/internal/logger"
	"github.com/toughbred/deployment-manager/internal/notifier"
	"github.com/toughbred/deployment-manager/internal/state"
)

type dockerCompose struct {
	env             config.EnvironmentConfig
	composeExecutor dockerComposeExecutor
	stateMgr        state.Manager
	log             *slog.Logger
	notifier        notifier.Notifier
}

// NewDockerCompose creates a Orchestrator for the given environment.
func NewDockerCompose(
	env config.EnvironmentConfig,
	stateMgr state.Manager,
	notifier notifier.Notifier, log *slog.Logger) Orchestrator {
	envFiles := []string{env.ComposeEnvFilePath}
	composeExecutor := newDockerComposeExecutor(env.ComposeFilePath, envFiles, log)

	return &dockerCompose{
		env:             env,
		composeExecutor: composeExecutor,
		stateMgr:        stateMgr,
		log:             log,
		notifier:        notifier,
	}
}

// Deploy executes a full deployment for the given metadata.
// On success, it commits the new state. On failure, it attempts rollback.
//
// Deploy is idempotent: if called with the same metadata twice (e.g. after
// an agent restart mid-deployment), the deployed state will match the target
// after completion regardless of intermediate state.
func (d *dockerCompose) Deploy(ctx context.Context, meta git_provider.DeploymentMetadata) error {
	d.log.Info("starting deployment",
		"phase", logger.PhasePull,
		"digest", meta.ManifestDigest,
		"git_sha", meta.GitSHA,
	)

	// Load previous state now so we have a rollback target before we change anything.
	// This must happen before pull/restart so the previous state is preserved.
	previousState, err := d.stateMgr.LoadDeployed()
	if err != nil && !errors.Is(err, state.ErrFileNotFound) {
		d.log.Warn("could not load previous state (first deployment?)", "error", err)
	}

	noPreviousState := false
	if errors.Is(err, state.ErrFileNotFound) {
		noPreviousState = true
	}

	d.notifier.NotifyOnNewDeploymentStarted(meta)

	newState, err := d.deploy(ctx, meta)
	if err == nil {
		d.notifier.NotifyOnDeploymentSuccess(newState)
		return nil
	}

	if !errorShouldTriggerRollback(err) {
		d.notifier.NotifyOnDeploymentFailed(meta, err)
		return fmt.Errorf("deployment failed but no rollback required: %w", err)
	}

	if noPreviousState {
		d.log.Error("no previous state available for rollback — manual intervention required",
			"phase", logger.PhaseRollback)
		return fmt.Errorf("rollback failed: no previous deployment state (reason: %s)", "previous state file not found")
	}

	rollbackState, err := d.rollback(ctx, previousState, meta.ManifestDigest, err.Error())
	if err == nil {
		d.notifier.NotifyOnDeploymentFailed(meta, fmt.Errorf("ROLLBACK FAILED: %w", err))
		return fmt.Errorf("deployment and rollback failed: %w", err)
	}

	d.notifier.NotifyOnDeploymentSuccess(rollbackState)
	return nil
}

func (d *dockerCompose) deploy(ctx context.Context, meta git_provider.DeploymentMetadata) (newState state.DeploymentState, err error) {

	// --- Phase 1: Update compose env ---
	d.log.Info("updating compose image tag", "phase", logger.PhasePull, "image_tag", meta.ImageTag)
	if err := d.updateComposeImageTag(meta); err != nil {
		return newState, errors.Join(errComposeEnvFailed, err)
	}

	// --- Phase 2: Pull image ---
	d.log.Info("pulling image", "phase", logger.PhasePull, "image", meta.Image)
	if err := d.composeExecutor.PullImage(ctx, d.env.AppService); err != nil {
		return newState, errors.Join(errImagePullFailed, err)
	}

	// --- Phase 3: Run migrations ---
	if d.env.MigrationService != "" {
		d.log.Info("running migrations", "phase", logger.PhaseMigration, "service", d.env.MigrationService)
		if err = d.composeExecutor.RunMigrations(ctx, d.env.MigrationService); err != nil {
			return newState, errors.Join(errMigrationFailed, err)
		}
	}

	// --- Phase 4: Restart application ---
	d.log.Info("restarting application", "phase", logger.PhaseRestart, "service", d.env.AppService)
	if err = d.composeExecutor.RestartApp(ctx, d.env.AppService); err != nil {
		return newState, errors.Join(errRestartFailed, err)
	}

	// --- Phase 5: Health check ---
	d.log.Info("waiting for health",
		"phase", logger.PhaseHealthCheck,
		"url", d.env.HealthCheckURL,
		"retries", d.env.HealthCheck.MaxRetries,
	)
	if err = health.WaitHealthy(
		ctx,
		d.env.HealthCheckURL,
		d.env.HealthCheck.MaxRetries,
		d.env.HealthCheck.Interval,
		d.env.HealthCheck.Timeout,
	); err != nil {
		return newState, errors.Join(errHealthCheckFailed, err)
	}

	// --- Phase 6: Commit state ---
	newState = state.DeploymentState{
		Environment:    d.env.Name,
		Image:          meta.Image,
		ManifestDigest: meta.ManifestDigest,
		GitSHA:         meta.GitSHA,
		DeployedAt:     time.Now().UTC(),
	}
	if err := d.stateMgr.CommitDeployed(newState); err != nil {
		// State commit failure is non-fatal from a runtime perspective
		// (the app is running and healthy), but it means the agent will
		// re-deploy on next poll. Log loudly
		d.log.Error("failed to commit deployment state",
			"phase", logger.PhaseCommit, "error", err)
	}

	d.log.Info("deployment successful",
		"phase", logger.PhaseCommit,
		"digest", meta.ManifestDigest,
		"git_sha", meta.GitSHA,
	)

	return newState, nil
}

func (d *dockerCompose) updateComposeImageTag(meta git_provider.DeploymentMetadata) error {
	if d.env.ComposeEnvFilePath == "" {
		return fmt.Errorf("compose env file path is required")
	}
	if meta.ImageTag == "" {
		return fmt.Errorf("deployment metadata image_tag is required")
	}
	return upsertEnvFileValue(d.env.ComposeEnvFilePath, "IMAGE_TAG", meta.ImageTag)
}

func upsertEnvFileValue(path, key, value string) error {
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read env file %q: %w", path, err)
	}

	// read file content into lines
	// file content is expected to be lines of key=value text
	content := string(data)
	lines := []string{}
	if content != "" {
		lines = strings.Split(strings.TrimSuffix(content, "\n"), "\n")
	}
	replacement := key + "=" + value
	found := false

	// if key already exist with an existing value,
	// replace it with replacement
	for i, line := range lines {
		if envLineKey(line) != key {
			continue
		}
		lines[i] = replacement
		found = true
		break
	}

	// if key doesn't already exist, append replacement
	if !found {
		if content == "" {
			lines = []string{replacement}
		} else {
			lines = append(lines, replacement)
		}
	}

	updated := strings.Join(lines, "\n")
	if !strings.HasSuffix(updated, "\n") {
		updated += "\n"
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("create env file dir %q: %w", filepath.Dir(path), err)
	}

	perm := os.FileMode(0o640)
	if info, statErr := os.Stat(path); statErr == nil {
		perm = info.Mode().Perm()
	}

	if err := os.WriteFile(path, []byte(updated), perm); err != nil {
		return fmt.Errorf("failed to write env file %q: %w", path, err)
	}

	return nil
}

func envLineKey(line string) string {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return ""
	}
	trimmed = strings.TrimPrefix(trimmed, "export ")
	before, _, ok := strings.Cut(trimmed, "=")
	if !ok {
		return ""
	}
	return strings.TrimSpace(before)
}

// rollback restores the previous deployment. It is called automatically
// when a deployment fails after the restart phase.
//
// Rollback procedure:
//  1. Pull the previous image (it should already be cached locally)
//  2. Restart services with the previous image
//  3. Health check to verify rollback is healthy
//  4. Commit the rollback state
//
// If rollback itself fails, we log loudly but cannot do anything further
// automatically — manual intervention is required.
func (d *dockerCompose) rollback(ctx context.Context, previous state.DeploymentState, failedDigest, reason string) (rollbackState state.DeploymentState, err error) {
	d.log.Warn("initiating rollback",
		"phase", logger.PhaseRollback,
		"reason", reason,
		"failed_digest", failedDigest,
	)

	d.log.Info("rolling back to previous version",
		"phase", logger.PhaseRollback,
		"target_digest", previous.ManifestDigest,
		"target_git_sha", previous.GitSHA,
	)

	// Restart with previous image. The previous image should be in the local
	// Docker cache. If docker-compose.yml still references the old tag, compose
	// will use the locally cached image without pulling.
	//
	// Note: This assumes the image tag in docker-compose.yml was the mutable
	// tag (e.g. :dev-latest). For immutable digest-based deployments, the
	// compose file would need to be updated here. This is a future improvement.
	if err := d.composeExecutor.RestartApp(ctx, d.env.AppService); err != nil {
		d.log.Error("rollback restart failed — manual intervention required",
			"phase", logger.PhaseRollback, "error", err)
		return rollbackState, fmt.Errorf("rollback restart failed: %w (original reason: %s)", err, reason)
	}

	// Health check rollback.
	if err := health.WaitHealthy(
		ctx,
		d.env.HealthCheckURL,
		d.env.HealthCheck.MaxRetries,
		d.env.HealthCheck.Interval,
		d.env.HealthCheck.Timeout,
	); err != nil {
		d.log.Error("rollback health check failed — manual intervention required",
			"phase", logger.PhaseRollback, "error", err)
		return rollbackState, fmt.Errorf("rollback health check failed: %w (original reason: %s)", err, reason)
	}

	// Commit rollback state with audit trail.
	rollbackState = state.DeploymentState{
		Environment:    d.env.Name,
		Image:          previous.Image,
		ManifestDigest: previous.ManifestDigest,
		GitSHA:         previous.GitSHA,
		DeployedAt:     time.Now().UTC(),
		RollbackFrom:   failedDigest, // record what we rolled back from
	}
	if err := d.stateMgr.CommitRollback(rollbackState); err != nil {
		d.log.Error("failed to commit rollback state",
			"phase", logger.PhaseRollback, "error", err)
		// Non-fatal: the app is running. Log and continue.
	}

	d.log.Info("rollback completed successfully",
		"phase", logger.PhaseRollback,
		"restored_digest", previous.ManifestDigest,
		"failed_digest", failedDigest,
		"reason", reason,
	)

	return rollbackState, nil
}

func errorShouldTriggerRollback(err error) bool {
	for _, e := range errorsThatCanTriggerRollback {
		if errors.Is(err, e) {
			return true
		}
	}

	return false
}
