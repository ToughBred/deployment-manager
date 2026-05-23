package reconciler

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/toughbred/deployment-manager/internal/config"
	"github.com/toughbred/deployment-manager/internal/git_provider"
	"github.com/toughbred/deployment-manager/internal/notifier"
	"github.com/toughbred/deployment-manager/internal/orchestrator"
	"github.com/toughbred/deployment-manager/internal/state"
)

const (
	version100 = "v1.0.0"
	version110 = "v1.1.0"
)

func TestReconcilerDeploysNewVersionAndConverges(t *testing.T) {
	t.Parallel()

	stateMgr := newTestStateManager(t)
	commitState(t, stateMgr, version100)

	github := NewFakeGitHubClient(metadata(version110))
	runtime := NewFakeRuntime(stateMgr)
	runtime.SetCurrent(version100, true)
	rec := newTestReconciler(stateMgr, github, runtime, 10*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	done := startReconciler(t, ctx, rec)
	t.Cleanup(func() {
		cancel()
		requireReconcilerStopped(t, done)
	})

	waitUntil(t, time.Second, func() bool {
		return runtime.PullCallsCount() == 1 && loadDeployed(t, stateMgr).ManifestDigest == version110
	})
	waitUntil(t, time.Second, func() bool {
		return github.FetchCallsCount() >= 2
	})

	require.Equal(t, 1, runtime.PullCallsCount())
	require.Equal(t, 1, runtime.StopCallsCount())
	require.Equal(t, 1, runtime.StartCallsCount())
	require.Equal(t, version110, runtime.CurrentVersionValue())
	require.True(t, runtime.ContainerRunningValue())
	require.Equal(t, version110, loadDeployed(t, stateMgr).ManifestDigest)
}

func TestReconcilerNoDriftIsIdempotent(t *testing.T) {
	t.Parallel()

	stateMgr := newTestStateManager(t)
	commitState(t, stateMgr, version110)

	github := NewFakeGitHubClient(metadata(version110))
	runtime := NewFakeRuntime(stateMgr)
	runtime.SetCurrent(version110, true)
	rec := newTestReconciler(stateMgr, github, runtime, 10*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	done := startReconciler(t, ctx, rec)
	t.Cleanup(func() {
		cancel()
		requireReconcilerStopped(t, done)
	})

	waitUntil(t, time.Second, func() bool {
		return github.FetchCallsCount() >= 3
	})

	require.Equal(t, 0, runtime.DeployCallsCount())
	require.Equal(t, 0, runtime.PullCallsCount())
	require.Equal(t, 0, runtime.StopCallsCount())
	require.Equal(t, 0, runtime.StartCallsCount())
	require.Equal(t, version110, loadDeployed(t, stateMgr).ManifestDigest)
}

func TestReconcilerCorrectsRuntimeDriftWhenPersistedStateMatchesDesired(t *testing.T) {
	t.Parallel()

	stateMgr := newTestStateManager(t)
	commitState(t, stateMgr, version110)

	github := NewFakeGitHubClient(metadata(version110))
	runtime := NewFakeRuntime(stateMgr)
	runtime.SetCurrent(version110, false)
	rec := newTestReconciler(stateMgr, github, runtime, 10*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	done := startReconciler(t, ctx, rec)
	t.Cleanup(func() {
		cancel()
		requireReconcilerStopped(t, done)
	})

	waitUntil(t, time.Second, func() bool {
		return runtime.ContainerRunningValue() && runtime.StartCallsCount() == 1
	})
	waitUntil(t, time.Second, func() bool {
		return github.FetchCallsCount() >= 2
	})

	require.Equal(t, 1, runtime.PullCallsCount())
	require.Equal(t, 1, runtime.StopCallsCount())
	require.Equal(t, 1, runtime.StartCallsCount())
	require.Equal(t, version110, runtime.CurrentVersionValue())
	require.Equal(t, version110, loadDeployed(t, stateMgr).ManifestDigest)
}

func TestReconcilerRetriesAfterGitHubFailure(t *testing.T) {
	t.Parallel()

	stateMgr := newTestStateManager(t)
	commitState(t, stateMgr, version100)

	github := NewFakeGitHubClient(metadata(version110))
	github.FailNextFetches(2, errors.New("temporary github failure"))
	unblockSuccess := github.BlockNextSuccess()

	runtime := NewFakeRuntime(stateMgr)
	runtime.SetCurrent(version100, true)
	rec := newTestReconciler(stateMgr, github, runtime, 10*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	done := startReconciler(t, ctx, rec)
	t.Cleanup(func() {
		cancel()
		requireReconcilerStopped(t, done)
	})

	waitUntil(t, time.Second, func() bool {
		return github.FetchCallsCount() == 3
	})

	require.Equal(t, version100, loadDeployed(t, stateMgr).ManifestDigest)
	require.Equal(t, 0, runtime.DeployCallsCount())

	close(unblockSuccess)
	waitUntil(t, time.Second, func() bool {
		return runtime.DeployCallsCount() == 1 && loadDeployed(t, stateMgr).ManifestDigest == version110
	})

	require.Equal(t, version110, runtime.CurrentVersionValue())
	require.Equal(t, 1, runtime.PullCallsCount())
}

func TestReconcilerRetriesAfterRuntimeFailureWithoutAdvancingState(t *testing.T) {
	t.Parallel()

	stateMgr := newTestStateManager(t)
	commitState(t, stateMgr, version100)

	github := NewFakeGitHubClient(metadata(version110))
	runtime := NewFakeRuntime(stateMgr)
	runtime.SetCurrent(version100, true)
	runtime.FailNextPull = true
	rec := newTestReconciler(stateMgr, github, runtime, 10*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	done := startReconciler(t, ctx, rec)
	t.Cleanup(func() {
		cancel()
		requireReconcilerStopped(t, done)
	})

	waitUntil(t, time.Second, func() bool {
		return runtime.PullFailuresCount() == 1
	})

	require.Equal(t, version100, loadDeployed(t, stateMgr).ManifestDigest)
	require.Equal(t, version100, runtime.CurrentVersionValue())
	require.Equal(t, 0, runtime.StopCallsCount())
	require.Equal(t, 0, runtime.StartCallsCount())

	waitUntil(t, time.Second, func() bool {
		return runtime.DeployCallsCount() == 2 && loadDeployed(t, stateMgr).ManifestDigest == version110
	})

	require.Equal(t, 2, runtime.PullCallsCount())
	require.Equal(t, 1, runtime.StopCallsCount())
	require.Equal(t, 1, runtime.StartCallsCount())
	require.Equal(t, version110, runtime.CurrentVersionValue())
}

func TestReconcilerExitsOnContextCancellation(t *testing.T) {
	t.Parallel()

	stateMgr := newTestStateManager(t)
	commitState(t, stateMgr, version110)

	github := NewFakeGitHubClient(metadata(version110))
	runtime := NewFakeRuntime(stateMgr)
	runtime.SetCurrent(version110, true)
	rec := newTestReconciler(stateMgr, github, runtime, time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	done := startReconciler(t, ctx, rec)

	waitUntil(t, time.Second, func() bool {
		return github.FetchCallsCount() >= 1
	})
	cancel()

	requireReconcilerStopped(t, done)
	require.Equal(t, 0, runtime.DeployCallsCount())
}

func TestReconcilerRestoresPersistedStateAfterRestart(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	stateMgr, err := state.NewManagerWithLocalFileSystem(stateDir)
	require.NoError(t, err)
	commitState(t, stateMgr, version110)

	restartedStateMgr, err := state.NewManagerWithLocalFileSystem(stateDir)
	require.NoError(t, err)

	github := NewFakeGitHubClient(metadata(version110))
	runtime := NewFakeRuntime(restartedStateMgr)
	runtime.SetCurrent(version110, true)
	rec := newTestReconciler(restartedStateMgr, github, runtime, 10*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	done := startReconciler(t, ctx, rec)
	t.Cleanup(func() {
		cancel()
		requireReconcilerStopped(t, done)
	})

	waitUntil(t, time.Second, func() bool {
		return github.FetchCallsCount() >= 3
	})

	require.Equal(t, version110, loadDeployed(t, restartedStateMgr).ManifestDigest)
	require.Equal(t, 0, runtime.DeployCallsCount())
}

func newTestReconciler(
	stateMgr state.Manager,
	github git_provider.GitProvider,
	runtime orchestrator.Orchestrator,
	pollInterval time.Duration,
) *Reconciler {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return &Reconciler{
		env: config.EnvironmentConfig{
			Name:          "dev",
			ReleasePrefix: "dev-",
			AppService:    "app",
		},
		pollInterval:       pollInterval,
		gitProvider:        github,
		deployOrchestrator: runtime,
		stateMgr:           stateMgr,
		log:                log,
	}
}

func startReconciler(t *testing.T, ctx context.Context, rec *Reconciler) <-chan struct{} {
	t.Helper()

	done := make(chan struct{})
	go func() {
		defer close(done)
		rec.StartPeriodicReconciliation(ctx)
	}()
	return done
}

func requireReconcilerStopped(t *testing.T, done <-chan struct{}) {
	t.Helper()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("reconciler did not stop after context cancellation")
	}
}

func newTestStateManager(t *testing.T) state.Manager {
	t.Helper()

	stateMgr, err := state.NewManagerWithLocalFileSystem(t.TempDir())
	require.NoError(t, err)
	return stateMgr
}

func metadata(version string) git_provider.DeploymentMetadata {
	return git_provider.DeploymentMetadata{
		Environment:    "dev",
		Image:          "ghcr.io/example/app:" + version,
		ImageTag:       version,
		ManifestDigest: version,
		GitSHA:         "sha-" + version,
		CreatedAt:      time.Now().UTC(),
	}
}

func commitState(t *testing.T, stateMgr state.Manager, version string) {
	t.Helper()

	require.NoError(t, stateMgr.CommitDeployed(state.DeploymentState{
		Environment:    "dev",
		Image:          "ghcr.io/example/app:" + version,
		ManifestDigest: version,
		GitSHA:         "sha-" + version,
		DeployedAt:     time.Now().UTC(),
	}))
}

func loadDeployed(t *testing.T, stateMgr state.Manager) state.DeploymentState {
	t.Helper()

	deployed, err := stateMgr.LoadDeployed()
	require.NoError(t, err)
	return deployed
}

func waitUntil(t *testing.T, timeout time.Duration, fn func() bool) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	require.True(t, fn(), "condition was not met within %s", timeout)
}

func eventuallyNoError(t *testing.T, timeout time.Duration, fn func() error) {
	t.Helper()

	waitUntil(t, timeout, func() bool {
		return fn() == nil
	})
	require.NoError(t, fn())
}

type FakeGitHubClient struct {
	mu             sync.Mutex
	meta           git_provider.DeploymentMetadata
	fetchCalls     int
	failures       []error
	blockedSuccess chan struct{}
}

func NewFakeGitHubClient(meta git_provider.DeploymentMetadata) *FakeGitHubClient {
	return &FakeGitHubClient{meta: meta}
}

func (f *FakeGitHubClient) FetchLatestForEnvironment(ctx context.Context, releasePrefix, environment string) (git_provider.DeploymentMetadata, error) {
	f.mu.Lock()
	f.fetchCalls++
	if len(f.failures) > 0 {
		err := f.failures[0]
		f.failures = f.failures[1:]
		f.mu.Unlock()
		return git_provider.DeploymentMetadata{}, err
	}
	blockedSuccess := f.blockedSuccess
	f.blockedSuccess = nil
	meta := f.meta
	f.mu.Unlock()

	if blockedSuccess != nil {
		select {
		case <-blockedSuccess:
		case <-ctx.Done():
			return git_provider.DeploymentMetadata{}, ctx.Err()
		}
	}

	if meta.Environment != environment {
		return git_provider.DeploymentMetadata{}, git_provider.ErrNoDeploymentMetaFound
	}
	return meta, nil
}

func (f *FakeGitHubClient) FailNextFetches(count int, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	for range count {
		f.failures = append(f.failures, err)
	}
}

func (f *FakeGitHubClient) BlockNextSuccess() chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.blockedSuccess = make(chan struct{})
	return f.blockedSuccess
}

func (f *FakeGitHubClient) FetchCallsCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fetchCalls
}

type FakeNotifier struct {
	mu             sync.Mutex
	StartedCalls   int
	FailedCalls    int
	SuccessCalls   int
	LastFailureErr error
}

func (f *FakeNotifier) NotifyOnNewDeploymentStarted(meta git_provider.DeploymentMetadata) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.StartedCalls++
}

func (f *FakeNotifier) NotifyOnDeploymentFailed(meta git_provider.DeploymentMetadata, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.FailedCalls++
	f.LastFailureErr = err
}

func (f *FakeNotifier) NotifyOnDeploymentSuccess(dep state.DeploymentState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.SuccessCalls++
}

var _ notifier.Notifier = (*FakeNotifier)(nil)

type FakeRuntime struct {
	mu sync.Mutex

	stateMgr state.Manager

	FailNextPull bool

	DeployCalls      int
	PullCalls        int
	StopCalls        int
	StartCalls       int
	PullFailures     int
	CurrentVersion   string
	ContainerRunning bool
}

func NewFakeRuntime(stateMgr state.Manager) *FakeRuntime {
	return &FakeRuntime{stateMgr: stateMgr}
}

func (f *FakeRuntime) Deploy(ctx context.Context, meta git_provider.DeploymentMetadata) error {
	f.mu.Lock()
	f.DeployCalls++
	f.PullCalls++
	if f.FailNextPull {
		f.FailNextPull = false
		f.PullFailures++
		f.mu.Unlock()
		return errors.New("pull image failed")
	}

	f.StopCalls++
	f.ContainerRunning = false
	f.StartCalls++
	f.CurrentVersion = meta.ManifestDigest
	f.ContainerRunning = true
	f.mu.Unlock()

	return f.stateMgr.CommitDeployed(state.DeploymentState{
		Environment:    meta.Environment,
		Image:          meta.Image,
		ManifestDigest: meta.ManifestDigest,
		GitSHA:         meta.GitSHA,
		DeployedAt:     time.Now().UTC(),
	})
}

func (f *FakeRuntime) CurrentRuntimeState(ctx context.Context) (orchestrator.RuntimeState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return orchestrator.RuntimeState{
		Running:        f.ContainerRunning,
		ManifestDigest: f.CurrentVersion,
	}, nil
}

func (f *FakeRuntime) SetCurrent(version string, running bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.CurrentVersion = version
	f.ContainerRunning = running
}

func (f *FakeRuntime) DeployCallsCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.DeployCalls
}

func (f *FakeRuntime) PullCallsCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.PullCalls
}

func (f *FakeRuntime) StopCallsCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.StopCalls
}

func (f *FakeRuntime) StartCallsCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.StartCalls
}

func (f *FakeRuntime) PullFailuresCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.PullFailures
}

func (f *FakeRuntime) CurrentVersionValue() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.CurrentVersion
}

func (f *FakeRuntime) ContainerRunningValue() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ContainerRunning
}

var _ orchestrator.Orchestrator = (*FakeRuntime)(nil)
var _ orchestrator.RuntimeObserver = (*FakeRuntime)(nil)
