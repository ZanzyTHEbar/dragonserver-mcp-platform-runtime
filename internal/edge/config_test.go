package edge

import (
	"testing"
	"time"

	"dragonserver/mcp-platform/internal/contracts"

	"github.com/spf13/viper"
	"github.com/stretchr/testify/require"
)

func TestConfigValidateRequiresPersistentInputsOutsideFixtureMode(t *testing.T) {
	t.Parallel()

	cfg := Config{
		PublicBaseURL:             "https://mcp.example.com",
		AuthentikIssuerURL:        "https://auth.example.com/application/o/mcp-edge/",
		AuthentikClientID:         "edge-client",
		OAuthAccessTokenTTL:       defaultOAuthAccessTokenTTL,
		OAuthRefreshTokenTTL:      defaultOAuthRefreshTokenTTL,
		OAuthAuthorizationCodeTTL: defaultOAuthAuthorizationCodeTTL,
		OAuthDeviceCodeTTL:        defaultOAuthDeviceCodeTTL,
	}

	err := cfg.Validate()
	require.ErrorContains(t, err, "platform database url is required")

	cfg.PlatformDatabaseURL = "file:data/mcp-platform.db"
	err = cfg.Validate()
	require.ErrorContains(t, err, "session encryption key path is required")

	cfg.SessionEncryptionKeyPath = "/run/secrets/mcp-edge-session-key"
	require.NoError(t, cfg.Validate())
}

func TestConfigValidateAllowsFixtureModeWithoutPersistentInputs(t *testing.T) {
	t.Parallel()

	cfg := Config{
		PublicBaseURL:             "https://mcp.example.com",
		EnableFixtureMode:         true,
		OAuthAccessTokenTTL:       defaultOAuthAccessTokenTTL,
		OAuthRefreshTokenTTL:      defaultOAuthRefreshTokenTTL,
		OAuthAuthorizationCodeTTL: defaultOAuthAuthorizationCodeTTL,
		OAuthDeviceCodeTTL:        defaultOAuthDeviceCodeTTL,
		FixtureOperatorToken:      "fixture-operator-token",
		FixtureAuthSubjectSub:     "fixture-user",
	}

	require.NoError(t, cfg.Validate())
}

func TestLoadConfigSetsDefaultOAuthLifetimes(t *testing.T) {
	resetViperForTest(t)
	t.Setenv(contracts.EnvEdgeEnableFixtureMode, "true")

	cfg, err := LoadConfig()
	require.NoError(t, err)
	require.Equal(t, defaultOAuthAccessTokenTTL, cfg.OAuthAccessTokenTTL)
	require.Equal(t, defaultOAuthRefreshTokenTTL, cfg.OAuthRefreshTokenTTL)
	require.Equal(t, defaultOAuthAuthorizationCodeTTL, cfg.OAuthAuthorizationCodeTTL)
	require.Equal(t, defaultOAuthDeviceCodeTTL, cfg.OAuthDeviceCodeTTL)
}

func TestLoadConfigParsesOAuthLifetimeOverrides(t *testing.T) {
	resetViperForTest(t)
	t.Setenv(contracts.EnvEdgeEnableFixtureMode, "true")
	t.Setenv(contracts.EnvEdgeOAuthAccessTokenTTL, "4h")
	t.Setenv(contracts.EnvEdgeOAuthRefreshTokenTTL, "7d")
	t.Setenv(contracts.EnvEdgeOAuthAuthorizationCodeTTL, "15m")
	t.Setenv(contracts.EnvEdgeOAuthDeviceCodeTTL, "20m")

	cfg, err := LoadConfig()
	require.NoError(t, err)
	require.Equal(t, 4*time.Hour, cfg.OAuthAccessTokenTTL)
	require.Equal(t, 7*24*time.Hour, cfg.OAuthRefreshTokenTTL)
	require.Equal(t, 15*time.Minute, cfg.OAuthAuthorizationCodeTTL)
	require.Equal(t, 20*time.Minute, cfg.OAuthDeviceCodeTTL)
}

func TestLoadConfigRejectsInvalidOAuthLifetime(t *testing.T) {
	resetViperForTest(t)
	t.Setenv(contracts.EnvEdgeOAuthAccessTokenTTL, "not-a-duration")

	_, err := LoadConfig()
	require.ErrorContains(t, err, contracts.EnvEdgeOAuthAccessTokenTTL)
}

func TestLoadConfigRejectsFractionalDayOAuthLifetime(t *testing.T) {
	resetViperForTest(t)
	t.Setenv(contracts.EnvEdgeOAuthRefreshTokenTTL, "1.5d")

	_, err := LoadConfig()
	require.ErrorContains(t, err, contracts.EnvEdgeOAuthRefreshTokenTTL)
}

func TestLoadConfigOAuthLifetimeOverridesStillValidateCaps(t *testing.T) {
	resetViperForTest(t)
	t.Setenv(contracts.EnvEdgeEnableFixtureMode, "true")
	t.Setenv(contracts.EnvEdgeOAuthRefreshTokenTTL, "31d")

	cfg, err := LoadConfig()
	require.NoError(t, err)
	err = cfg.Validate()
	require.ErrorContains(t, err, contracts.EnvEdgeOAuthRefreshTokenTTL)
}

func TestConfigValidateOAuthLifetimeBounds(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{
			name:    "access token ttl must be positive",
			mutate:  func(cfg *Config) { cfg.OAuthAccessTokenTTL = 0 },
			wantErr: contracts.EnvEdgeOAuthAccessTokenTTL,
		},
		{
			name:    "access token ttl is capped",
			mutate:  func(cfg *Config) { cfg.OAuthAccessTokenTTL = 24*time.Hour + time.Second },
			wantErr: contracts.EnvEdgeOAuthAccessTokenTTL,
		},
		{
			name:    "refresh token ttl is capped",
			mutate:  func(cfg *Config) { cfg.OAuthRefreshTokenTTL = 30*24*time.Hour + time.Second },
			wantErr: contracts.EnvEdgeOAuthRefreshTokenTTL,
		},
		{
			name:    "authorization code ttl is capped",
			mutate:  func(cfg *Config) { cfg.OAuthAuthorizationCodeTTL = 30*time.Minute + time.Second },
			wantErr: contracts.EnvEdgeOAuthAuthorizationCodeTTL,
		},
		{
			name:    "device code ttl is capped",
			mutate:  func(cfg *Config) { cfg.OAuthDeviceCodeTTL = 30*time.Minute + time.Second },
			wantErr: contracts.EnvEdgeOAuthDeviceCodeTTL,
		},
		{
			name: "refresh ttl cannot be shorter than access ttl",
			mutate: func(cfg *Config) {
				cfg.OAuthAccessTokenTTL = 4 * time.Hour
				cfg.OAuthRefreshTokenTTL = 2 * time.Hour
			},
			wantErr: contracts.EnvEdgeOAuthRefreshTokenTTL,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := validFixtureConfig()
			tt.mutate(&cfg)

			err := cfg.Validate()
			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}

func validFixtureConfig() Config {
	return Config{
		PublicBaseURL:             "https://mcp.example.com",
		EnableFixtureMode:         true,
		OAuthAccessTokenTTL:       defaultOAuthAccessTokenTTL,
		OAuthRefreshTokenTTL:      defaultOAuthRefreshTokenTTL,
		OAuthAuthorizationCodeTTL: defaultOAuthAuthorizationCodeTTL,
		OAuthDeviceCodeTTL:        defaultOAuthDeviceCodeTTL,
		FixtureOperatorToken:      "fixture-operator-token",
		FixtureAuthSubjectSub:     "fixture-user",
	}
}

func resetViperForTest(t *testing.T) {
	t.Helper()
	viper.Reset()
	t.Cleanup(viper.Reset)
}
