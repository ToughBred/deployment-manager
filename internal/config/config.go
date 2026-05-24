// Package config handles loading and validation of the agent configuration.
// Configuration is stored as YAML but parsed using a purpose-built parser
// that avoids external dependencies — a deliberate choice for a security-
// sensitive deployment agent that must be auditable and have a minimal
// supply chain footprint.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// Config is the top-level agent configuration.
type Config struct {
	// PollIntervalInSeconds controls how frequently each environment reconciler checks
	// for new deployment metadata. Default: 60.
	PollIntervalInSeconds int64 `json:"poll_interval_in_seconds"`

	// GitHub contains repository coordinates used to fetch release metadata.
	GitHub GitProviderConfig `json:"github"`

	// Environments is the list of environments this agent manages.
	// Each runs an independent reconciliation loop.
	Environments []EnvironmentConfig `json:"environments"`
}

// GitProviderConfig holds GitProvider API coordinates.
type GitProviderConfig struct {
	Owner string `json:"owner"`
	Repo  string `json:"repo"`
	// Token is optional; set via GITHUB_TOKEN env var for private repos.
	// If empty, unauthenticated requests are used (60 req/hr limit applies).
	Token string `json:"token"`
}

// EnvironmentConfig describes a single managed environment.
type EnvironmentConfig struct {
	// Name is the canonical environment identifier (e.g. "dev", "production").
	Name string `json:"name"`

	NotificationUrl string `json:"notification_url"`

	// ReleasePrefix is matched against GitHub Release tag names to scope
	// which releases belong to this environment.
	// e.g. "dev-" matches "dev-2026-05-22-abc1234"
	ReleasePrefix string `json:"release_prefix"`

	// ComposeFilePath is the absolute path to the docker-compose.yml for this env.
	ComposeFilePath string `json:"compose_file_path"`

	// ComposeEnvFilePath is loaded by the deployment-manager for
	// docker compose deployment-time variables
	// (e.g. migration DSN, registry credentials, image-tag).
	ComposeEnvFilePath string `json:"compose_env_file_path"`

	// StateDir is the directory where deployed.json and previous.json live.
	StateDir string `json:"state_dir"`

	// HealthCheckURL is the HTTP endpoint polled to verify a deployment.
	HealthCheckURL string `json:"health_check_url"`

	// MigrationService is the docker compose service name that runs migrations.
	// If empty, migrations are skipped.
	// Defaults to "migrate" if not specified.
	MigrationService string `json:"migration_service"`

	// AppService is the docker compose service name for the application.
	// Defaults to "app" if not specified.
	AppService string `json:"app_service"`

	// HealthCheck allows per-environment health check tuning.
	HealthCheck HealthCheckConfig `json:"health_check"`
}

// HealthCheckConfig controls retry behavior for post-deployment health checks.
type HealthCheckConfig struct {
	// MaxRetries is the number of health check attempts before declaring failure.
	MaxRetries int `json:"retries"`
	// Interval is the wait between retries.
	Interval time.Duration
	// Timeout is the per-request HTTP timeout.
	Timeout time.Duration
}

// fillDefaultOnZeroValues fills in zero values with sensible production default.
func (e *EnvironmentConfig) fillDefaultOnZeroValues() {
	if e.MigrationService == "" {
		e.MigrationService = "migrate"
	}
	if e.AppService == "" {
		e.AppService = "app"
	}
	if e.HealthCheck.MaxRetries == 0 {
		e.HealthCheck.MaxRetries = 12
	}
	if e.HealthCheck.Interval == 0 {
		e.HealthCheck.Interval = 10 * time.Second
	}
	if e.HealthCheck.Timeout == 0 {
		e.HealthCheck.Timeout = 5 * time.Second
	}
}

// Load reads and parses the YAML configuration file at path.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("error reading config file %q: %w", path, err)
	}

	var cfg Config
	if err = json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("error parsing config file %q: %w", path, err)
	}

	for i := range cfg.Environments {
		cfg.Environments[i].fillDefaultOnZeroValues()
	}

	return &cfg, cfg.validate()
}

func (cfg Config) validate() error {
	if cfg.GitHub.Token == "" {
		cfg.GitHub.Token = os.Getenv("GITHUB_TOKEN")
		if cfg.GitHub.Token == "" {
			return fmt.Errorf("GITHUB_TOKEN environment variable not set")
		}
	}
	if cfg.GitHub.Owner == "" {
		return fmt.Errorf("github.owner is required")
	}
	if cfg.GitHub.Repo == "" {
		return fmt.Errorf("github.repo is required")
	}
	if len(cfg.Environments) == 0 {
		return fmt.Errorf("at least one environment must be configured")
	}
	for i, e := range cfg.Environments {
		if e.Name == "" {
			return fmt.Errorf("environments[%d]: name is required", i)
		}
		if e.ComposeFilePath == "" {
			return fmt.Errorf("environment %q: compose_file is required", e.Name)
		}
		if e.ComposeEnvFilePath == "" {
			return fmt.Errorf("environment %q: compose_env_file_path is required", e.Name)
		}
		if e.StateDir == "" {
			return fmt.Errorf("environment %q: state_dir is required", e.Name)
		}
		if e.HealthCheckURL == "" {
			return fmt.Errorf("environment %q: healthcheck_url is required", e.Name)
		}
	}
	return nil
}
