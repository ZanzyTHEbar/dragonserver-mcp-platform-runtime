package edge

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"dragonserver/mcp-platform/internal/catalog"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

func TestCatalogSnapshotMatchesExactAndNestedPublicPaths(t *testing.T) {
	t.Parallel()

	snapshot, err := newCatalogSnapshot([]catalog.ServiceCatalogEntry{
		{ServiceID: "example-c", PublicPath: "/example-c/mcp"},
		{ServiceID: "example-a", PublicPath: "/example-a/mcp"},
	}, time.Now().UTC())
	require.NoError(t, err)

	service, ok := snapshot.MatchPublicPath("/example-c/mcp")
	require.True(t, ok)
	require.Equal(t, "example-c", service.ServiceID)

	service, ok = snapshot.MatchPublicPath("/example-c/mcp/tools/list")
	require.True(t, ok)
	require.Equal(t, "example-c", service.ServiceID)

	_, ok = snapshot.MatchPublicPath("/example-c/mcp2")
	require.False(t, ok)
}

func TestCatalogCacheRefreshKeepsLastGoodSnapshotOnError(t *testing.T) {
	t.Parallel()

	store := newMutableCatalogStore(t)
	cache := NewCatalogCache(store, zerolog.Nop())
	require.NoError(t, cache.Refresh(context.Background()))
	require.Equal(t, len(testServiceCatalogEntries()), cache.Len())

	store.err = errors.New("database unavailable")
	require.Error(t, cache.Refresh(context.Background()))
	require.Equal(t, len(testServiceCatalogEntries()), cache.Len())
	require.Equal(t, "database unavailable", cache.LastError())
}

func TestCatalogSnapshotRejectsReservedPublicPath(t *testing.T) {
	t.Parallel()

	_, err := newCatalogSnapshot([]catalog.ServiceCatalogEntry{{ServiceID: "bad", PublicPath: "/oauth/register"}}, time.Now().UTC())
	require.ErrorContains(t, err, "conflicts with a reserved edge route")
}

func TestCatalogSnapshotRejectsInvalidServiceID(t *testing.T) {
	t.Parallel()

	_, err := newCatalogSnapshot([]catalog.ServiceCatalogEntry{{ServiceID: "bad service/id", PublicPath: "/bad/mcp"}}, time.Now().UTC())
	require.ErrorContains(t, err, "invalid service_id")
}

func TestServerHandlerUsesRefreshedCatalogWithoutRebuild(t *testing.T) {
	t.Parallel()

	store := newMutableCatalogStore(t)
	server, err := NewServerWithStateStore(context.Background(), testEdgeConfig(), zerolog.Nop(), staticResolver{}, store)
	require.NoError(t, err)
	handler := server.Handler()

	req := httptest.NewRequest(http.MethodGet, "/newservice/mcp", nil)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	require.Equal(t, http.StatusNotFound, res.Code)

	newService := testServiceCatalogEntries()[0]
	newService.ServiceID = "newservice"
	newService.PublicPath = "/newservice/mcp"
	store.entries = append(store.entries, newService)
	require.NoError(t, server.catalogCache.Refresh(context.Background()))

	req = httptest.NewRequest(http.MethodGet, "/newservice/mcp", nil)
	res = httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	require.Equal(t, http.StatusUnauthorized, res.Code)
	require.Contains(t, res.Body.String(), "invalid_token")
	require.Contains(t, res.Header().Get("WWW-Authenticate"), `error="invalid_token"`)
	require.Contains(t, res.Header().Get("WWW-Authenticate"), `scope="mcp:newservice"`)
	require.Contains(t, res.Header().Get("WWW-Authenticate"), `resource_metadata="https://mcp.example.com/.well-known/oauth-protected-resource/newservice"`)

	store.entries = testServiceCatalogEntries()
	require.NoError(t, server.catalogCache.Refresh(context.Background()))
	req = httptest.NewRequest(http.MethodGet, "/newservice/mcp", nil)
	res = httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	require.Equal(t, http.StatusNotFound, res.Code)
}

func TestRootDiscoveryReturnsCatalogAndMetadataLinks(t *testing.T) {
	t.Parallel()

	server := newTestEdgeServer(t, nil)
	handler := server.Handler()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	require.Equal(t, http.StatusOK, res.Code)

	var payload map[string]any
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &payload))
	require.Equal(t, "mcp-edge", payload["name"])
	require.Equal(t, "https://mcp.example.com", payload["public_base_url"])
	require.Equal(t, "ok", payload["catalog_status"])

	services, ok := payload["services"].([]any)
	require.True(t, ok)
	require.Len(t, services, len(testServiceCatalogEntries()))
	require.Contains(t, res.Body.String(), `"id":"example-a"`)
	require.Contains(t, res.Body.String(), `"path":"/example-a/mcp"`)
	require.Contains(t, res.Body.String(), `"url":"https://mcp.example.com/example-a/mcp"`)
	require.Contains(t, res.Body.String(), `"resource":"https://mcp.example.com/example-a/mcp"`)
	require.Contains(t, res.Body.String(), `"protected_resource_metadata_url":"https://mcp.example.com/.well-known/oauth-protected-resource/example-a"`)
	require.Contains(t, res.Body.String(), `"scope":"mcp:example-a"`)
	require.Contains(t, res.Body.String(), `"id":"example-b"`)
	require.Contains(t, res.Body.String(), `"path":"/example-b/mcp"`)
	require.Contains(t, res.Body.String(), `"url":"https://mcp.example.com/example-b/mcp"`)
	require.Contains(t, res.Body.String(), `"resource":"https://mcp.example.com/example-b/mcp"`)
	require.Contains(t, res.Body.String(), `"protected_resource_metadata_url":"https://mcp.example.com/.well-known/oauth-protected-resource/example-b"`)
	require.Contains(t, res.Body.String(), `"scope":"mcp:example-b"`)

	oauth, ok := payload["oauth"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "https://mcp.example.com/.well-known/oauth-authorization-server", oauth["authorization_server_metadata"])
	require.Equal(t, "https://mcp.example.com/oauth/register", oauth["registration_endpoint"])
}

func TestCanonicalServiceMetadataAcrossDiscoveryAndWellKnown(t *testing.T) {
	t.Parallel()

	server := newTestEdgeServer(t, nil)
	handler := server.Handler()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	require.Equal(t, http.StatusOK, res.Code)

	var discovery struct {
		Services []struct {
			ID                           string `json:"id"`
			DisplayName                  string `json:"display_name"`
			Path                         string `json:"path"`
			URL                          string `json:"url"`
			Resource                     string `json:"resource"`
			ProtectedResourceMetadataURL string `json:"protected_resource_metadata_url"`
			Scope                        string `json:"scope"`
		} `json:"services"`
	}
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &discovery))
	require.Len(t, discovery.Services, len(testServiceCatalogEntries()))

	for _, entry := range testServiceCatalogEntries() {
		resolution, ok := server.serviceDirectory.ResolveByID(entry.ServiceID)
		require.True(t, ok)

		var discovered struct {
			ID                           string `json:"id"`
			DisplayName                  string `json:"display_name"`
			Path                         string `json:"path"`
			URL                          string `json:"url"`
			Resource                     string `json:"resource"`
			ProtectedResourceMetadataURL string `json:"protected_resource_metadata_url"`
			Scope                        string `json:"scope"`
		}
		found := false
		for _, service := range discovery.Services {
			if service.ID == entry.ServiceID {
				discovered = service
				found = true
				break
			}
		}
		require.True(t, found, "missing discovery entry for %s", entry.ServiceID)
		require.Equal(t, resolution.ServiceID, discovered.ID)
		require.Equal(t, resolution.Service.DisplayName, discovered.DisplayName)
		require.Equal(t, resolution.Service.PublicPath, discovered.Path)
		require.Equal(t, resolution.PublicURL, discovered.URL)
		require.Equal(t, resolution.Resource, discovered.Resource)
		require.Equal(t, resolution.ProtectedResourceMetadataURL, discovered.ProtectedResourceMetadataURL)
		require.Equal(t, resolution.Scope, discovered.Scope)

		assertProtectedResourceMetadata(t, handler, "/.well-known/oauth-protected-resource/"+resolution.ServiceID, resolution)
		assertProtectedResourceMetadata(t, handler, "/.well-known/oauth-protected-resource"+resolution.Service.PublicPath, resolution)
		assertAuthorizationServerMetadata(t, handler, "/.well-known/oauth-authorization-server/"+resolution.ServiceID, resolution)
		assertAuthorizationServerMetadata(t, handler, "/.well-known/oauth-authorization-server"+resolution.Service.PublicPath, resolution)
	}
}

func TestBearerChallengeUsesCanonicalServiceMetadata(t *testing.T) {
	t.Parallel()

	server := newTestEdgeServer(t, nil)
	handler := server.Handler()

	for _, entry := range testServiceCatalogEntries() {
		resolution, ok := server.serviceDirectory.ResolveByID(entry.ServiceID)
		require.True(t, ok)

		req := httptest.NewRequest(http.MethodGet, resolution.Service.PublicPath, nil)
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)

		require.Equal(t, http.StatusUnauthorized, res.Code)
		challenge := res.Header().Get("WWW-Authenticate")
		require.Contains(t, challenge, `Bearer realm="mcp-edge"`)
		require.Contains(t, challenge, `error="invalid_token"`)
		require.Contains(t, challenge, `scope="`+resolution.Scope+`"`)
		require.Contains(t, challenge, `resource_metadata="`+resolution.ProtectedResourceMetadataURL+`"`)
	}
}

func TestRootDiscoveryDoesNotExposeCatalogRefreshErrors(t *testing.T) {
	t.Parallel()

	store := newMutableCatalogStore(t)
	server, err := NewServerWithStateStore(context.Background(), testEdgeConfig(), zerolog.Nop(), staticResolver{}, store)
	require.NoError(t, err)

	store.err = errors.New("sqlite failed at /data/private/mcp-platform.db")
	require.Error(t, server.catalogCache.Refresh(context.Background()))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	res := httptest.NewRecorder()
	server.Handler().ServeHTTP(res, req)

	require.Equal(t, http.StatusOK, res.Code)
	require.Contains(t, res.Body.String(), `"catalog_status":"degraded"`)
	require.NotContains(t, res.Body.String(), "sqlite failed")
	require.NotContains(t, res.Body.String(), "/data/private")
}

func assertProtectedResourceMetadata(t *testing.T, handler http.Handler, path string, resolution ServiceResolution) {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, path, nil)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	require.Equal(t, http.StatusOK, res.Code)

	var payload struct {
		Resource             string   `json:"resource"`
		ResourceName         string   `json:"resource_name"`
		AuthorizationServers []string `json:"authorization_servers"`
		ScopesSupported      []string `json:"scopes_supported"`
	}
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &payload))
	require.Equal(t, resolution.Resource, payload.Resource)
	require.Equal(t, resolution.Service.DisplayName, payload.ResourceName)
	require.Equal(t, []string{resolution.AuthorizationServerIssuer}, payload.AuthorizationServers)
	require.Equal(t, []string{resolution.Scope}, payload.ScopesSupported)
}

func assertAuthorizationServerMetadata(t *testing.T, handler http.Handler, path string, resolution ServiceResolution) {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, path, nil)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	require.Equal(t, http.StatusOK, res.Code)

	var payload struct {
		Issuer                       string   `json:"issuer"`
		AuthorizationEndpoint        string   `json:"authorization_endpoint"`
		DeviceAuthorizationEndpoint  string   `json:"device_authorization_endpoint"`
		RegistrationEndpoint         string   `json:"registration_endpoint"`
		ScopesSupported              []string `json:"scopes_supported"`
		ResourceIndicatorsSupported  bool     `json:"resource_indicators_supported"`
		DynamicRegistrationSupported bool     `json:"dynamic_client_registration_supported"`
	}
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &payload))
	require.Equal(t, resolution.AuthorizationServerIssuer, payload.Issuer)
	require.Equal(t, resolution.AuthorizationEndpoint, payload.AuthorizationEndpoint)
	require.Equal(t, resolution.DeviceAuthorizationEndpoint, payload.DeviceAuthorizationEndpoint)
	require.Equal(t, resolution.RegistrationEndpoint, payload.RegistrationEndpoint)
	require.Equal(t, []string{resolution.Scope}, payload.ScopesSupported)
	require.True(t, payload.ResourceIndicatorsSupported)
}

func TestRootDiscoverySupportsHEAD(t *testing.T) {
	t.Parallel()

	server := newTestEdgeServer(t, nil)
	handler := server.Handler()

	req := httptest.NewRequest(http.MethodHead, "/", nil)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	require.Equal(t, http.StatusOK, res.Code)
	require.Equal(t, "application/json", res.Header().Get("Content-Type"))
}

func TestRootDiscoveryTrimsPublicBaseURL(t *testing.T) {
	t.Parallel()

	cfg := testEdgeConfig()
	cfg.PublicBaseURL = "https://mcp.example.com/"
	server, err := NewServerWithStateStore(context.Background(), cfg, zerolog.Nop(), staticResolver{}, newMutableCatalogStore(t))
	require.NoError(t, err)
	handler := server.Handler()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	require.Equal(t, http.StatusOK, res.Code)
	require.Contains(t, res.Body.String(), `"registration_endpoint":"https://mcp.example.com/oauth/register"`)
	require.NotContains(t, res.Body.String(), "https://mcp.example.com//")
}

func TestRootDiscoveryRejectsUnsupportedMethod(t *testing.T) {
	t.Parallel()

	server := newTestEdgeServer(t, nil)
	handler := server.Handler()

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	require.Equal(t, http.StatusMethodNotAllowed, res.Code)
	require.Equal(t, "GET, HEAD", res.Header().Get("Allow"))
	require.Contains(t, res.Body.String(), "method_not_allowed")
}

func TestOAuthScopesReflectRefreshedCatalog(t *testing.T) {
	t.Parallel()

	store := newMutableCatalogStore(t)
	server, err := NewServerWithStateStore(context.Background(), testEdgeConfig(), zerolog.Nop(), staticResolver{}, store)
	require.NoError(t, err)
	handler := server.Handler()

	store.entries = filterCatalogService(store.entries, "example-a")
	require.NoError(t, server.catalogCache.Refresh(context.Background()))

	req := httptest.NewRequest(http.MethodGet, "/.well-known/oauth-authorization-server", nil)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	require.Equal(t, http.StatusOK, res.Code)
	require.NotContains(t, res.Body.String(), "mcp:example-a")

	registrationBody := `{"client_name":"bad-scope","redirect_uris":["https://client.example.com/callback"],"scope":"mcp:example-a"}`
	req = httptest.NewRequest(http.MethodPost, "/oauth/register", strings.NewReader(registrationBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer fixture-operator-token")
	res = httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	require.Equal(t, http.StatusBadRequest, res.Code)
	require.Contains(t, res.Body.String(), "requested scopes are not supported")
}

func TestReservedRoutesWinOverDynamicFallback(t *testing.T) {
	t.Parallel()

	store := newMutableCatalogStore(t)
	server, err := NewServerWithStateStore(context.Background(), testEdgeConfig(), zerolog.Nop(), staticResolver{}, store)
	require.NoError(t, err)
	handler := server.Handler()

	req := httptest.NewRequest(http.MethodGet, "/health/live", nil)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	require.Equal(t, http.StatusOK, res.Code)
	require.Contains(t, res.Body.String(), "live")
}

func TestReadinessReportsCanonicalMetadataStatus(t *testing.T) {
	t.Parallel()

	server := newTestEdgeServer(t, nil)
	req := httptest.NewRequest(http.MethodGet, "/health/ready", nil)
	res := httptest.NewRecorder()
	server.Handler().ServeHTTP(res, req)

	require.Equal(t, http.StatusOK, res.Code)
	var payload map[string]any
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &payload))
	require.Equal(t, "ok", payload["canonical_metadata_status"])
}

func TestServiceDiagnosticsRequireOperatorTokenAndReportCanonicalMetadata(t *testing.T) {
	t.Parallel()

	server := newTestEdgeServer(t, nil)
	handler := server.Handler()

	req := httptest.NewRequest(http.MethodGet, "/health/diagnostics/services", nil)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	require.Equal(t, http.StatusUnauthorized, res.Code)

	req = httptest.NewRequest(http.MethodGet, "/health/diagnostics/services", nil)
	req.Header.Set("Authorization", "Bearer fixture-operator-token")
	res = httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	require.Equal(t, http.StatusOK, res.Code)

	var payload struct {
		Status   string `json:"status"`
		Services []struct {
			ID               string   `json:"id"`
			Resource         string   `json:"resource"`
			Scope            string   `json:"scope"`
			DiagnosticStatus string   `json:"diagnostic_status"`
			Issues           []string `json:"issues"`
		} `json:"services"`
	}
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &payload))
	require.Equal(t, "ok", payload.Status)
	require.Len(t, payload.Services, len(testServiceCatalogEntries()))
	for _, service := range payload.Services {
		require.NotEmpty(t, service.ID)
		require.NotEmpty(t, service.Resource)
		require.NotEmpty(t, service.Scope)
		require.Equal(t, "ok", service.DiagnosticStatus)
		require.Empty(t, service.Issues)
	}
}

func TestServiceMetadataDiagnosticsDetectInvalidCanonicalURLs(t *testing.T) {
	t.Parallel()

	issues := serviceMetadataIssues(ServiceResolution{
		ServiceID:                    "bad",
		Scope:                        "mcp:bad",
		Resource:                     "mcp.example.com/bad/mcp",
		PublicURL:                    "mcp.example.com/bad/mcp",
		ProtectedResourceMetadataURL: "mcp.example.com/.well-known/oauth-protected-resource/bad",
		AuthorizationServerIssuer:    "mcp.example.com/bad",
		AuthorizationEndpoint:        "mcp.example.com/oauth/authorize/bad",
		DeviceAuthorizationEndpoint:  "mcp.example.com/oauth/device_authorization/bad",
		RegistrationEndpoint:         "mcp.example.com/oauth/register/bad",
	})

	require.Contains(t, issues, "invalid_resource")
	require.Contains(t, issues, "invalid_public_url")
	require.Contains(t, issues, "invalid_protected_resource_metadata_url")
	require.Contains(t, issues, "invalid_authorization_server")
}

func TestCORSPreflightAllowsMCPTransportHeaders(t *testing.T) {
	t.Parallel()

	server := newTestEdgeServer(t, nil)
	handler := server.Handler()

	req := httptest.NewRequest(http.MethodOptions, "/example-a/mcp", nil)
	req.Header.Set("Origin", "https://client.example.com")
	req.Header.Set("Access-Control-Request-Headers", "authorization, mcp-protocol-version, mcp-session-id")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	require.Equal(t, http.StatusNoContent, res.Code)
	require.Equal(t, "*", res.Header().Get("Access-Control-Allow-Origin"))
	require.Contains(t, res.Header().Get("Access-Control-Allow-Headers"), "MCP-Protocol-Version")
	require.Contains(t, res.Header().Get("Access-Control-Allow-Headers"), "MCP-Session-Id")
	require.Contains(t, res.Header().Get("Access-Control-Allow-Headers"), "Last-Event-ID")
}

func TestCORSPreflightDoesNotAllowUnconfiguredOrigin(t *testing.T) {
	t.Parallel()

	cfg := testEdgeConfig()
	cfg.CORSAllowedOrigins = []string{"https://trusted.example.com"}
	server, err := NewServerWithStateStore(context.Background(), cfg, zerolog.Nop(), staticResolver{}, newMutableCatalogStore(t))
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodOptions, "/example-a/mcp", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	res := httptest.NewRecorder()
	server.Handler().ServeHTTP(res, req)

	require.Equal(t, http.StatusNoContent, res.Code)
	require.Empty(t, res.Header().Get("Access-Control-Allow-Origin"))
}

type mutableCatalogStore struct {
	*memoryEdgeStateStore
	entries []catalog.ServiceCatalogEntry
	err     error
}

func testServiceCatalogEntries() []catalog.ServiceCatalogEntry {
	return []catalog.ServiceCatalogEntry{
		{
			ServiceID:              "example-a",
			DisplayName:            "Example A",
			UpstreamServiceName:    "example-a-mcp",
			TransportType:          catalog.TransportTypeStreamableHTTP,
			InternalPort:           3031,
			PublicPath:             "/example-a/mcp",
			InternalUpstreamPath:   "/mcp",
			HealthPath:             "/health",
			HealthProbeExpectation: "GET returns OK",
			ResourceProfile:        "small",
			PersistencePolicy:      "stateless",
			AdapterRequirement:     catalog.AdapterRequirementNone,
		},
		{
			ServiceID:              "example-b",
			DisplayName:            "Example B",
			UpstreamServiceName:    "example-b-mcp",
			TransportType:          catalog.TransportTypeStreamableHTTP,
			InternalPort:           3031,
			PublicPath:             "/example-b/mcp",
			InternalUpstreamPath:   "/mcp",
			HealthPath:             "/health",
			HealthProbeExpectation: "GET returns OK",
			ResourceProfile:        "small",
			PersistencePolicy:      "stateless",
			AdapterRequirement:     catalog.AdapterRequirementNone,
		},
	}
}

func newMutableCatalogStore(t *testing.T) *mutableCatalogStore {
	t.Helper()
	memoryStore, err := newMemoryEdgeStateStore()
	require.NoError(t, err)
	return &mutableCatalogStore{memoryEdgeStateStore: memoryStore, entries: testServiceCatalogEntries()}
}

func (s *mutableCatalogStore) ListEnabledServiceCatalog(context.Context) ([]catalog.ServiceCatalogEntry, error) {
	if s.err != nil {
		return nil, s.err
	}
	return append([]catalog.ServiceCatalogEntry(nil), s.entries...), nil
}

func testEdgeConfig() Config {
	return Config{
		PlatformEnv:               "test",
		PublicBaseURL:             "https://mcp.example.com",
		CookieSecure:              false,
		CORSAllowedOrigins:        []string{"*"},
		EnableFixtureMode:         true,
		OAuthAccessTokenTTL:       defaultOAuthAccessTokenTTL,
		OAuthRefreshTokenTTL:      defaultOAuthRefreshTokenTTL,
		OAuthAuthorizationCodeTTL: defaultOAuthAuthorizationCodeTTL,
		OAuthDeviceCodeTTL:        defaultOAuthDeviceCodeTTL,
		FixtureAuthSubjectSub:     "fixture-user",
		FixtureAuthGroups:         []string{"mcp-users", "mcp-service-example-a"},
		FixtureOperatorToken:      "fixture-operator-token",
	}
}

func filterCatalogService(entries []catalog.ServiceCatalogEntry, serviceID string) []catalog.ServiceCatalogEntry {
	filtered := make([]catalog.ServiceCatalogEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.ServiceID != serviceID {
			filtered = append(filtered, entry)
		}
	}
	return filtered
}
