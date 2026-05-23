package notifier

import (
	"github.com/toughbred/deployment-manager/internal/git_provider"
	"github.com/toughbred/deployment-manager/internal/state"
)

// Notifier sends deployment state notifications or alert to external destinations like Slack.
//
// Since alerts are not core business logic, error encountered should be logged
type Notifier interface {
	NotifyOnNewDeploymentStarted(meta git_provider.DeploymentMetadata)
	NotifyOnDeploymentFailed(meta git_provider.DeploymentMetadata, err error)
	NotifyOnDeploymentSuccess(dep state.DeploymentState)
}
