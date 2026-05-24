package edge

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"dragonserver/mcp-platform/internal/contracts"

	"github.com/spf13/viper"
)

const (
	defaultOAuthAccessTokenTTL       = 2 * time.Hour
	defaultOAuthRefreshTokenTTL      = 72 * time.Hour
	defaultOAuthAuthorizationCodeTTL = 10 * time.Minute
	defaultOAuthDeviceCodeTTL        = 10 * time.Minute
	maxOAuthAccessTokenTTL           = 24 * time.Hour
	maxOAuthRefreshTokenTTL          = 30 * 24 * time.Hour
	maxOAuthAuthorizationCodeTTL     = 30 * time.Minute
	maxOAuthDeviceCodeTTL            = 30 * time.Minute

	envEdgeFixtureUpstreamMealieURL       = "MCP_EDGE_FIXTURE_UPSTREAM_MEALIE_URL"
	envEdgeFixtureUpstreamActualBudgetURL = "MCP_EDGE_FIXTURE_UPSTREAM_ACTUALBUDGET_URL"
	envEdgeFixtureUpstreamMemoryURL       = "MCP_EDGE_FIXTURE_UPSTREAM_MEMORY_URL"
	envEdgeFixtureInsecureSkipVerify      = "MCP_EDGE_FIXTURE_INSECURE_SKIP_VERIFY"
	envEdgeFixtureAuthentikClientSecret   = "MCP_EDGE_FIXTURE_AUTHENTIK_CLIENT_SECRET"
	envEdgeFixtureAuthSubjectSub          = "MCP_EDGE_FIXTURE_AUTH_SUBJECT_SUB"
	envEdgeFixtureAuthSubjectEmail        = "MCP_EDGE_FIXTURE_AUTH_SUBJECT_EMAIL"
	envEdgeFixtureAuthSubjectName         = "MCP_EDGE_FIXTURE_AUTH_SUBJECT_NAME"
	envEdgeFixtureAuthPreferredUsername   = "MCP_EDGE_FIXTURE_AUTH_PREFERRED_USERNAME"
	envEdgeFixtureAuthAccountBindingID    = "MCP_EDGE_FIXTURE_AUTH_ACCOUNT_BINDING_ID"
	envEdgeFixtureAuthGroups              = "MCP_EDGE_FIXTURE_AUTH_GROUPS"
	envEdgeFixtureOperatorToken           = "MCP_EDGE_FIXTURE_OPERATOR_TOKEN"
)

type Config struct {
	PlatformEnv                    string
	LogLevel                       string
	PlatformDatabaseURL            string
	HTTPBindAddr                   string
	PublicBaseURL                  string
	EnableFixtureMode              bool
	AuthentikIssuerURL             string
	AuthentikClientID              string
	AuthentikClientSecretPath      string
	OperatorTokenPath              string
	SessionEncryptionKeyPath       string
	IdentityHeaderSecretPath       string
	AccountBindingClaim            string
	CookieSecure                   bool
	CORSAllowedOrigins             []string
	DCREnabled                     bool
	CIMDEnabled                    bool
	OAuthAccessTokenTTL            time.Duration
	OAuthRefreshTokenTTL           time.Duration
	OAuthAuthorizationCodeTTL      time.Duration
	OAuthDeviceCodeTTL             time.Duration
	FixtureUpstreamMealieURL       string
	FixtureUpstreamActualBudgetURL string
	FixtureUpstreamMemoryURL       string
	FixtureInsecureSkipVerify      bool
	FixtureAuthentikClientSecret   string
	FixtureAuthSubjectSub          string
	FixtureAuthSubjectEmail        string
	FixtureAuthSubjectName         string
	FixtureAuthPreferredUsername   string
	FixtureAuthAccountBindingID    string
	FixtureAuthGroups              []string
	FixtureOperatorToken           string
}

func LoadConfig() (Config, error) {
	viper.AutomaticEnv()
	viper.SetDefault(contracts.EnvPlatformEnv, "development")
	viper.SetDefault(contracts.EnvPlatformLogLevel, "info")
	viper.SetDefault(contracts.EnvEdgeHTTPBindAddr, ":8080")
	viper.SetDefault(contracts.EnvEdgePublicBaseURL, "http://localhost:8080")
	viper.SetDefault(contracts.EnvEdgeCookieSecure, true)
	viper.SetDefault(contracts.EnvEdgeOAuthAccessTokenTTL, defaultOAuthAccessTokenTTL.String())
	viper.SetDefault(contracts.EnvEdgeOAuthRefreshTokenTTL, defaultOAuthRefreshTokenTTL.String())
	viper.SetDefault(contracts.EnvEdgeOAuthAuthorizationCodeTTL, defaultOAuthAuthorizationCodeTTL.String())
	viper.SetDefault(contracts.EnvEdgeOAuthDeviceCodeTTL, defaultOAuthDeviceCodeTTL.String())

	accessTokenTTL, err := parseDurationConfig(contracts.EnvEdgeOAuthAccessTokenTTL)
	if err != nil {
		return Config{}, err
	}
	refreshTokenTTL, err := parseDurationConfig(contracts.EnvEdgeOAuthRefreshTokenTTL)
	if err != nil {
		return Config{}, err
	}
	authorizationCodeTTL, err := parseDurationConfig(contracts.EnvEdgeOAuthAuthorizationCodeTTL)
	if err != nil {
		return Config{}, err
	}
	deviceCodeTTL, err := parseDurationConfig(contracts.EnvEdgeOAuthDeviceCodeTTL)
	if err != nil {
		return Config{}, err
	}

	return Config{
		PlatformEnv:                    strings.TrimSpace(viper.GetString(contracts.EnvPlatformEnv)),
		LogLevel:                       strings.TrimSpace(viper.GetString(contracts.EnvPlatformLogLevel)),
		PlatformDatabaseURL:            strings.TrimSpace(viper.GetString(contracts.EnvPlatformDatabaseURL)),
		HTTPBindAddr:                   strings.TrimSpace(viper.GetString(contracts.EnvEdgeHTTPBindAddr)),
		PublicBaseURL:                  strings.TrimSpace(viper.GetString(contracts.EnvEdgePublicBaseURL)),
		EnableFixtureMode:              viper.GetBool(contracts.EnvEdgeEnableFixtureMode),
		AuthentikIssuerURL:             strings.TrimSpace(viper.GetString(contracts.EnvEdgeAuthentikIssuerURL)),
		AuthentikClientID:              strings.TrimSpace(viper.GetString(contracts.EnvEdgeAuthentikClientID)),
		AuthentikClientSecretPath:      strings.TrimSpace(viper.GetString(contracts.EnvEdgeAuthentikClientSecretPath)),
		OperatorTokenPath:              strings.TrimSpace(viper.GetString(contracts.EnvEdgeOperatorTokenPath)),
		SessionEncryptionKeyPath:       strings.TrimSpace(viper.GetString(contracts.EnvEdgeSessionEncryptionKeyPath)),
		IdentityHeaderSecretPath:       strings.TrimSpace(viper.GetString(contracts.EnvEdgeIdentityHeaderSecretPath)),
		AccountBindingClaim:            strings.TrimSpace(viper.GetString(contracts.EnvEdgeAccountBindingClaim)),
		CookieSecure:                   viper.GetBool(contracts.EnvEdgeCookieSecure),
		CORSAllowedOrigins:             splitCommaSeparated(viper.GetString(contracts.EnvEdgeCORSAllowedOrigins)),
		DCREnabled:                     viper.GetBool(contracts.EnvEdgeDCREnabled),
		CIMDEnabled:                    viper.GetBool(contracts.EnvEdgeCIMDEnabled),
		OAuthAccessTokenTTL:            accessTokenTTL,
		OAuthRefreshTokenTTL:           refreshTokenTTL,
		OAuthAuthorizationCodeTTL:      authorizationCodeTTL,
		OAuthDeviceCodeTTL:             deviceCodeTTL,
		FixtureUpstreamMealieURL:       strings.TrimSpace(viper.GetString(envEdgeFixtureUpstreamMealieURL)),
		FixtureUpstreamActualBudgetURL: strings.TrimSpace(viper.GetString(envEdgeFixtureUpstreamActualBudgetURL)),
		FixtureUpstreamMemoryURL:       strings.TrimSpace(viper.GetString(envEdgeFixtureUpstreamMemoryURL)),
		FixtureInsecureSkipVerify:      viper.GetBool(envEdgeFixtureInsecureSkipVerify),
		FixtureAuthentikClientSecret:   strings.TrimSpace(viper.GetString(envEdgeFixtureAuthentikClientSecret)),
		FixtureAuthSubjectSub:          strings.TrimSpace(viper.GetString(envEdgeFixtureAuthSubjectSub)),
		FixtureAuthSubjectEmail:        strings.TrimSpace(viper.GetString(envEdgeFixtureAuthSubjectEmail)),
		FixtureAuthSubjectName:         strings.TrimSpace(viper.GetString(envEdgeFixtureAuthSubjectName)),
		FixtureAuthPreferredUsername:   strings.TrimSpace(viper.GetString(envEdgeFixtureAuthPreferredUsername)),
		FixtureAuthAccountBindingID:    strings.TrimSpace(viper.GetString(envEdgeFixtureAuthAccountBindingID)),
		FixtureAuthGroups:              splitCommaSeparated(viper.GetString(envEdgeFixtureAuthGroups)),
		FixtureOperatorToken:           strings.TrimSpace(viper.GetString(envEdgeFixtureOperatorToken)),
	}, nil
}

func (c Config) HasOIDCConfig() bool {
	return strings.TrimSpace(c.AuthentikIssuerURL) != "" && strings.TrimSpace(c.AuthentikClientID) != ""
}

func (c Config) Validate() error {
	if strings.TrimSpace(c.PublicBaseURL) == "" {
		return fmt.Errorf("mcp-edge public base url is required")
	}
	if c.EnableFixtureMode && strings.EqualFold(strings.TrimSpace(c.PlatformEnv), "production") {
		return fmt.Errorf("fixture mode cannot be enabled in production")
	}
	if !c.HasOIDCConfig() && !c.EnableFixtureMode {
		return fmt.Errorf("mcp-edge requires Authentik OIDC configuration unless fixture mode is explicitly enabled")
	}
	if !c.EnableFixtureMode && strings.TrimSpace(c.PlatformDatabaseURL) == "" {
		return fmt.Errorf("mcp-edge platform database url is required outside fixture mode")
	}
	if !c.EnableFixtureMode && strings.TrimSpace(c.SessionEncryptionKeyPath) == "" {
		return fmt.Errorf("mcp-edge session encryption key path is required outside fixture mode")
	}
	if c.FixtureInsecureSkipVerify && !c.EnableFixtureMode {
		return fmt.Errorf("fixture insecure skip verify requires fixture mode")
	}
	if !c.EnableFixtureMode && c.usesFixtureInputs() {
		return fmt.Errorf("fixture inputs are configured but fixture mode is disabled")
	}
	if err := c.validateOAuthLifetimes(); err != nil {
		return err
	}
	return nil
}

func (c Config) validateOAuthLifetimes() error {
	checks := []struct {
		envKey string
		value  time.Duration
		max    time.Duration
	}{
		{envKey: contracts.EnvEdgeOAuthAccessTokenTTL, value: c.OAuthAccessTokenTTL, max: maxOAuthAccessTokenTTL},
		{envKey: contracts.EnvEdgeOAuthRefreshTokenTTL, value: c.OAuthRefreshTokenTTL, max: maxOAuthRefreshTokenTTL},
		{envKey: contracts.EnvEdgeOAuthAuthorizationCodeTTL, value: c.OAuthAuthorizationCodeTTL, max: maxOAuthAuthorizationCodeTTL},
		{envKey: contracts.EnvEdgeOAuthDeviceCodeTTL, value: c.OAuthDeviceCodeTTL, max: maxOAuthDeviceCodeTTL},
	}

	for _, check := range checks {
		if check.value <= 0 {
			return fmt.Errorf("%s must be greater than zero", check.envKey)
		}
		if check.value > check.max {
			return fmt.Errorf("%s must be less than or equal to %s", check.envKey, check.max)
		}
	}
	if c.OAuthRefreshTokenTTL < c.OAuthAccessTokenTTL {
		return fmt.Errorf("%s must be greater than or equal to %s", contracts.EnvEdgeOAuthRefreshTokenTTL, contracts.EnvEdgeOAuthAccessTokenTTL)
	}
	return nil
}

func parseDurationConfig(envKey string) (time.Duration, error) {
	raw := strings.TrimSpace(viper.GetString(envKey))
	value, err := time.ParseDuration(raw)
	if err == nil {
		return value, nil
	}
	if strings.HasSuffix(raw, "d") {
		days, dayErr := strconv.ParseInt(strings.TrimSuffix(raw, "d"), 10, 64)
		if dayErr == nil {
			return time.Duration(days) * 24 * time.Hour, nil
		}
	}
	return 0, fmt.Errorf("%s must be a valid duration: %w", envKey, err)
}

func (c Config) usesFixtureInputs() bool {
	return c.FixtureUpstreamMealieURL != "" ||
		c.FixtureUpstreamActualBudgetURL != "" ||
		c.FixtureUpstreamMemoryURL != "" ||
		c.FixtureInsecureSkipVerify ||
		c.FixtureAuthentikClientSecret != "" ||
		c.FixtureAuthSubjectSub != "" ||
		c.FixtureAuthSubjectEmail != "" ||
		c.FixtureAuthSubjectName != "" ||
		c.FixtureAuthPreferredUsername != "" ||
		c.FixtureAuthAccountBindingID != "" ||
		len(c.FixtureAuthGroups) > 0 ||
		c.FixtureOperatorToken != ""
}

func splitCommaSeparated(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}

	parts := strings.Split(raw, ",")
	values := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		values = append(values, part)
	}
	return values
}
