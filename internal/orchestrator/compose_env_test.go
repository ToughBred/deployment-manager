package orchestrator

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/toughbred/deployment-manager/internal/config"
	"github.com/toughbred/deployment-manager/internal/git_provider"
)

func TestUpsertEnvFileValueReplacesImageTagAndPreservesOtherLines(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "compose.env")
	require.NoError(t, os.WriteFile(path, []byte("FOO=bar\nIMAGE_TAG=prod-sha-old\n# keep me\n"), 0o600))

	require.NoError(t, upsertEnvFileValue(path, "IMAGE_TAG", "prod-sha-new"))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "FOO=bar\nIMAGE_TAG=prod-sha-new\n# keep me\n", string(data))

	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestUpsertEnvFileValueAppendsImageTagWhenMissing(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "compose.env")
	require.NoError(t, os.WriteFile(path, []byte("FOO=bar\n"), 0o640))

	require.NoError(t, upsertEnvFileValue(path, "IMAGE_TAG", "prod-sha-new"))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "FOO=bar\nIMAGE_TAG=prod-sha-new\n", string(data))
}

func TestUpsertEnvFileValueCreatesMissingFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "nested", "compose.env")

	require.NoError(t, upsertEnvFileValue(path, "IMAGE_TAG", "prod-sha-new"))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "IMAGE_TAG=prod-sha-new\n", string(data))
}

func TestDockerComposeUpdateComposeImageTagRequiresPathAndTag(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		env  config.EnvironmentConfig
		meta git_provider.DeploymentMetadata
	}{
		{
			name: "missing compose env file path",
			env:  config.EnvironmentConfig{},
			meta: git_provider.DeploymentMetadata{ImageTag: "prod-sha-new"},
		},
		{
			name: "missing image tag",
			env: config.EnvironmentConfig{
				ComposeEnvFilePath: filepath.Join(t.TempDir(), "compose.env"),
			},
			meta: git_provider.DeploymentMetadata{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			d := &dockerCompose{env: tt.env}
			require.Error(t, d.updateComposeImageTag(tt.meta))
		})
	}
}
