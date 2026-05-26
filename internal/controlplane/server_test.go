package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dragonserver/mcp-platform/internal/catalog"
	"dragonserver/mcp-platform/internal/domain"
	"dragonserver/mcp-platform/internal/ids"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

type rewriteHostTransport struct {
	target *url.URL
}

func (t rewriteHostTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.URL.Scheme = t.target.Scheme
	clone.URL.Host = t.target.Host
	return http.DefaultTransport.RoundTrip(clone)
}

func installStaticUpstreamTestNetwork(t *testing.T, upstreamURL string) string {
	t.Helper()
	target, err := url.Parse(upstreamURL)
	require.NoError(t, err)

	previousLookup := lookupStaticUpstreamIP
	previousClient := newStaticUpstreamHealthClient
	lookupStaticUpstreamIP = func(host string) ([]net.IP, error) {
		if host == "mcp-lan.test" {
			return []net.IP{net.ParseIP("192.168.1.10")}, nil
		}
		return previousLookup(host)
	}
	newStaticUpstreamHealthClient = func() *http.Client {
		return &http.Client{
			Timeout:   5 * time.Second,
			Transport: rewriteHostTransport{target: target},
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	t.Cleanup(func() {
		lookupStaticUpstreamIP = previousLookup
		newStaticUpstreamHealthClient = previousClient
	})

	return target.Scheme + "://mcp-lan.test:" + target.Port()
}

func catalogEntryForTest(serviceID string, publicPath string) catalog.ServiceCatalogEntry {
	return catalog.ServiceCatalogEntry{
		ServiceID:              serviceID,
		DisplayName:            "Test MCP",
		UpstreamServiceName:    serviceID + "-mcp",
		TransportType:          catalog.TransportTypeStreamableHTTP,
		InternalPort:           7070,
		PublicPath:             publicPath,
		InternalUpstreamPath:   "/mcp",
		HealthPath:             "/health",
		HealthProbeExpectation: "GET returns OK",
		ResourceProfile:        "small",
		PersistencePolicy:      "stateless",
		AdapterRequirement:     catalog.AdapterRequirementNone,
	}
}

const serverTestServiceID = "example-mcp"

func registerServerTestService(t *testing.T, ctx context.Context, store *Store, serviceID string) {
	t.Helper()
	require.NoError(t, store.UpsertAdminServiceCatalogEntry(ctx, catalogEntryForTest(serviceID, "/"+serviceID+"/mcp")))
}

func registerBuiltinServerTestService(t *testing.T, ctx context.Context, store *Store, serviceID string) {
	t.Helper()
	registerServerTestService(t, ctx, store, serviceID)
	_, err := store.db.ExecContext(ctx, `UPDATE service_catalog SET source = 'builtin' WHERE service_id = ?`, serviceID)
	require.NoError(t, err)
}

func createStaticTenantForServerTest(t *testing.T, ctx context.Context, store *Store, subject domain.Subject, serviceID string) {
	t.Helper()
	require.NoError(t, store.UpsertManualServiceGrant(ctx, subject, serviceID))
	require.NoError(t, store.UpsertStaticTenantUpstream(ctx, subject, serviceID, "http://"+serviceID+".example/mcp", time.Now().UTC()))
}

func TestHealthStateClearsDatabaseErrorAfterRecovery(t *testing.T) {
	t.Parallel()

	state := &healthState{}
	state.setDatabaseStatus(errors.New("ping database: boom"))

	snapshot := state.snapshot()
	require.False(t, snapshot.Ready)
	require.Equal(t, "ping database: boom", snapshot.LastError)

	state.setDatabaseStatus(nil)
	snapshot = state.snapshot()
	require.True(t, snapshot.Ready)
	require.Empty(t, snapshot.LastError)
}

func TestHealthStatePrefersReconcileError(t *testing.T) {
	t.Parallel()

	state := &healthState{}
	state.setDatabaseStatus(nil)
	state.setReconcileResult(ReconcileSummary{}, errors.New("reconcile failed"))

	snapshot := state.snapshot()
	require.False(t, snapshot.Ready)
	require.Equal(t, "reconcile failed", snapshot.LastError)

	state.setReconcileResult(ReconcileSummary{LastRunAt: time.Now().UTC()}, nil)
	snapshot = state.snapshot()
	require.True(t, snapshot.Ready)
	require.Empty(t, snapshot.LastError)
}

func TestRunStartupSequenceContinuesOnInitialReconcileFailure(t *testing.T) {
	t.Parallel()

	var callOrder []string
	err := runStartupSequence(
		context.Background(),
		zerolog.Nop(),
		func(context.Context) error {
			callOrder = append(callOrder, "seed")
			return nil
		},
		func(context.Context) error {
			callOrder = append(callOrder, "probe")
			return nil
		},
		func(context.Context) (ReconcileSummary, error) {
			callOrder = append(callOrder, "reconcile")
			return ReconcileSummary{}, errors.New("initial reconcile failed")
		},
	)

	require.NoError(t, err)
	require.Equal(t, []string{"seed", "probe", "reconcile"}, callOrder)
}

func TestRunStartupSequenceStopsBeforeReconcileWhenHealthProbeFails(t *testing.T) {
	t.Parallel()

	reconcileCalled := false
	err := runStartupSequence(
		context.Background(),
		zerolog.Nop(),
		func(context.Context) error { return nil },
		func(context.Context) error { return errors.New("probe failed") },
		func(context.Context) (ReconcileSummary, error) {
			reconcileCalled = true
			return ReconcileSummary{}, nil
		},
	)

	require.ErrorContains(t, err, "probe failed")
	require.False(t, reconcileCalled)
}

func TestRunLeaseRenewalLoopCancelsRuntimeOnLeaseLoss(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runtimeCanceled := make(chan struct{})
	runtimeCancel := func() {
		cancel()
		close(runtimeCanceled)
	}

	app := &App{logger: zerolog.Nop()}
	go app.runLeaseRenewalLoopWithInterval(ctx, runtimeCancel, time.Millisecond)

	select {
	case <-runtimeCanceled:
	case <-time.After(time.Second):
		t.Fatal("lease renewal loop did not cancel runtime after leadership loss")
	}
}

func TestRequireLeadershipReturnsContextCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	app := &App{logger: zerolog.Nop()}
	require.ErrorIs(t, app.requireLeadership(ctx), context.Canceled)
}

func TestHandleReadinessReportsConfigErrors(t *testing.T) {
	t.Parallel()

	t.Run("missing dependencies", func(t *testing.T) {
		t.Parallel()

		app := &App{
			cfg:    Config{},
			health: &healthState{},
		}
		app.health.setDatabaseStatus(nil)
		app.health.setReconcileResult(ReconcileSummary{LastRunAt: time.Now().UTC()}, nil)

		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/health/ready", nil)
		app.handleReadiness(recorder, request)

		require.Equal(t, http.StatusServiceUnavailable, recorder.Code)

		var payload map[string]any
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &payload))
		require.Equal(t, "not_ready", payload["status"])
		require.Empty(t, payload["last_error"])
		require.Equal(t, false, payload["leader"])
		require.Equal(t, false, payload["dependencies_configured"])
		require.Equal(t, false, payload["tenant_runtime_configured"])
	})

	t.Run("missing tenant runtime", func(t *testing.T) {
		t.Parallel()

		app := &App{
			cfg: Config{
				AuthentikIssuerURL:               "https://auth.example.com/application/o/mcp/",
				AuthentikClientID:                "client-id",
				AuthentikClientSecretPath:        "/run/secrets/authentik-client-secret",
				CoolifyAPIBaseURL:                "https://coolify.example.com/api/v1",
				CoolifyAPITokenPath:              "/run/secrets/coolify-api-token",
				InfisicalAPIBaseURL:              "https://infisical.example.com/api",
				InfisicalProjectSlug:             "example-project",
				InfisicalEnvSlug:                 "prod",
				InfisicalMachineClientID:         "machine-id",
				InfisicalMachineClientSecretPath: "/run/secrets/infisical-machine-secret",
			},
			health: &healthState{},
		}
		app.health.setDatabaseStatus(nil)
		app.health.setReconcileResult(ReconcileSummary{LastRunAt: time.Now().UTC()}, nil)

		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/health/ready", nil)
		app.handleReadiness(recorder, request)

		require.Equal(t, http.StatusServiceUnavailable, recorder.Code)

		var payload map[string]any
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &payload))
		require.Equal(t, "not_ready", payload["status"])
		require.Empty(t, payload["last_error"])
		require.Equal(t, false, payload["leader"])
		require.Equal(t, true, payload["dependencies_configured"])
		require.Equal(t, false, payload["tenant_runtime_configured"])
	})
}

func TestCatalogAdminRegistersDynamicService(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, err := NewStore(ctx, "file:"+filepath.Join(t.TempDir(), "mcp-platform.db"), zerolog.New(io.Discard))
	require.NoError(t, err)
	defer store.Close()
	require.NoError(t, store.RunMigrations(ctx))
	require.NoError(t, store.SeedServiceCatalog(ctx))

	tokenPath := filepath.Join(t.TempDir(), "admin-token")
	require.NoError(t, os.WriteFile(tokenPath, []byte("test-admin-token"), 0o600))
	app := &App{cfg: Config{AdminTokenPath: tokenPath}, store: store, health: &healthState{}}

	body := `{
  "display_name": "Custom MCP",
  "upstream_service_name": "custom-mcp",
  "transport_type": "streamable-http",
  "internal_port": 7070,
  "public_path": "/custom/mcp",
  "internal_upstream_path": "/mcp",
  "health_path": "/health",
  "health_probe_expectation": "GET returns OK",
  "resource_profile": "small",
  "persistence_policy": "stateless",
  "adapter_requirement": "none",
  "secret_contract": [{"Key":"api-token","Required":true}]
}`
	request := httptest.NewRequest(http.MethodPut, "/v1/services/custom", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer test-admin-token")
	recorder := httptest.NewRecorder()
	app.Handler().ServeHTTP(recorder, request)

	require.Equal(t, http.StatusOK, recorder.Code)

	var enabled int
	require.NoError(t, store.db.QueryRowContext(ctx, `SELECT enabled FROM service_catalog WHERE service_id = 'custom'`).Scan(&enabled))
	require.Equal(t, 1, enabled)

	require.NoError(t, store.SeedServiceCatalog(ctx))
	require.NoError(t, store.db.QueryRowContext(ctx, `SELECT enabled FROM service_catalog WHERE service_id = 'custom'`).Scan(&enabled))
	require.Equal(t, 1, enabled)
}

func TestCatalogAdminRequiresBearerToken(t *testing.T) {
	t.Parallel()

	tokenPath := filepath.Join(t.TempDir(), "admin-token")
	require.NoError(t, os.WriteFile(tokenPath, []byte("test-admin-token"), 0o600))
	app := &App{cfg: Config{AdminTokenPath: tokenPath}, health: &healthState{}}

	request := httptest.NewRequest(http.MethodGet, "/v1/services", nil)
	recorder := httptest.NewRecorder()
	app.Handler().ServeHTTP(recorder, request)

	require.Equal(t, http.StatusUnauthorized, recorder.Code)
}

func TestCatalogAdminRejectsReservedPublicPath(t *testing.T) {
	t.Parallel()

	tokenPath := filepath.Join(t.TempDir(), "admin-token")
	require.NoError(t, os.WriteFile(tokenPath, []byte("test-admin-token"), 0o600))
	app := &App{cfg: Config{AdminTokenPath: tokenPath}, health: &healthState{}}

	body := `{
  "display_name": "Bad MCP",
  "upstream_service_name": "bad-mcp",
  "transport_type": "streamable-http",
  "internal_port": 7070,
  "public_path": "/oauth/bad",
  "internal_upstream_path": "/mcp",
  "health_path": "/health",
  "health_probe_expectation": "GET returns OK",
  "resource_profile": "small",
  "persistence_policy": "stateless",
  "adapter_requirement": "none"
}`
	request := httptest.NewRequest(http.MethodPut, "/v1/services/bad", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer test-admin-token")
	recorder := httptest.NewRecorder()
	app.Handler().ServeHTTP(recorder, request)

	require.Equal(t, http.StatusBadRequest, recorder.Code)
}

func TestCatalogAdminRejectsSuspiciousPublicPath(t *testing.T) {
	t.Parallel()

	tokenPath := filepath.Join(t.TempDir(), "admin-token")
	require.NoError(t, os.WriteFile(tokenPath, []byte("test-admin-token"), 0o600))
	app := &App{cfg: Config{AdminTokenPath: tokenPath}, health: &healthState{}}

	body := `{
  "display_name": "Bad MCP",
  "upstream_service_name": "bad-mcp",
  "transport_type": "streamable-http",
  "internal_port": 7070,
  "public_path": "/custom/mcp?x",
  "internal_upstream_path": "/mcp",
  "health_path": "/health",
  "health_probe_expectation": "GET returns OK",
  "resource_profile": "small",
  "persistence_policy": "stateless",
  "adapter_requirement": "none"
}`
	request := httptest.NewRequest(http.MethodPut, "/v1/services/bad", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer test-admin-token")
	recorder := httptest.NewRecorder()
	app.Handler().ServeHTTP(recorder, request)

	require.Equal(t, http.StatusBadRequest, recorder.Code)
}

func TestCatalogAdminReportsSourceAndEnabled(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, err := NewStore(ctx, "file:"+filepath.Join(t.TempDir(), "mcp-platform.db"), zerolog.New(io.Discard))
	require.NoError(t, err)
	defer store.Close()
	require.NoError(t, store.RunMigrations(ctx))
	require.NoError(t, store.SeedServiceCatalog(ctx))
	registerBuiltinServerTestService(t, ctx, store, serverTestServiceID)

	tokenPath := filepath.Join(t.TempDir(), "admin-token")
	require.NoError(t, os.WriteFile(tokenPath, []byte("test-admin-token"), 0o600))
	app := &App{cfg: Config{AdminTokenPath: tokenPath}, store: store, health: &healthState{}}

	request := httptest.NewRequest(http.MethodGet, "/v1/services/"+serverTestServiceID, nil)
	request.Header.Set("Authorization", "Bearer test-admin-token")
	recorder := httptest.NewRecorder()
	app.Handler().ServeHTTP(recorder, request)

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Contains(t, recorder.Body.String(), `"source":"builtin"`)
	require.Contains(t, recorder.Body.String(), `"enabled":true`)
}

func TestCatalogAdminRejectsBuiltinMutation(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, err := NewStore(ctx, "file:"+filepath.Join(t.TempDir(), "mcp-platform.db"), zerolog.New(io.Discard))
	require.NoError(t, err)
	defer store.Close()
	require.NoError(t, store.RunMigrations(ctx))
	require.NoError(t, store.SeedServiceCatalog(ctx))
	registerBuiltinServerTestService(t, ctx, store, serverTestServiceID)

	tokenPath := filepath.Join(t.TempDir(), "admin-token")
	require.NoError(t, os.WriteFile(tokenPath, []byte("test-admin-token"), 0o600))
	app := &App{cfg: Config{AdminTokenPath: tokenPath}, store: store, health: &healthState{}}
	handler := app.Handler()

	body := `{
  "display_name": "Bad MCP",
  "upstream_service_name": "bad-mcp",
  "transport_type": "streamable-http",
  "internal_port": 7070,
  "public_path": "/bad-mcp/mcp",
  "internal_upstream_path": "/mcp",
  "health_path": "/health",
  "health_probe_expectation": "GET returns OK",
  "resource_profile": "small",
  "persistence_policy": "stateless",
  "adapter_requirement": "none"
}`
	putRequest := httptest.NewRequest(http.MethodPut, "/v1/services/"+serverTestServiceID, strings.NewReader(body))
	putRequest.Header.Set("Authorization", "Bearer test-admin-token")
	putRecorder := httptest.NewRecorder()
	handler.ServeHTTP(putRecorder, putRequest)
	require.Equal(t, http.StatusConflict, putRecorder.Code)
	require.Contains(t, putRecorder.Body.String(), `"error":"builtin_service_locked"`)

	deleteRequest := httptest.NewRequest(http.MethodDelete, "/v1/services/"+serverTestServiceID, nil)
	deleteRequest.Header.Set("Authorization", "Bearer test-admin-token")
	deleteRecorder := httptest.NewRecorder()
	handler.ServeHTTP(deleteRecorder, deleteRequest)
	require.Equal(t, http.StatusConflict, deleteRecorder.Code)
	require.Contains(t, deleteRecorder.Body.String(), `"error":"builtin_service_locked"`)
}

func TestCatalogAdminRejectsOverlappingPublicPath(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, err := NewStore(ctx, "file:"+filepath.Join(t.TempDir(), "mcp-platform.db"), zerolog.New(io.Discard))
	require.NoError(t, err)
	defer store.Close()
	require.NoError(t, store.RunMigrations(ctx))
	require.NoError(t, store.SeedServiceCatalog(ctx))
	registerServerTestService(t, ctx, store, serverTestServiceID)
	require.NoError(t, store.UpsertAdminServiceCatalogEntry(ctx, catalogEntryForTest("custom", "/custom/mcp")))

	tokenPath := filepath.Join(t.TempDir(), "admin-token")
	require.NoError(t, os.WriteFile(tokenPath, []byte("test-admin-token"), 0o600))
	app := &App{cfg: Config{AdminTokenPath: tokenPath}, store: store, health: &healthState{}}

	body := `{
  "display_name": "Nested MCP",
  "upstream_service_name": "nested-mcp",
  "transport_type": "streamable-http",
  "internal_port": 7070,
  "public_path": "/custom/mcp/admin",
  "internal_upstream_path": "/mcp",
  "health_path": "/health",
  "health_probe_expectation": "GET returns OK",
  "resource_profile": "small",
  "persistence_policy": "stateless",
  "adapter_requirement": "none"
}`
	request := httptest.NewRequest(http.MethodPut, "/v1/services/nested", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer test-admin-token")
	recorder := httptest.NewRecorder()
	app.Handler().ServeHTTP(recorder, request)

	require.Equal(t, http.StatusConflict, recorder.Code)
	require.Contains(t, recorder.Body.String(), `"error":"public_path_conflict"`)
}

func TestGrantAdminUpsertsListsAndDeletesManualGrant(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, err := NewStore(ctx, "file:"+filepath.Join(t.TempDir(), "mcp-platform.db"), zerolog.New(io.Discard))
	require.NoError(t, err)
	defer store.Close()
	require.NoError(t, store.RunMigrations(ctx))
	require.NoError(t, store.SeedServiceCatalog(ctx))
	registerServerTestService(t, ctx, store, serverTestServiceID)

	tokenPath := filepath.Join(t.TempDir(), "admin-token")
	require.NoError(t, os.WriteFile(tokenPath, []byte("test-admin-token"), 0o600))
	app := &App{cfg: Config{AdminTokenPath: tokenPath}, store: store, health: &healthState{}}
	handler := app.Handler()

	putRequest := httptest.NewRequest(http.MethodPut, "/v1/subjects/manual-sub/grants/"+serverTestServiceID, nil)
	putRequest.Header.Set("Authorization", "Bearer test-admin-token")
	putRecorder := httptest.NewRecorder()
	handler.ServeHTTP(putRecorder, putRequest)
	require.Equal(t, http.StatusOK, putRecorder.Code)

	getRequest := httptest.NewRequest(http.MethodGet, "/v1/subjects/manual-sub/grants", nil)
	getRequest.Header.Set("Authorization", "Bearer test-admin-token")
	getRecorder := httptest.NewRecorder()
	handler.ServeHTTP(getRecorder, getRequest)
	require.Equal(t, http.StatusOK, getRecorder.Code)
	require.Contains(t, getRecorder.Body.String(), `"service_id":"`+serverTestServiceID+`"`)
	require.Contains(t, getRecorder.Body.String(), `"source_group":"manual"`)

	deleteRequest := httptest.NewRequest(http.MethodDelete, "/v1/subjects/manual-sub/grants/"+serverTestServiceID, nil)
	deleteRequest.Header.Set("Authorization", "Bearer test-admin-token")
	deleteRecorder := httptest.NewRecorder()
	handler.ServeHTTP(deleteRecorder, deleteRequest)
	require.Equal(t, http.StatusNoContent, deleteRecorder.Code)

	grants, err := store.ListSubjectServiceGrants(ctx, "manual-sub")
	require.NoError(t, err)
	require.Empty(t, grants)
}

func TestGrantAdminReturnsNotFoundForUnknownService(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, err := NewStore(ctx, "file:"+filepath.Join(t.TempDir(), "mcp-platform.db"), zerolog.New(io.Discard))
	require.NoError(t, err)
	defer store.Close()
	require.NoError(t, store.RunMigrations(ctx))
	require.NoError(t, store.SeedServiceCatalog(ctx))
	registerServerTestService(t, ctx, store, serverTestServiceID)

	tokenPath := filepath.Join(t.TempDir(), "admin-token")
	require.NoError(t, os.WriteFile(tokenPath, []byte("test-admin-token"), 0o600))
	app := &App{cfg: Config{AdminTokenPath: tokenPath}, store: store, health: &healthState{}}

	request := httptest.NewRequest(http.MethodPut, "/v1/subjects/manual-sub/grants/unknown", nil)
	request.Header.Set("Authorization", "Bearer test-admin-token")
	recorder := httptest.NewRecorder()
	app.Handler().ServeHTTP(recorder, request)

	require.Equal(t, http.StatusNotFound, recorder.Code)
}

func TestStaticUpstreamAdminBindsGrantedSubject(t *testing.T) {
	ctx := context.Background()
	store, err := NewStore(ctx, "file:"+filepath.Join(t.TempDir(), "mcp-platform.db"), zerolog.New(io.Discard))
	require.NoError(t, err)
	defer store.Close()
	require.NoError(t, store.RunMigrations(ctx))
	require.NoError(t, store.SeedServiceCatalog(ctx))
	registerServerTestService(t, ctx, store, serverTestServiceID)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/health", r.URL.Path)
		_, _ = w.Write([]byte(`{"transport":"streamable"}`))
	}))
	defer upstream.Close()
	upstreamURL := installStaticUpstreamTestNetwork(t, upstream.URL)

	tokenPath := filepath.Join(t.TempDir(), "admin-token")
	require.NoError(t, os.WriteFile(tokenPath, []byte("test-admin-token"), 0o600))
	app := &App{cfg: Config{AdminTokenPath: tokenPath}, store: store, health: &healthState{}}
	handler := app.Handler()

	grantRequest := httptest.NewRequest(http.MethodPut, "/v1/subjects/manual-sub/grants/"+serverTestServiceID, nil)
	grantRequest.Header.Set("Authorization", "Bearer test-admin-token")
	grantRecorder := httptest.NewRecorder()
	handler.ServeHTTP(grantRecorder, grantRequest)
	require.Equal(t, http.StatusOK, grantRecorder.Code)

	body := `{"upstream_url":"` + upstreamURL + `/mcp"}`
	bindRequest := httptest.NewRequest(http.MethodPut, "/v1/subjects/manual-sub/services/"+serverTestServiceID+"/upstream", strings.NewReader(body))
	bindRequest.Header.Set("Authorization", "Bearer test-admin-token")
	bindRecorder := httptest.NewRecorder()
	handler.ServeHTTP(bindRecorder, bindRequest)
	require.Equal(t, http.StatusOK, bindRecorder.Code)
	require.Contains(t, bindRecorder.Body.String(), `"upstream_url":"`+upstreamURL+`/mcp"`)

	tenants, err := store.ListTenantInstances(ctx)
	require.NoError(t, err)
	require.Len(t, tenants, 1)
	require.Equal(t, "manual-sub", tenants[0].SubjectSub)
	require.Equal(t, serverTestServiceID, tenants[0].ServiceID)
	require.Equal(t, upstreamURL+"/mcp", tenants[0].UpstreamURL)
	require.Equal(t, domain.TenantRuntimeStateReady, tenants[0].RuntimeState)
	require.NotNil(t, tenants[0].LastHealthyAt)
}

func TestStaticUpstreamAdminRequiresGrant(t *testing.T) {
	ctx := context.Background()
	store, err := NewStore(ctx, "file:"+filepath.Join(t.TempDir(), "mcp-platform.db"), zerolog.New(io.Discard))
	require.NoError(t, err)
	defer store.Close()
	require.NoError(t, store.RunMigrations(ctx))
	require.NoError(t, store.SeedServiceCatalog(ctx))
	registerServerTestService(t, ctx, store, serverTestServiceID)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"transport":"streamable"}`))
	}))
	defer upstream.Close()
	upstreamURL := installStaticUpstreamTestNetwork(t, upstream.URL)

	tokenPath := filepath.Join(t.TempDir(), "admin-token")
	require.NoError(t, os.WriteFile(tokenPath, []byte("test-admin-token"), 0o600))
	app := &App{cfg: Config{AdminTokenPath: tokenPath}, store: store, health: &healthState{}}

	body := `{"upstream_url":"` + upstreamURL + `/mcp"}`
	request := httptest.NewRequest(http.MethodPut, "/v1/subjects/manual-sub/services/"+serverTestServiceID+"/upstream", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer test-admin-token")
	recorder := httptest.NewRecorder()
	app.Handler().ServeHTTP(recorder, request)

	require.Equal(t, http.StatusConflict, recorder.Code)
	require.Contains(t, recorder.Body.String(), "service_not_granted")
}

func TestTenantLifecycleAdminSuspendsAndResumesTenant(t *testing.T) {
	ctx := context.Background()
	store, err := NewStore(ctx, "file:"+filepath.Join(t.TempDir(), "mcp-platform.db"), zerolog.New(io.Discard))
	require.NoError(t, err)
	defer store.Close()
	require.NoError(t, store.RunMigrations(ctx))
	require.NoError(t, store.SeedServiceCatalog(ctx))
	registerServerTestService(t, ctx, store, serverTestServiceID)
	createStaticTenantForServerTest(t, ctx, store, domain.Subject{Sub: "manual-sub", SubjectKey: "manual-key"}, serverTestServiceID)

	tokenPath := filepath.Join(t.TempDir(), "admin-token")
	require.NoError(t, os.WriteFile(tokenPath, []byte("test-admin-token"), 0o600))
	app := &App{cfg: Config{AdminTokenPath: tokenPath}, store: store, health: &healthState{}}
	handler := app.Handler()

	suspendRequest := httptest.NewRequest(http.MethodPut, "/v1/subjects/manual-sub/services/"+serverTestServiceID+"/suspend", nil)
	suspendRequest.Header.Set("Authorization", "Bearer test-admin-token")
	suspendRecorder := httptest.NewRecorder()
	handler.ServeHTTP(suspendRecorder, suspendRequest)
	require.Equal(t, http.StatusOK, suspendRecorder.Code)
	require.Contains(t, suspendRecorder.Body.String(), `"desired_state":"disabled"`)

	tenants, err := store.ListTenantInstances(ctx)
	require.NoError(t, err)
	require.Len(t, tenants, 1)
	require.Equal(t, domain.TenantDesiredStateDisabled, tenants[0].DesiredState)

	resumeRequest := httptest.NewRequest(http.MethodPut, "/v1/subjects/manual-sub/services/"+serverTestServiceID+"/resume", nil)
	resumeRequest.Header.Set("Authorization", "Bearer test-admin-token")
	resumeRecorder := httptest.NewRecorder()
	handler.ServeHTTP(resumeRecorder, resumeRequest)
	require.Equal(t, http.StatusOK, resumeRecorder.Code)
	require.Contains(t, resumeRecorder.Body.String(), `"desired_state":"enabled"`)

	tenants, err = store.ListTenantInstances(ctx)
	require.NoError(t, err)
	require.Len(t, tenants, 1)
	require.Equal(t, domain.TenantDesiredStateEnabled, tenants[0].DesiredState)
}

func TestTenantLifecycleAdminResumeRequiresGrant(t *testing.T) {
	ctx := context.Background()
	store, err := NewStore(ctx, "file:"+filepath.Join(t.TempDir(), "mcp-platform.db"), zerolog.New(io.Discard))
	require.NoError(t, err)
	defer store.Close()
	require.NoError(t, store.RunMigrations(ctx))
	require.NoError(t, store.SeedServiceCatalog(ctx))

	tokenPath := filepath.Join(t.TempDir(), "admin-token")
	require.NoError(t, os.WriteFile(tokenPath, []byte("test-admin-token"), 0o600))
	app := &App{cfg: Config{AdminTokenPath: tokenPath}, store: store, health: &healthState{}}

	request := httptest.NewRequest(http.MethodPut, "/v1/subjects/manual-sub/services/"+serverTestServiceID+"/resume", nil)
	request.Header.Set("Authorization", "Bearer test-admin-token")
	recorder := httptest.NewRecorder()
	app.Handler().ServeHTTP(recorder, request)

	require.Equal(t, http.StatusConflict, recorder.Code)
	require.Contains(t, recorder.Body.String(), "service_not_granted")
}

func TestMemoryBankAdminUpsertsAndListsProject(t *testing.T) {
	ctx := context.Background()
	store, err := NewStore(ctx, "file:"+filepath.Join(t.TempDir(), "mcp-platform.db"), zerolog.New(io.Discard))
	require.NoError(t, err)
	defer store.Close()
	require.NoError(t, store.RunMigrations(ctx))
	require.NoError(t, store.SeedServiceCatalog(ctx))
	registerServerTestService(t, ctx, store, serverTestServiceID)
	createStaticTenantForServerTest(t, ctx, store, domain.Subject{Sub: "owner-sub", SubjectKey: "owner-key"}, serverTestServiceID)

	tokenPath := filepath.Join(t.TempDir(), "admin-token")
	require.NoError(t, os.WriteFile(tokenPath, []byte("test-admin-token"), 0o600))
	app := &App{cfg: Config{AdminTokenPath: tokenPath}, store: store, health: &healthState{}}
	handler := app.Handler()

	body := `{"display_name":"Work Notes","root_path":"/projects/work","metadata":{"color":"blue"}}`
	putRequest := httptest.NewRequest(http.MethodPut, "/v1/subjects/owner-sub/services/"+serverTestServiceID+"/projects/work", strings.NewReader(body))
	putRequest.Header.Set("Authorization", "Bearer test-admin-token")
	putRecorder := httptest.NewRecorder()
	handler.ServeHTTP(putRecorder, putRequest)
	require.Equal(t, http.StatusOK, putRecorder.Code)
	require.Contains(t, putRecorder.Body.String(), `"project_key":"work"`)
	require.Contains(t, putRecorder.Body.String(), `"root_path":"/projects/work"`)

	getRequest := httptest.NewRequest(http.MethodGet, "/v1/subjects/owner-sub/services/"+serverTestServiceID+"/projects", nil)
	getRequest.Header.Set("Authorization", "Bearer test-admin-token")
	getRecorder := httptest.NewRecorder()
	handler.ServeHTTP(getRecorder, getRequest)
	require.Equal(t, http.StatusOK, getRecorder.Code)
	require.Contains(t, getRecorder.Body.String(), `"project_key":"work"`)
	require.Contains(t, getRecorder.Body.String(), `"display_name":"Work Notes"`)
}

func TestMemoryBankAdminCreatesAcceptsAndRevokesShare(t *testing.T) {
	ctx := context.Background()
	store, err := NewStore(ctx, "file:"+filepath.Join(t.TempDir(), "mcp-platform.db"), zerolog.New(io.Discard))
	require.NoError(t, err)
	defer store.Close()
	require.NoError(t, store.RunMigrations(ctx))
	require.NoError(t, store.SeedServiceCatalog(ctx))
	registerServerTestService(t, ctx, store, serverTestServiceID)
	createStaticTenantForServerTest(t, ctx, store, domain.Subject{Sub: "owner-sub", SubjectKey: "owner-key"}, serverTestServiceID)

	tokenPath := filepath.Join(t.TempDir(), "admin-token")
	require.NoError(t, os.WriteFile(tokenPath, []byte("test-admin-token"), 0o600))
	app := &App{cfg: Config{AdminTokenPath: tokenPath}, store: store, health: &healthState{}}
	handler := app.Handler()

	projectRequest := httptest.NewRequest(http.MethodPut, "/v1/subjects/owner-sub/services/"+serverTestServiceID+"/projects/work", strings.NewReader(`{"display_name":"Work Notes","root_path":"/projects/work"}`))
	projectRequest.Header.Set("Authorization", "Bearer test-admin-token")
	projectRecorder := httptest.NewRecorder()
	handler.ServeHTTP(projectRecorder, projectRequest)
	require.Equal(t, http.StatusOK, projectRecorder.Code)

	shareBody := `{"collaborator_subject_sub":"collab-sub","permission":"edit","metadata":{"reason":"pairing"}}`
	shareRequest := httptest.NewRequest(http.MethodPost, "/v1/subjects/owner-sub/services/"+serverTestServiceID+"/projects/work/shares", strings.NewReader(shareBody))
	shareRequest.Header.Set("Authorization", "Bearer test-admin-token")
	shareRecorder := httptest.NewRecorder()
	handler.ServeHTTP(shareRecorder, shareRequest)
	require.Equal(t, http.StatusCreated, shareRecorder.Code)

	var sharePayload map[string]any
	require.NoError(t, json.Unmarshal(shareRecorder.Body.Bytes(), &sharePayload))
	shareID, ok := sharePayload["share_id"].(string)
	require.True(t, ok)
	require.NotEmpty(t, shareID)
	require.Equal(t, "pending", sharePayload["state"])

	acceptRequest := httptest.NewRequest(http.MethodPut, "/v1/subjects/collab-sub/services/"+serverTestServiceID+"/shares/"+shareID+"/accept", nil)
	acceptRequest.Header.Set("Authorization", "Bearer test-admin-token")
	acceptRecorder := httptest.NewRecorder()
	handler.ServeHTTP(acceptRecorder, acceptRequest)
	require.Equal(t, http.StatusOK, acceptRecorder.Code)
	require.Contains(t, acceptRecorder.Body.String(), `"state":"active"`)

	listRequest := httptest.NewRequest(http.MethodGet, "/v1/subjects/collab-sub/services/"+serverTestServiceID+"/shares", nil)
	listRequest.Header.Set("Authorization", "Bearer test-admin-token")
	listRecorder := httptest.NewRecorder()
	handler.ServeHTTP(listRecorder, listRequest)
	require.Equal(t, http.StatusOK, listRecorder.Code)
	require.Contains(t, listRecorder.Body.String(), shareID)
	require.Contains(t, listRecorder.Body.String(), `"permission":"edit"`)

	deleteRequest := httptest.NewRequest(http.MethodDelete, "/v1/subjects/owner-sub/services/"+serverTestServiceID+"/shares/"+shareID, nil)
	deleteRequest.Header.Set("Authorization", "Bearer test-admin-token")
	deleteRecorder := httptest.NewRecorder()
	handler.ServeHTTP(deleteRecorder, deleteRequest)
	require.Equal(t, http.StatusNoContent, deleteRecorder.Code)

	listAfterDeleteRequest := httptest.NewRequest(http.MethodGet, "/v1/subjects/collab-sub/services/"+serverTestServiceID+"/shares", nil)
	listAfterDeleteRequest.Header.Set("Authorization", "Bearer test-admin-token")
	listAfterDeleteRecorder := httptest.NewRecorder()
	handler.ServeHTTP(listAfterDeleteRecorder, listAfterDeleteRequest)
	require.Equal(t, http.StatusOK, listAfterDeleteRecorder.Code)
	require.NotContains(t, listAfterDeleteRecorder.Body.String(), shareID)
}

func TestMemoryBankAdminHidesExpiredSharesByDefault(t *testing.T) {
	ctx := context.Background()
	store, err := NewStore(ctx, "file:"+filepath.Join(t.TempDir(), "mcp-platform.db"), zerolog.New(io.Discard))
	require.NoError(t, err)
	defer store.Close()
	require.NoError(t, store.RunMigrations(ctx))
	require.NoError(t, store.SeedServiceCatalog(ctx))
	registerServerTestService(t, ctx, store, serverTestServiceID)
	createStaticTenantForServerTest(t, ctx, store, domain.Subject{Sub: "owner-sub", SubjectKey: "owner-key"}, serverTestServiceID)

	tokenPath := filepath.Join(t.TempDir(), "admin-token")
	require.NoError(t, os.WriteFile(tokenPath, []byte("test-admin-token"), 0o600))
	app := &App{cfg: Config{AdminTokenPath: tokenPath}, store: store, health: &healthState{}}
	handler := app.Handler()

	projectRequest := httptest.NewRequest(http.MethodPut, "/v1/subjects/owner-sub/services/"+serverTestServiceID+"/projects/work", strings.NewReader(`{"display_name":"Work Notes","root_path":"/projects/work"}`))
	projectRequest.Header.Set("Authorization", "Bearer test-admin-token")
	projectRecorder := httptest.NewRecorder()
	handler.ServeHTTP(projectRecorder, projectRequest)
	require.Equal(t, http.StatusOK, projectRecorder.Code)

	expiresAt := time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)
	shareBody := `{"collaborator_subject_sub":"collab-sub","permission":"view","expires_at":"` + expiresAt + `"}`
	shareRequest := httptest.NewRequest(http.MethodPost, "/v1/subjects/owner-sub/services/"+serverTestServiceID+"/projects/work/shares", strings.NewReader(shareBody))
	shareRequest.Header.Set("Authorization", "Bearer test-admin-token")
	shareRecorder := httptest.NewRecorder()
	handler.ServeHTTP(shareRecorder, shareRequest)
	require.Equal(t, http.StatusCreated, shareRecorder.Code)

	var sharePayload map[string]any
	require.NoError(t, json.Unmarshal(shareRecorder.Body.Bytes(), &sharePayload))
	shareID := sharePayload["share_id"].(string)
	parsedShareID, err := ids.Parse(shareID)
	require.NoError(t, err)
	_, err = store.db.ExecContext(ctx, `UPDATE memory_bank_project_shares SET expires_at = ? WHERE share_id = ?`, time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano), parsedShareID.Bytes())
	require.NoError(t, err)

	listRequest := httptest.NewRequest(http.MethodGet, "/v1/subjects/collab-sub/services/"+serverTestServiceID+"/shares", nil)
	listRequest.Header.Set("Authorization", "Bearer test-admin-token")
	listRecorder := httptest.NewRecorder()
	handler.ServeHTTP(listRecorder, listRequest)
	require.Equal(t, http.StatusOK, listRecorder.Code)
	require.NotContains(t, listRecorder.Body.String(), shareID)

	includeInactiveRequest := httptest.NewRequest(http.MethodGet, "/v1/subjects/collab-sub/services/"+serverTestServiceID+"/shares?include_inactive=true", nil)
	includeInactiveRequest.Header.Set("Authorization", "Bearer test-admin-token")
	includeInactiveRecorder := httptest.NewRecorder()
	handler.ServeHTTP(includeInactiveRecorder, includeInactiveRequest)
	require.Equal(t, http.StatusOK, includeInactiveRecorder.Code)
	require.Contains(t, includeInactiveRecorder.Body.String(), shareID)

	acceptRequest := httptest.NewRequest(http.MethodPut, "/v1/subjects/collab-sub/services/"+serverTestServiceID+"/shares/"+shareID+"/accept", nil)
	acceptRequest.Header.Set("Authorization", "Bearer test-admin-token")
	acceptRecorder := httptest.NewRecorder()
	handler.ServeHTTP(acceptRecorder, acceptRequest)
	require.Equal(t, http.StatusNotFound, acceptRecorder.Code)

	replacementRequest := httptest.NewRequest(http.MethodPost, "/v1/subjects/owner-sub/services/"+serverTestServiceID+"/projects/work/shares", strings.NewReader(`{"collaborator_subject_sub":"collab-sub","permission":"edit"}`))
	replacementRequest.Header.Set("Authorization", "Bearer test-admin-token")
	replacementRecorder := httptest.NewRecorder()
	handler.ServeHTTP(replacementRecorder, replacementRequest)
	require.Equal(t, http.StatusCreated, replacementRecorder.Code)
	require.Contains(t, replacementRecorder.Body.String(), `"permission":"edit"`)
}

func TestMemoryBankAdminRejectsInvalidMetadataAndPastShareExpiry(t *testing.T) {
	ctx := context.Background()
	store, err := NewStore(ctx, "file:"+filepath.Join(t.TempDir(), "mcp-platform.db"), zerolog.New(io.Discard))
	require.NoError(t, err)
	defer store.Close()
	require.NoError(t, store.RunMigrations(ctx))
	require.NoError(t, store.SeedServiceCatalog(ctx))
	registerServerTestService(t, ctx, store, serverTestServiceID)
	createStaticTenantForServerTest(t, ctx, store, domain.Subject{Sub: "owner-sub", SubjectKey: "owner-key"}, serverTestServiceID)

	tokenPath := filepath.Join(t.TempDir(), "admin-token")
	require.NoError(t, os.WriteFile(tokenPath, []byte("test-admin-token"), 0o600))
	app := &App{cfg: Config{AdminTokenPath: tokenPath}, store: store, health: &healthState{}}
	handler := app.Handler()

	invalidProjectRequest := httptest.NewRequest(http.MethodPut, "/v1/subjects/owner-sub/services/"+serverTestServiceID+"/projects/work", strings.NewReader(`{"display_name":"Work Notes","root_path":"/projects/work","metadata":{`))
	invalidProjectRequest.Header.Set("Authorization", "Bearer test-admin-token")
	invalidProjectRecorder := httptest.NewRecorder()
	handler.ServeHTTP(invalidProjectRecorder, invalidProjectRequest)
	require.Equal(t, http.StatusBadRequest, invalidProjectRecorder.Code)
	require.Contains(t, invalidProjectRecorder.Body.String(), `"error":"invalid_json"`)

	projectRequest := httptest.NewRequest(http.MethodPut, "/v1/subjects/owner-sub/services/"+serverTestServiceID+"/projects/work", strings.NewReader(`{"display_name":"Work Notes","root_path":"/projects/work"}`))
	projectRequest.Header.Set("Authorization", "Bearer test-admin-token")
	projectRecorder := httptest.NewRecorder()
	handler.ServeHTTP(projectRecorder, projectRequest)
	require.Equal(t, http.StatusOK, projectRecorder.Code)

	expiresAt := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano)
	shareRequest := httptest.NewRequest(http.MethodPost, "/v1/subjects/owner-sub/services/"+serverTestServiceID+"/projects/work/shares", strings.NewReader(`{"collaborator_subject_sub":"collab-sub","permission":"view","expires_at":"`+expiresAt+`"}`))
	shareRequest.Header.Set("Authorization", "Bearer test-admin-token")
	shareRecorder := httptest.NewRecorder()
	handler.ServeHTTP(shareRecorder, shareRequest)
	require.Equal(t, http.StatusBadRequest, shareRecorder.Code)
	require.Contains(t, shareRecorder.Body.String(), `"error":"share_expires_at_must_be_future"`)
}

func TestMemoryBankAdminRejectsAndHidesArchivedProjectShares(t *testing.T) {
	ctx := context.Background()
	store, err := NewStore(ctx, "file:"+filepath.Join(t.TempDir(), "mcp-platform.db"), zerolog.New(io.Discard))
	require.NoError(t, err)
	defer store.Close()
	require.NoError(t, store.RunMigrations(ctx))
	require.NoError(t, store.SeedServiceCatalog(ctx))
	registerServerTestService(t, ctx, store, serverTestServiceID)
	createStaticTenantForServerTest(t, ctx, store, domain.Subject{Sub: "owner-sub", SubjectKey: "owner-key"}, serverTestServiceID)

	tokenPath := filepath.Join(t.TempDir(), "admin-token")
	require.NoError(t, os.WriteFile(tokenPath, []byte("test-admin-token"), 0o600))
	app := &App{cfg: Config{AdminTokenPath: tokenPath}, store: store, health: &healthState{}}
	handler := app.Handler()

	projectRequest := httptest.NewRequest(http.MethodPut, "/v1/subjects/owner-sub/services/"+serverTestServiceID+"/projects/work", strings.NewReader(`{"display_name":"Work Notes","root_path":"/projects/work"}`))
	projectRequest.Header.Set("Authorization", "Bearer test-admin-token")
	projectRecorder := httptest.NewRecorder()
	handler.ServeHTTP(projectRecorder, projectRequest)
	require.Equal(t, http.StatusOK, projectRecorder.Code)

	shareRequest := httptest.NewRequest(http.MethodPost, "/v1/subjects/owner-sub/services/"+serverTestServiceID+"/projects/work/shares", strings.NewReader(`{"collaborator_subject_sub":"collab-sub","permission":"view"}`))
	shareRequest.Header.Set("Authorization", "Bearer test-admin-token")
	shareRecorder := httptest.NewRecorder()
	handler.ServeHTTP(shareRecorder, shareRequest)
	require.Equal(t, http.StatusCreated, shareRecorder.Code)

	var sharePayload map[string]any
	require.NoError(t, json.Unmarshal(shareRecorder.Body.Bytes(), &sharePayload))
	shareID := sharePayload["share_id"].(string)

	archivedAt := time.Now().UTC().Format(time.RFC3339Nano)
	archiveBody := `{"display_name":"Work Notes","root_path":"/projects/work","archived_at":"` + archivedAt + `"}`
	archiveRequest := httptest.NewRequest(http.MethodPut, "/v1/subjects/owner-sub/services/"+serverTestServiceID+"/projects/work", strings.NewReader(archiveBody))
	archiveRequest.Header.Set("Authorization", "Bearer test-admin-token")
	archiveRecorder := httptest.NewRecorder()
	handler.ServeHTTP(archiveRecorder, archiveRequest)
	require.Equal(t, http.StatusOK, archiveRecorder.Code)

	listRequest := httptest.NewRequest(http.MethodGet, "/v1/subjects/collab-sub/services/"+serverTestServiceID+"/shares", nil)
	listRequest.Header.Set("Authorization", "Bearer test-admin-token")
	listRecorder := httptest.NewRecorder()
	handler.ServeHTTP(listRecorder, listRequest)
	require.Equal(t, http.StatusOK, listRecorder.Code)
	require.NotContains(t, listRecorder.Body.String(), shareID)

	acceptRequest := httptest.NewRequest(http.MethodPut, "/v1/subjects/collab-sub/services/"+serverTestServiceID+"/shares/"+shareID+"/accept", nil)
	acceptRequest.Header.Set("Authorization", "Bearer test-admin-token")
	acceptRecorder := httptest.NewRecorder()
	handler.ServeHTTP(acceptRecorder, acceptRequest)
	require.Equal(t, http.StatusNotFound, acceptRecorder.Code)

	newShareRequest := httptest.NewRequest(http.MethodPost, "/v1/subjects/owner-sub/services/"+serverTestServiceID+"/projects/work/shares", strings.NewReader(`{"collaborator_subject_sub":"other-sub","permission":"view"}`))
	newShareRequest.Header.Set("Authorization", "Bearer test-admin-token")
	newShareRecorder := httptest.NewRecorder()
	handler.ServeHTTP(newShareRecorder, newShareRequest)
	require.Equal(t, http.StatusConflict, newShareRecorder.Code)
	require.Contains(t, newShareRecorder.Body.String(), `"error":"project_archived"`)
}

func TestMemoryBankAdminAllowsRegisteredProjectService(t *testing.T) {
	ctx := context.Background()
	store, err := NewStore(ctx, "file:"+filepath.Join(t.TempDir(), "mcp-platform.db"), zerolog.New(io.Discard))
	require.NoError(t, err)
	defer store.Close()
	require.NoError(t, store.RunMigrations(ctx))
	require.NoError(t, store.SeedServiceCatalog(ctx))
	registerServerTestService(t, ctx, store, serverTestServiceID)
	createStaticTenantForServerTest(t, ctx, store, domain.Subject{Sub: "owner-sub", SubjectKey: "owner-key"}, serverTestServiceID)

	tokenPath := filepath.Join(t.TempDir(), "admin-token")
	require.NoError(t, os.WriteFile(tokenPath, []byte("test-admin-token"), 0o600))
	app := &App{cfg: Config{AdminTokenPath: tokenPath}, store: store, health: &healthState{}}

	request := httptest.NewRequest(http.MethodGet, "/v1/subjects/owner-sub/services/"+serverTestServiceID+"/projects", nil)
	request.Header.Set("Authorization", "Bearer test-admin-token")
	recorder := httptest.NewRecorder()
	app.Handler().ServeHTTP(recorder, request)

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Contains(t, recorder.Body.String(), `"projects"`)
}

func TestMemoryBankAdminRequiresBearerToken(t *testing.T) {
	tokenPath := filepath.Join(t.TempDir(), "admin-token")
	require.NoError(t, os.WriteFile(tokenPath, []byte("test-admin-token"), 0o600))
	app := &App{cfg: Config{AdminTokenPath: tokenPath}, health: &healthState{}}

	request := httptest.NewRequest(http.MethodGet, "/v1/subjects/owner-sub/services/"+serverTestServiceID+"/projects", nil)
	recorder := httptest.NewRecorder()
	app.Handler().ServeHTTP(recorder, request)

	require.Equal(t, http.StatusUnauthorized, recorder.Code)
}

func TestNormalizeStaticUpstreamURLRejectsHostnameResolvingToLoopback(t *testing.T) {
	previousLookup := lookupStaticUpstreamIP
	lookupStaticUpstreamIP = func(host string) ([]net.IP, error) {
		require.Equal(t, "loopback.test", host)
		return []net.IP{net.ParseIP("127.0.0.1")}, nil
	}
	t.Cleanup(func() { lookupStaticUpstreamIP = previousLookup })

	_, err := normalizeStaticUpstreamURL("http://loopback.test:8080")
	require.ErrorContains(t, err, "resolved ip address is not allowed")
}

func TestHandleReadinessRequiresLeadership(t *testing.T) {
	t.Parallel()

	app := &App{
		cfg:    validTenantRuntimeControlPlaneConfig(),
		health: &healthState{},
	}
	app.health.setDatabaseStatus(nil)
	app.health.setReconcileResult(ReconcileSummary{LastRunAt: time.Now().UTC()}, nil)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/health/ready", nil)
	app.handleReadiness(recorder, request)

	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)

	var payload map[string]any
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &payload))
	require.Equal(t, "not_ready", payload["status"])
	require.Equal(t, false, payload["leader"])
	require.Equal(t, true, payload["dependencies_configured"])
	require.Equal(t, true, payload["tenant_runtime_configured"])
}
