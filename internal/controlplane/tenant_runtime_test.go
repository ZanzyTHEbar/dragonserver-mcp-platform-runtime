package controlplane

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"dragonserver/mcp-platform/internal/catalog"
	"dragonserver/mcp-platform/internal/domain"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

func TestShouldRequeueDelete(t *testing.T) {
	t.Parallel()

	require.True(t, shouldRequeueDelete(nil))

	recent := time.Now().UTC().Add(-(deleteRequeueInterval / 2))
	require.False(t, shouldRequeueDelete(&recent))

	stale := time.Now().UTC().Add(-(deleteRequeueInterval + time.Second))
	require.True(t, shouldRequeueDelete(&stale))
}

func TestProbeTenantHealthUsesStaticUpstreamURL(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/mcp", r.URL.Path)
		_, _ = w.Write([]byte(`{"transport":"streamable"}`))
	}))
	defer upstream.Close()

	service := catalog.ServiceCatalogEntry{ServiceID: "example-mcp", HealthPath: "/mcp"}
	runtime := &CoolifyTenantRuntime{healthClient: upstream.Client()}
	healthy, detail, err := runtime.probeTenantHealth(context.Background(), TenantInstance{ServiceID: service.ServiceID, SubjectKey: "subject-a"}, service, "", upstream.URL+"/mcp")
	require.NoError(t, err)
	require.True(t, healthy)
	require.Equal(t, "example-mcp returned healthy status 200", detail)
}

func TestStaticUpstreamRuntimeObservesDynamicServiceWithoutCoolifyProvisioning(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, err := NewStore(ctx, "file:"+filepath.Join(t.TempDir(), "mcp-platform.db"), zerolog.New(io.Discard))
	require.NoError(t, err)
	defer store.Close()
	require.NoError(t, store.RunMigrations(ctx))
	require.NoError(t, store.SeedServiceCatalog(ctx))

	service := catalog.ServiceCatalogEntry{
		ServiceID:              "custom",
		DisplayName:            "Custom",
		UpstreamServiceName:    "custom-mcp",
		TransportType:          catalog.TransportTypeStreamableHTTP,
		InternalPort:           8080,
		PublicPath:             "/custom/mcp",
		InternalUpstreamPath:   "/mcp",
		HealthPath:             "/health",
		HealthProbeExpectation: "GET returns OK",
		ResourceProfile:        "small",
		PersistencePolicy:      "stateless",
		AdapterRequirement:     catalog.AdapterRequirementNone,
	}
	require.NoError(t, store.UpsertAdminServiceCatalogEntry(ctx, service))
	require.NoError(t, store.UpsertManualServiceGrant(ctx, domain.Subject{Sub: "subject-sub", SubjectKey: "subject-key"}, service.ServiceID))

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/health", r.URL.Path)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	require.NoError(t, store.UpsertStaticTenantUpstream(ctx, domain.Subject{Sub: "subject-sub", SubjectKey: "subject-key"}, service.ServiceID, upstream.URL+"/mcp", time.Now().UTC()))
	_, err = store.db.ExecContext(ctx, `UPDATE tenant_instances SET runtime_state = 'degraded', last_error = 'previous failure' WHERE subject_sub = 'subject-sub' AND service_id = 'custom'`)
	require.NoError(t, err)

	tenants, err := store.ListTenantInstances(ctx)
	require.NoError(t, err)
	require.Len(t, tenants, 1)
	require.Equal(t, domain.TenantRuntimeStateDegraded, tenants[0].RuntimeState)

	runtime := NewCoolifyTenantRuntime(Config{}, store, &DependencyClients{}, zerolog.New(io.Discard))
	result, err := runtime.Apply(ctx, tenants[0], TenantPlan{Action: ReconcileActionEnsure})
	require.NoError(t, err)
	require.Equal(t, "ready", result.Status)
	require.Equal(t, domain.TenantRuntimeStateReady, result.ObservedState)

	tenants, err = store.ListTenantInstances(ctx)
	require.NoError(t, err)
	require.Equal(t, domain.TenantRuntimeStateReady, tenants[0].RuntimeState)
	require.Equal(t, "trusted", tenants[0].AttestationState)
	require.NotNil(t, tenants[0].LastAttestedAt)
	require.False(t, tenants[0].LastAttestationID.IsZero())

	specs, err := store.ListTenantRuntimeSpecsForTenant(ctx, tenants[0].TenantID)
	require.NoError(t, err)
	require.Len(t, specs, 1)
	require.Equal(t, "observe-v1", specs[0].SpecVersion)
	require.Equal(t, service.ServiceID, specs[0].ServiceID)
	require.Equal(t, tenants[0].SubjectSub, specs[0].SubjectSub)

	measurements, err := store.ListTenantRuntimeMeasurementsForTenant(ctx, tenants[0].TenantID)
	require.NoError(t, err)
	require.Len(t, measurements, 1)
	require.Equal(t, "static_upstream", measurements[0].Source)
	require.Equal(t, "healthy", measurements[0].HealthStatus)

	latest, err := store.GetLatestTenantRuntimeAttestation(ctx, tenants[0].TenantID)
	require.NoError(t, err)
	require.Equal(t, "trusted", latest.Verdict)
	require.Equal(t, specs[0].SpecID, latest.SpecID)
	require.Equal(t, measurements[0].MeasurementID, latest.MeasurementID)
}

func TestStaticUpstreamRuntimeDeletesAfterCatalogServiceDisabled(t *testing.T) {
	t.Parallel()

	runtime := NewCoolifyTenantRuntime(Config{}, nil, &DependencyClients{}, zerolog.New(io.Discard))
	result, err := runtime.Apply(context.Background(), TenantInstance{
		ServiceID:    "disabled-dynamic",
		DesiredState: domain.TenantDesiredStateDeleted,
		RuntimeState: domain.TenantRuntimeStateReady,
		UpstreamURL:  "http://mcp.example:8080",
		SubjectSub:   "subject-sub",
		SubjectKey:   "subject-key",
		Metadata:     []byte(`{"runtime_mode":"static_upstream"}`),
	}, TenantPlan{Action: ReconcileActionDelete})
	require.NoError(t, err)
	require.Equal(t, "deleted", result.Status)
	require.True(t, result.DeleteCompleted)
}
