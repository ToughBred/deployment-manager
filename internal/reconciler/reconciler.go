// Package reconciler implements the core control loop for a single environment.
//
// The reconciler is the heart of the deployment agent. It continuously:
//  1. Polls GitHub Releases for the desired state of the environment
//  2. Compares the desired state (remote manifest digest) against the actual
//     state (locally persisted deployed digest)
//  3. If they differ (drift detected), delegates to the Orchestrator to converge
//  4. Sleeps for poll_interval and repeats
//
// Each environment runs its own independent reconciler goroutine.
// A failure in the dev reconciler cannot affect the production reconciler.
//
// Design: Reconcile-then-sleep (not ticker) so a slow deployment doesn't
// cause reconcile loops to overlap.
package reconciler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/toughbred/deployment-manager/internal/config"
	"github.com/toughbred/deployment-manager/internal/git_provider"
	"github.com/toughbred/deployment-manager/internal/logger"
	"github.com/toughbred/deployment-manager/internal/notifier"
	"github.com/toughbred/deployment-manager/internal/orchestrator"
	"github.com/toughbred/deployment-manager/internal/state"
)

// Reconciler manages the continuous reconciliation loop for one environment.
type Reconciler struct {
	env                config.EnvironmentConfig
	pollInterval       time.Duration
	gitProvider        git_provider.GitProvider
	deployOrchestrator orchestrator.Orchestrator
	stateMgr           state.Manager
	log                *slog.Logger
}

// New creates a Reconciler for one environment.
func New(
	env config.EnvironmentConfig,
	pollInterval time.Duration,
	gitProvider git_provider.GitProvider,
	stateMgr state.Manager,
	alertManager notifier.Notifier,
	log *slog.Logger,
) *Reconciler {
	envLog := logger.WithEnvironment(log, env.Name)
	d := orchestrator.NewDockerCompose(env, stateMgr, alertManager, envLog)

	return &Reconciler{
		env:                env,
		pollInterval:       pollInterval,
		gitProvider:        gitProvider,
		deployOrchestrator: d,
		stateMgr:           stateMgr,
		log:                envLog,
	}
}

// StartPeriodicReconciliation starts the reconciliation loop and blocks until ctx is cancelled.
// It is safe to run multiple Reconcilers concurrently in separate goroutines.
func (r *Reconciler) StartPeriodicReconciliation(ctx context.Context) {
	r.log.Info("reconciler started", "poll_interval", r.pollInterval)

	// Run an immediate reconcile on startup rather than waiting for the first
	// tick. This means deployments that happened while the agent was down are
	// applied within seconds of agent start.
	r.reconcileOnce(ctx)

	for {
		select {
		case <-ctx.Done():
			r.log.Info("reconciler shutting down", "reason", ctx.Err())
			return
		case <-time.After(r.pollInterval):
			r.reconcileOnce(ctx)
		}
	}
}

// reconcileOnce runs a single poll-compare-converge cycle.
// It is the unit of work: safe to call repeatedly, never panics.
func (r *Reconciler) reconcileOnce(ctx context.Context) {
	start := time.Now()

	r.log.Debug("reconcile cycle started", "phase", logger.PhasePoll)

	// --- Step 1: Poll desired state from GitHub Releases ---
	desiredState, err := r.fetchDesiredState(ctx)
	if err != nil {
		if errors.Is(err, git_provider.ErrNoDeploymentMetaFound) {
			r.log.Info("no release found for environment",
				"phase", logger.PhasePoll,
				"release_prefix", r.env.ReleasePrefix,
			)
			return
		}
		r.log.Error("failed to fetch desired state",
			"phase", logger.PhasePoll,
			"error", err,
			"duration_ms", time.Since(start).Milliseconds(),
		)
		// Transient failure — will retry on next poll.
		return
	}

	r.log.Debug("desired state fetched",
		"phase", logger.PhasePoll,
		"digest", desiredState.ManifestDigest,
		"git_sha", desiredState.GitSHA,
	)

	// --- Step 2: Load currentState (deployed) state ---
	currentState, err := r.stateMgr.LoadDeployed()
	// if fileNotFound error, that usually means no previous deployed state and this is likely the first run,
	// drift in that case
	if err != nil && !errors.Is(err, state.ErrFileNotFound) {
		r.log.Error("failed to load deployed state",
			"phase", logger.PhaseDrift,
			"error", err,
		)
		return
	}

	// --- Step 3: Drift detection ---
	if !r.hasDrift(currentState, desiredState) {
		r.log.Debug("no drift detected",
			"phase", logger.PhaseNoop,
			"digest", desiredState.ManifestDigest,
			"duration_ms", time.Since(start).Milliseconds(),
		)
		return
	}

	r.log.Info("drift detected — deployment required",
		"phase", logger.PhaseDrift,
		"deployed_digest", deployedDigest(currentState),
		"desired_digest", desiredState.ManifestDigest,
		"desired_git_sha", desiredState.GitSHA,
	)

	// --- Step 4: Deploy ---
	if err = r.deployOrchestrator.Deploy(ctx, desiredState); err != nil {
		r.log.Error("deployment failed",
			"error", err,
			"duration_ms", time.Since(start).Milliseconds(),
		)
		// Error is already logged with full detail by the orchestrator.
		// Next reconcile cycle will retry (or find the drift resolved
		// if a rollback was successful).
		return
	}

	r.log.Info("reconcile cycle completed successfully",
		"digest", desiredState.ManifestDigest,
		"duration_ms", time.Since(start).Milliseconds(),
	)
}

// fetchDesiredState retrieves the latest deployment metadata for this environment.
func (r *Reconciler) fetchDesiredState(ctx context.Context) (meta git_provider.DeploymentMetadata, err error) {
	meta, err = r.gitProvider.FetchLatestForEnvironment(ctx, r.env.ReleasePrefix, r.env.Name)
	if err != nil {
		return meta, fmt.Errorf("failed to fetch metadata: %w", err)
	}
	return meta, nil
}

// hasDrift reports whether the deployed state differs from the desired state.
//
// Drift is detected by comparing manifest digests. Image tags are mutable
// and cannot be trusted for this purpose — the same tag (e.g. :dev-latest)
// can point to different images over time.
//
// If actual is nil (no state persisted), drift always exists (first deploy).
func (r *Reconciler) hasDrift(actual state.DeploymentState, desired git_provider.DeploymentMetadata) bool {
	return actual.ManifestDigest != desired.ManifestDigest
}

// deployedDigest safely returns the digest of the current state.
// Returns "<none>" if no state exists (for structured log fields).
func deployedDigest(s state.DeploymentState) string {
	if s.ManifestDigest == "" {
		return "<none>"
	}
	return s.ManifestDigest
}
