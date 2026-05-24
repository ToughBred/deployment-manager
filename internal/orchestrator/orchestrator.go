// Package orchestrator implements the deployment execution layer.
// orchestrator.go implements the deployment orchestrator.
// It coordinates the end-to-end deployment sequence:
//
//  1. Pull updated image
//  2. Run database migrations
//  3. Restart application
//  4. Health check
//  5. Commit state (or rollback on failure)
//
// This is intentionally separate from the reconciler (which detects drift)
// and the compose executor (which runs commands). The orchestrator owns the
// deployment lifecycle and rollback logic.
package orchestrator

import (
	"context"
	"errors"

	"github.com/toughbred/deployment-manager/internal/git_provider"
)

var (
	errMigrationFailed   = errors.New("error encountered during migration")
	errImagePullFailed   = errors.New("error encountered during image pull")
	errRestartFailed     = errors.New("failed to restart service")
	errHealthCheckFailed = errors.New("new deployment failed health check")
	errComposeEnvFailed  = errors.New("failed to update compose env file")
)

var errorsThatCanTriggerRollback = []error{
	errImagePullFailed, errRestartFailed, errHealthCheckFailed,
}

// Orchestrator orchestrates deployments for a single environment.
type Orchestrator interface {
	Deploy(ctx context.Context, meta git_provider.DeploymentMetadata) error
}

// RuntimeState is the reconciler's view of the currently running application.
type RuntimeState struct {
	Running        bool
	ManifestDigest string
}

// RuntimeObserver can be implemented by orchestrators that can report the
// active runtime state. Reconcilers use it to repair runtime drift when
// persisted state still matches the desired release.
type RuntimeObserver interface {
	CurrentRuntimeState(ctx context.Context) (RuntimeState, error)
}
