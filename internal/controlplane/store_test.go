package controlplane

import (
	"context"
	"io"
	"path/filepath"
	"testing"
	"time"

	"dragonserver/mcp-platform/internal/catalog"
	"dragonserver/mcp-platform/internal/domain"
	"dragonserver/mcp-platform/internal/ids"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

const storeTestServiceID = "example-mcp"

func registerStoreTestService(t *testing.T, ctx context.Context, store *Store, serviceID string) {
	t.Helper()
	require.NoError(t, store.UpsertAdminServiceCatalogEntry(ctx, catalog.ServiceCatalogEntry{
		ServiceID:              serviceID,
		DisplayName:            "Example MCP",
		UpstreamServiceName:    serviceID,
		TransportType:          catalog.TransportTypeStreamableHTTP,
		InternalPort:           8080,
		PublicPath:             "/" + serviceID + "/mcp",
		InternalUpstreamPath:   "/mcp",
		HealthPath:             "/health",
		HealthProbeExpectation: "GET returns 2xx",
		ResourceProfile:        "small",
		PersistencePolicy:      "stateless",
		AdapterRequirement:     catalog.AdapterRequirementNone,
	}))
}

func registerBuiltinStoreTestService(t *testing.T, ctx context.Context, store *Store, serviceID string) {
	t.Helper()
	registerStoreTestService(t, ctx, store, serviceID)
	_, err := store.db.ExecContext(ctx, `UPDATE service_catalog SET source = 'builtin' WHERE service_id = ?`, serviceID)
	require.NoError(t, err)
}

func TestControlPlaneLockUsesDatabaseBackedLease(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	databaseURL := "file:" + filepath.Join(t.TempDir(), "mcp-platform.db")
	logger := zerolog.New(io.Discard)

	storeA, err := NewStore(ctx, databaseURL, logger)
	require.NoError(t, err)
	defer storeA.Close()
	require.NoError(t, storeA.RunMigrations(ctx))

	storeB, err := NewStore(ctx, databaseURL, logger)
	require.NoError(t, err)
	defer storeB.Close()

	lockA, err := storeA.AcquireControlPlaneLock(ctx)
	require.NoError(t, err)
	require.True(t, lockA.Held(ctx))

	lockB, err := storeB.AcquireControlPlaneLock(ctx)
	require.Error(t, err)
	require.Nil(t, lockB)

	require.NoError(t, lockA.Release(ctx))
	require.False(t, lockA.Held(ctx))

	lockB, err = storeB.AcquireControlPlaneLock(ctx)
	require.NoError(t, err)
	require.True(t, lockB.Held(ctx))
	require.NoError(t, lockB.Release(ctx))
}

func TestNewAppRunsMigrationsBeforeAcquiringControlPlaneLock(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	app, err := NewApp(ctx, Config{
		DatabaseURL:         "file:" + filepath.Join(t.TempDir(), "mcp-platform.db"),
		HTTPBindAddr:        "127.0.0.1:0",
		ReconcileInterval:   30 * time.Second,
		HealthcheckInterval: 30 * time.Second,
	}, zerolog.New(io.Discard))
	require.NoError(t, err)
	defer app.Close()
	require.True(t, app.lock.Held(ctx))
}

func TestUpdateTenantRuntimeStatusCanClearRuntimeReferences(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, err := NewStore(ctx, "file:"+filepath.Join(t.TempDir(), "mcp-platform.db"), zerolog.New(io.Discard))
	require.NoError(t, err)
	defer store.Close()
	require.NoError(t, store.RunMigrations(ctx))
	require.NoError(t, store.SeedServiceCatalog(ctx))
	registerStoreTestService(t, ctx, store, storeTestServiceID)
	registerStoreTestService(t, ctx, store, storeTestServiceID)

	tenantID := ids.New()
	_, err = store.db.ExecContext(ctx, `
INSERT INTO subjects (subject_sub, subject_key, preferred_username, email, display_name, last_synced_at)
VALUES ('user-sub', 'subject-key', 'user', 'user@example.com', 'User', CURRENT_TIMESTAMP);`)
	require.NoError(t, err)
	_, err = store.db.ExecContext(ctx, `
INSERT INTO tenant_instances (
    tenant_id,
    subject_sub,
    service_id,
    subject_key,
    tenant_instance_name,
    internal_dns_name,
    desired_state,
    runtime_state,
    coolify_resource_id,
    coolify_application_id,
    upstream_url,
    last_healthy_at
)
VALUES (?, 'user-sub', ?, 'subject-key', 'u-subject-key-example', 'u-subject-key-example', 'enabled', 'ready', 'service-uuid', 'app-uuid', 'http://example:9000', CURRENT_TIMESTAMP);`, tenantID.Bytes(), storeTestServiceID)
	require.NoError(t, err)

	require.NoError(t, store.UpdateTenantRuntimeStatus(ctx, TenantRuntimeUpdate{
		TenantID:               tenantID,
		RuntimeState:           domain.TenantRuntimeStateDegraded,
		ClearRuntimeReferences: true,
		LastError:              "service disappeared",
	}))

	tenants, err := store.ListTenantInstances(ctx)
	require.NoError(t, err)
	require.Len(t, tenants, 1)
	require.Empty(t, tenants[0].CoolifyResourceID)
	require.Empty(t, tenants[0].CoolifyApplicationID)
	require.Empty(t, tenants[0].UpstreamURL)
	require.Nil(t, tenants[0].LastHealthyAt)
	require.Equal(t, domain.TenantRuntimeStateDegraded, tenants[0].RuntimeState)
}

func TestUpsertStaticTenantUpstreamUsesPersistedSubjectKey(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, err := NewStore(ctx, "file:"+filepath.Join(t.TempDir(), "mcp-platform.db"), zerolog.New(io.Discard))
	require.NoError(t, err)
	defer store.Close()
	require.NoError(t, store.RunMigrations(ctx))
	require.NoError(t, store.SeedServiceCatalog(ctx))
	registerStoreTestService(t, ctx, store, storeTestServiceID)

	subject := domain.Subject{Sub: "user-sub", SubjectKey: "persisted-key", PreferredUsername: "user"}
	require.NoError(t, store.UpsertSubject(ctx, subject))
	require.NoError(t, store.UpsertManualServiceGrant(ctx, domain.Subject{Sub: subject.Sub}, storeTestServiceID))

	require.NoError(t, store.UpsertStaticTenantUpstream(ctx, domain.Subject{Sub: subject.Sub, SubjectKey: "operator-provided-wrong-key"}, storeTestServiceID, "https://mcp.lan", time.Now().UTC()))

	tenants, err := store.ListTenantInstances(ctx)
	require.NoError(t, err)
	require.Len(t, tenants, 1)
	require.Equal(t, "persisted-key", tenants[0].SubjectKey)
	require.Equal(t, domain.BuildTenantInstanceName(storeTestServiceID, "persisted-key"), tenants[0].TenantInstanceName)
}

func TestSuspendAndResumeTenantInstancePreservesRuntimeReferences(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, err := NewStore(ctx, "file:"+filepath.Join(t.TempDir(), "mcp-platform.db"), zerolog.New(io.Discard))
	require.NoError(t, err)
	defer store.Close()
	require.NoError(t, store.RunMigrations(ctx))
	require.NoError(t, store.SeedServiceCatalog(ctx))
	registerStoreTestService(t, ctx, store, storeTestServiceID)

	subject := domain.Subject{Sub: "user-sub", SubjectKey: "persisted-key", PreferredUsername: "user"}
	require.NoError(t, store.UpsertManualServiceGrant(ctx, subject, storeTestServiceID))
	require.NoError(t, store.UpsertStaticTenantUpstream(ctx, subject, storeTestServiceID, "http://example-mcp:8080/mcp", time.Now().UTC()))

	tenants, err := store.ListTenantInstances(ctx)
	require.NoError(t, err)
	require.Len(t, tenants, 1)
	tenantID := tenants[0].TenantID
	require.NoError(t, store.UpdateTenantRuntimeStatus(ctx, TenantRuntimeUpdate{
		TenantID:          tenantID,
		RuntimeState:      domain.TenantRuntimeStateReady,
		CoolifyResourceID: "service-uuid",
		UpstreamURL:       "http://example-mcp:8080/mcp",
	}))

	require.NoError(t, store.SuspendTenantInstance(ctx, subject.Sub, storeTestServiceID))
	require.NoError(t, store.ReconcileDesiredTenants(ctx))

	tenants, err = store.ListTenantInstances(ctx)
	require.NoError(t, err)
	require.Len(t, tenants, 1)
	require.Equal(t, domain.TenantDesiredStateDisabled, tenants[0].DesiredState)
	require.Equal(t, "service-uuid", tenants[0].CoolifyResourceID)
	require.Equal(t, "http://example-mcp:8080/mcp", tenants[0].UpstreamURL)

	require.NoError(t, store.ResumeTenantInstance(ctx, subject.Sub, storeTestServiceID))
	tenants, err = store.ListTenantInstances(ctx)
	require.NoError(t, err)
	require.Len(t, tenants, 1)
	require.Equal(t, domain.TenantDesiredStateEnabled, tenants[0].DesiredState)
	require.Equal(t, "service-uuid", tenants[0].CoolifyResourceID)
}

func TestDeletedTenantIsTerminalForDesiredReconcileAndLifecycle(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, err := NewStore(ctx, "file:"+filepath.Join(t.TempDir(), "mcp-platform.db"), zerolog.New(io.Discard))
	require.NoError(t, err)
	defer store.Close()
	require.NoError(t, store.RunMigrations(ctx))
	require.NoError(t, store.SeedServiceCatalog(ctx))
	registerStoreTestService(t, ctx, store, storeTestServiceID)

	subject := domain.Subject{Sub: "user-sub", SubjectKey: "persisted-key", PreferredUsername: "user"}
	require.NoError(t, store.UpsertManualServiceGrant(ctx, subject, storeTestServiceID))
	require.NoError(t, store.UpsertStaticTenantUpstream(ctx, subject, storeTestServiceID, "http://example-mcp:8080/mcp", time.Now().UTC()))

	tenants, err := store.ListTenantInstances(ctx)
	require.NoError(t, err)
	require.Len(t, tenants, 1)
	_, err = store.db.ExecContext(ctx, `UPDATE tenant_instances SET desired_state = 'deleted' WHERE tenant_id = ?`, tenants[0].TenantID.Bytes())
	require.NoError(t, err)

	require.NoError(t, store.ReconcileDesiredTenants(ctx))
	tenants, err = store.ListTenantInstances(ctx)
	require.NoError(t, err)
	require.Len(t, tenants, 1)
	require.Equal(t, domain.TenantDesiredStateDeleted, tenants[0].DesiredState)

	require.ErrorIs(t, store.SuspendTenantInstance(ctx, subject.Sub, storeTestServiceID), ErrTenantInstanceNotFound)
	require.ErrorIs(t, store.ResumeTenantInstance(ctx, subject.Sub, storeTestServiceID), ErrTenantInstanceNotFound)

	tenants, err = store.ListTenantInstances(ctx)
	require.NoError(t, err)
	require.Len(t, tenants, 1)
	require.Equal(t, domain.TenantDesiredStateDeleted, tenants[0].DesiredState)
}

func TestResumeTenantInstanceRequiresGrant(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, err := NewStore(ctx, "file:"+filepath.Join(t.TempDir(), "mcp-platform.db"), zerolog.New(io.Discard))
	require.NoError(t, err)
	defer store.Close()
	require.NoError(t, store.RunMigrations(ctx))
	require.NoError(t, store.SeedServiceCatalog(ctx))

	err = store.ResumeTenantInstance(ctx, "user-sub", storeTestServiceID)
	require.ErrorIs(t, err, ErrSubjectServiceGrantNotFound)
}

func TestMemoryBankProjectShareLifecycle(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, err := NewStore(ctx, "file:"+filepath.Join(t.TempDir(), "mcp-platform.db"), zerolog.New(io.Discard))
	require.NoError(t, err)
	defer store.Close()
	require.NoError(t, store.RunMigrations(ctx))
	require.NoError(t, store.SeedServiceCatalog(ctx))
	registerStoreTestService(t, ctx, store, storeTestServiceID)

	owner := domain.Subject{Sub: "owner-sub", SubjectKey: "owner-key", PreferredUsername: "owner"}
	collaborator := domain.Subject{Sub: "collaborator-sub", SubjectKey: "collab-key", PreferredUsername: "collab"}
	require.NoError(t, store.UpsertManualServiceGrant(ctx, owner, storeTestServiceID))
	require.NoError(t, store.UpsertSubject(ctx, collaborator))
	require.NoError(t, store.UpsertStaticTenantUpstream(ctx, owner, storeTestServiceID, "http://example-mcp:8080/mcp", time.Now().UTC()))

	tenants, err := store.ListTenantInstances(ctx)
	require.NoError(t, err)
	require.Len(t, tenants, 1)

	projectID, err := store.UpsertMemoryBankProject(ctx, MemoryBankProject{
		OwnerSubjectSub: owner.Sub,
		OwnerTenantID:   tenants[0].TenantID,
		ServiceID:       storeTestServiceID,
		ProjectKey:      "ops-notes",
		DisplayName:     "Ops Notes",
		RootPath:        "/data/memory-bank/ops-notes",
	})
	require.NoError(t, err)
	require.False(t, projectID.IsZero())

	_, err = store.UpsertMemoryBankProject(ctx, MemoryBankProject{
		OwnerSubjectSub: collaborator.Sub,
		OwnerTenantID:   tenants[0].TenantID,
		ServiceID:       storeTestServiceID,
		ProjectKey:      "wrong-owner",
		DisplayName:     "Wrong Owner",
		RootPath:        "/data/memory-bank/wrong-owner",
	})
	require.ErrorContains(t, err, "owner tenant mismatch")

	updatedProjectID, err := store.UpsertMemoryBankProject(ctx, MemoryBankProject{
		OwnerSubjectSub: owner.Sub,
		OwnerTenantID:   tenants[0].TenantID,
		ServiceID:       storeTestServiceID,
		ProjectKey:      "ops-notes",
		DisplayName:     "Ops Notes Updated",
		RootPath:        "/data/memory-bank/ops-notes-renamed",
	})
	require.NoError(t, err)
	require.Equal(t, projectID, updatedProjectID)

	_, err = store.UpsertMemoryBankProject(ctx, MemoryBankProject{
		OwnerSubjectSub: owner.Sub,
		OwnerTenantID:   tenants[0].TenantID,
		ServiceID:       storeTestServiceID,
		ProjectKey:      "invalid-metadata",
		DisplayName:     "Invalid Metadata",
		RootPath:        "/data/memory-bank/invalid-metadata",
		Metadata:        []byte(`{`),
	})
	require.ErrorIs(t, err, ErrMemoryBankInvalidMetadata)

	projects, err := store.ListMemoryBankProjectsByOwner(ctx, owner.Sub, storeTestServiceID, false)
	require.NoError(t, err)
	require.Len(t, projects, 1)
	require.Equal(t, "ops-notes", projects[0].ProjectKey)
	require.Equal(t, "/data/memory-bank/ops-notes-renamed", projects[0].RootPath)
	require.JSONEq(t, `{}`, string(projects[0].Metadata))

	_, err = store.CreateMemoryBankProjectShare(ctx, MemoryBankProjectShare{
		ProjectID:              projectID,
		OwnerSubjectSub:        collaborator.Sub,
		CollaboratorSubjectSub: collaborator.Sub,
		Permission:             "view",
		CreatedBySubjectSub:    owner.Sub,
	})
	require.Error(t, err)

	shareID, err := store.CreateMemoryBankProjectShare(ctx, MemoryBankProjectShare{
		ProjectID:              projectID,
		OwnerSubjectSub:        owner.Sub,
		CollaboratorSubjectSub: collaborator.Sub,
		Permission:             "view",
		CreatedBySubjectSub:    owner.Sub,
	})
	require.NoError(t, err)
	require.False(t, shareID.IsZero())

	pastExpiry := time.Now().UTC().Add(-time.Minute)
	_, err = store.CreateMemoryBankProjectShare(ctx, MemoryBankProjectShare{
		ProjectID:              projectID,
		OwnerSubjectSub:        owner.Sub,
		CollaboratorSubjectSub: "expired-collab",
		Permission:             "view",
		CreatedBySubjectSub:    owner.Sub,
		ExpiresAt:              &pastExpiry,
	})
	require.ErrorIs(t, err, ErrMemoryBankShareExpired)

	_, err = store.CreateMemoryBankProjectShare(ctx, MemoryBankProjectShare{
		ProjectID:              projectID,
		OwnerSubjectSub:        owner.Sub,
		CollaboratorSubjectSub: "invalid-metadata-collab",
		Permission:             "view",
		CreatedBySubjectSub:    owner.Sub,
		Metadata:               []byte(`{`),
	})
	require.ErrorIs(t, err, ErrMemoryBankInvalidMetadata)

	shares, err := store.ListMemoryBankProjectSharesForSubject(ctx, collaborator.Sub, false)
	require.NoError(t, err)
	require.Len(t, shares, 1)
	require.Equal(t, "pending", shares[0].State)
	require.Equal(t, "oauth_consent", shares[0].Source)

	acceptedAt := time.Now().UTC()
	activated, err := store.ActivateMemoryBankProjectShare(ctx, shareID, collaborator.Sub, acceptedAt)
	require.NoError(t, err)
	require.True(t, activated)

	share, err := store.GetMemoryBankProjectShare(ctx, shareID)
	require.NoError(t, err)
	require.Equal(t, "active", share.State)
	require.NotNil(t, share.AcceptedAt)

	revoked, err := store.RevokeMemoryBankProjectShare(ctx, shareID, time.Now().UTC())
	require.NoError(t, err)
	require.True(t, revoked)

	shares, err = store.ListMemoryBankProjectSharesForSubject(ctx, collaborator.Sub, false)
	require.NoError(t, err)
	require.Empty(t, shares)

	archivedAt := time.Now().UTC()
	_, err = store.UpsertMemoryBankProject(ctx, MemoryBankProject{
		OwnerSubjectSub: owner.Sub,
		OwnerTenantID:   tenants[0].TenantID,
		ServiceID:       storeTestServiceID,
		ProjectKey:      "ops-notes",
		DisplayName:     "Ops Notes Archived",
		RootPath:        "/data/memory-bank/ops-notes-archived",
		ArchivedAt:      &archivedAt,
	})
	require.NoError(t, err)
	_, err = store.UpsertMemoryBankProject(ctx, MemoryBankProject{
		OwnerSubjectSub: owner.Sub,
		OwnerTenantID:   tenants[0].TenantID,
		ServiceID:       storeTestServiceID,
		ProjectKey:      "ops-notes",
		DisplayName:     "Ops Notes Still Archived",
		RootPath:        "/data/memory-bank/ops-notes-still-archived",
	})
	require.NoError(t, err)
	project, err := store.GetMemoryBankProject(ctx, projectID)
	require.NoError(t, err)
	require.NotNil(t, project.ArchivedAt)

	_, err = store.CreateMemoryBankProjectShare(ctx, MemoryBankProjectShare{
		ProjectID:              projectID,
		OwnerSubjectSub:        owner.Sub,
		CollaboratorSubjectSub: collaborator.Sub,
		Permission:             "view",
		CreatedBySubjectSub:    owner.Sub,
	})
	require.ErrorIs(t, err, ErrMemoryBankProjectNotShareable)

	shares, err = store.ListMemoryBankProjectSharesForSubject(ctx, collaborator.Sub, false)
	require.NoError(t, err)
	require.Empty(t, shares)
}

func TestTenantRuntimeAttestationLifecycle(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, err := NewStore(ctx, "file:"+filepath.Join(t.TempDir(), "mcp-platform.db"), zerolog.New(io.Discard))
	require.NoError(t, err)
	defer store.Close()
	require.NoError(t, store.RunMigrations(ctx))
	require.NoError(t, store.SeedServiceCatalog(ctx))
	registerStoreTestService(t, ctx, store, storeTestServiceID)

	subject := domain.Subject{Sub: "attested-sub", SubjectKey: "attested-key", PreferredUsername: "attested"}
	require.NoError(t, store.UpsertManualServiceGrant(ctx, subject, storeTestServiceID))
	require.NoError(t, store.UpsertStaticTenantUpstream(ctx, subject, storeTestServiceID, "http://example-mcp:8080/mcp", time.Now().UTC()))

	tenants, err := store.ListTenantInstances(ctx)
	require.NoError(t, err)
	require.Len(t, tenants, 1)
	tenant := tenants[0]
	require.Equal(t, "unknown", tenant.AttestationState)

	specID, err := store.RecordTenantRuntimeSpec(ctx, TenantRuntimeSpec{
		TenantID:            tenant.TenantID,
		ServiceID:           tenant.ServiceID,
		SubjectSub:          tenant.SubjectSub,
		SpecVersion:         "v1",
		ComposeHash:         "sha256:compose",
		EnvContractHash:     "sha256:env",
		SecretContractHash:  "sha256:secret",
		IdentityContextHash: "sha256:identity",
	})
	require.NoError(t, err)
	require.False(t, specID.IsZero())

	_, err = store.RecordTenantRuntimeSpec(ctx, TenantRuntimeSpec{
		TenantID:           tenant.TenantID,
		ServiceID:          "different-service",
		SubjectSub:         tenant.SubjectSub,
		SpecVersion:        "v1",
		ComposeHash:        "sha256:compose",
		EnvContractHash:    "sha256:env",
		SecretContractHash: "sha256:secret",
	})
	require.ErrorContains(t, err, "identity mismatch")

	measurementID, err := store.RecordTenantRuntimeMeasurement(ctx, TenantRuntimeMeasurement{
		TenantID:          tenant.TenantID,
		CoolifyResourceID: "coolify-resource",
		Source:            "coolify_api",
		ComposeHash:       "sha256:compose",
		EnvContractHash:   "sha256:env",
		HealthStatus:      "healthy",
	})
	require.NoError(t, err)
	require.False(t, measurementID.IsZero())

	other := domain.Subject{Sub: "other-attested-sub", SubjectKey: "other-attested-key", PreferredUsername: "other"}
	require.NoError(t, store.UpsertManualServiceGrant(ctx, other, storeTestServiceID))
	require.NoError(t, store.UpsertStaticTenantUpstream(ctx, other, storeTestServiceID, "http://other-example-mcp:8080/mcp", time.Now().UTC()))
	tenants, err = store.ListTenantInstances(ctx)
	require.NoError(t, err)
	require.Len(t, tenants, 2)
	var otherTenant TenantInstance
	for _, tenant := range tenants {
		if tenant.SubjectSub == other.Sub {
			otherTenant = tenant
		}
	}
	require.False(t, otherTenant.TenantID.IsZero())

	_, err = store.RecordTenantRuntimeAttestation(ctx, TenantRuntimeAttestation{
		TenantID:           otherTenant.TenantID,
		SpecID:             specID,
		MeasurementID:      measurementID,
		PolicyVersion:      "observe-v1",
		Verdict:            "trusted",
		FailureReasonsJSON: []byte(`[]`),
	})
	require.Error(t, err)

	expiresAt := time.Now().UTC().Add(5 * time.Minute)
	attestationID, err := store.RecordTenantRuntimeAttestation(ctx, TenantRuntimeAttestation{
		TenantID:           tenant.TenantID,
		SpecID:             specID,
		MeasurementID:      measurementID,
		PolicyVersion:      "observe-v1",
		Verdict:            "trusted",
		FailureReasonsJSON: []byte(`[]`),
		ExpiresAt:          &expiresAt,
	})
	require.NoError(t, err)
	require.False(t, attestationID.IsZero())

	latest, err := store.GetLatestTenantRuntimeAttestation(ctx, tenant.TenantID)
	require.NoError(t, err)
	require.Equal(t, attestationID, latest.AttestationID)
	require.Equal(t, "trusted", latest.Verdict)
	require.Equal(t, specID, latest.SpecID)
	require.Equal(t, measurementID, latest.MeasurementID)

	specs, err := store.ListTenantRuntimeSpecsForTenant(ctx, tenant.TenantID)
	require.NoError(t, err)
	require.Len(t, specs, 1)
	require.JSONEq(t, `[]`, string(specs[0].ImageRefsJSON))
	require.JSONEq(t, `{}`, string(specs[0].NetworkPolicyJSON))

	measurements, err := store.ListTenantRuntimeMeasurementsForTenant(ctx, tenant.TenantID)
	require.NoError(t, err)
	require.Len(t, measurements, 1)
	require.JSONEq(t, `{}`, string(measurements[0].RawSummaryJSON))

	tenants, err = store.ListTenantInstances(ctx)
	require.NoError(t, err)
	require.Len(t, tenants, 2)
	var updatedTenant TenantInstance
	for _, candidate := range tenants {
		if candidate.TenantID == tenant.TenantID {
			updatedTenant = candidate
		}
	}
	require.False(t, updatedTenant.TenantID.IsZero())
	require.Equal(t, "trusted", updatedTenant.AttestationState)
	require.Equal(t, attestationID, updatedTenant.LastAttestationID)
	require.Equal(t, specID, updatedTenant.RuntimeSpecID)
	require.NotNil(t, updatedTenant.LastAttestedAt)

	tieCreatedAt := time.Now().UTC()
	firstTieID, err := store.RecordTenantRuntimeAttestation(ctx, TenantRuntimeAttestation{
		TenantID:           tenant.TenantID,
		SpecID:             specID,
		MeasurementID:      measurementID,
		PolicyVersion:      "observe-v1",
		Verdict:            "warning",
		FailureReasonsJSON: []byte(`["first"]`),
		CreatedAt:          tieCreatedAt,
	})
	require.NoError(t, err)
	secondTieID, err := store.RecordTenantRuntimeAttestation(ctx, TenantRuntimeAttestation{
		TenantID:           tenant.TenantID,
		SpecID:             specID,
		MeasurementID:      measurementID,
		PolicyVersion:      "observe-v1",
		Verdict:            "untrusted",
		FailureReasonsJSON: []byte(`["second"]`),
		CreatedAt:          tieCreatedAt,
	})
	require.NoError(t, err)
	require.NotEqual(t, firstTieID, secondTieID)
	latest, err = store.GetLatestTenantRuntimeAttestation(ctx, tenant.TenantID)
	require.NoError(t, err)
	require.Equal(t, secondTieID, latest.AttestationID)
	require.Equal(t, "untrusted", latest.Verdict)
}

func TestSeedServiceCatalogDisablesRemovedBuiltinEntries(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, err := NewStore(ctx, "file:"+filepath.Join(t.TempDir(), "mcp-platform.db"), zerolog.New(io.Discard))
	require.NoError(t, err)
	defer store.Close()
	require.NoError(t, store.RunMigrations(ctx))
	require.NoError(t, store.SeedServiceCatalog(ctx))

	_, err = store.db.ExecContext(ctx, `
INSERT INTO service_catalog (
    service_id,
    display_name,
    upstream_service_name,
    transport_type,
    internal_port,
    public_path,
    internal_upstream_path,
    health_path,
    health_probe_expectation,
    resource_profile,
    persistence_policy,
    adapter_requirement,
    secret_contract,
    source,
    enabled
)
VALUES ('stale-service', 'Stale', 'stale', 'streamable-http', 8080, '/stale/mcp', '/mcp', '/health', 'ok', 'small', 'ephemeral', 'none', '[]', 'builtin', 1);`)
	require.NoError(t, err)

	require.NoError(t, store.SeedServiceCatalog(ctx))

	var staleEnabled int
	require.NoError(t, store.db.QueryRowContext(ctx, `SELECT enabled FROM service_catalog WHERE service_id = 'stale-service'`).Scan(&staleEnabled))
	require.Equal(t, 0, staleEnabled)
}

func TestManualServiceGrantSurvivesAuthentikSnapshotSync(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, err := NewStore(ctx, "file:"+filepath.Join(t.TempDir(), "mcp-platform.db"), zerolog.New(io.Discard))
	require.NoError(t, err)
	defer store.Close()
	require.NoError(t, store.RunMigrations(ctx))
	require.NoError(t, store.SeedServiceCatalog(ctx))
	registerStoreTestService(t, ctx, store, storeTestServiceID)

	require.NoError(t, store.UpsertManualServiceGrant(ctx, domain.Subject{Sub: "manual-sub"}, storeTestServiceID))
	require.NoError(t, store.SyncSubjectGrantSnapshot(ctx, nil, nil))

	grants, err := store.ListSubjectServiceGrants(ctx, "manual-sub")
	require.NoError(t, err)
	require.Len(t, grants, 1)
	require.Equal(t, storeTestServiceID, grants[0].ServiceID)
	require.Equal(t, "manual", grants[0].SourceGroup)
}

func TestListSubjectServiceGrantsExcludesDisabledServices(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, err := NewStore(ctx, "file:"+filepath.Join(t.TempDir(), "mcp-platform.db"), zerolog.New(io.Discard))
	require.NoError(t, err)
	defer store.Close()
	require.NoError(t, store.RunMigrations(ctx))
	require.NoError(t, store.SeedServiceCatalog(ctx))
	registerStoreTestService(t, ctx, store, storeTestServiceID)

	require.NoError(t, store.UpsertManualServiceGrant(ctx, domain.Subject{Sub: "manual-sub"}, storeTestServiceID))
	_, err = store.db.ExecContext(ctx, `UPDATE service_catalog SET enabled = 0 WHERE service_id = ?`, storeTestServiceID)
	require.NoError(t, err)

	grants, err := store.ListSubjectServiceGrants(ctx, "manual-sub")
	require.NoError(t, err)
	require.Empty(t, grants)
}

func TestReconcileDesiredTenantsDoesNotProvisionAdminRegisteredServices(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, err := NewStore(ctx, "file:"+filepath.Join(t.TempDir(), "mcp-platform.db"), zerolog.New(io.Discard))
	require.NoError(t, err)
	defer store.Close()
	require.NoError(t, store.RunMigrations(ctx))
	require.NoError(t, store.SeedServiceCatalog(ctx))
	registerStoreTestService(t, ctx, store, storeTestServiceID)

	require.NoError(t, store.UpsertManualServiceGrant(ctx, domain.Subject{Sub: "manual-sub"}, storeTestServiceID))
	require.NoError(t, store.ReconcileDesiredTenants(ctx))

	tenants, err := store.ListTenantInstances(ctx)
	require.NoError(t, err)
	require.Empty(t, tenants)
}

func TestManualServiceGrantPreservesExistingSubjectMetadata(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, err := NewStore(ctx, "file:"+filepath.Join(t.TempDir(), "mcp-platform.db"), zerolog.New(io.Discard))
	require.NoError(t, err)
	defer store.Close()
	require.NoError(t, store.RunMigrations(ctx))
	require.NoError(t, store.SeedServiceCatalog(ctx))
	registerStoreTestService(t, ctx, store, storeTestServiceID)
	require.NoError(t, store.UpsertSubject(ctx, domain.Subject{
		Sub:               "manual-sub",
		SubjectKey:        "manual-key",
		PreferredUsername: "existing-user",
		Email:             "existing@example.com",
		DisplayName:       "Existing User",
	}))

	require.NoError(t, store.UpsertManualServiceGrant(ctx, domain.Subject{Sub: "manual-sub"}, storeTestServiceID))

	var preferredUsername, email, displayName string
	var subjectKey string
	require.NoError(t, store.db.QueryRowContext(ctx, `SELECT subject_key, preferred_username, email, display_name FROM subjects WHERE subject_sub = 'manual-sub'`).Scan(&subjectKey, &preferredUsername, &email, &displayName))
	require.Equal(t, "manual-key", subjectKey)
	require.Equal(t, "existing-user", preferredUsername)
	require.Equal(t, "existing@example.com", email)
	require.Equal(t, "Existing User", displayName)
}

func TestManualGrantDeletePreservesAuthentikGrantSource(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, err := NewStore(ctx, "file:"+filepath.Join(t.TempDir(), "mcp-platform.db"), zerolog.New(io.Discard))
	require.NoError(t, err)
	defer store.Close()
	require.NoError(t, store.RunMigrations(ctx))
	require.NoError(t, store.SeedServiceCatalog(ctx))
	registerStoreTestService(t, ctx, store, storeTestServiceID)

	subject := domain.Subject{Sub: "overlap-sub", SubjectKey: "overlap-key"}
	require.NoError(t, store.SyncSubjectGrantSnapshot(ctx, []domain.Subject{subject}, []ServiceGrant{{SubjectSub: subject.Sub, ServiceID: storeTestServiceID, SourceGroup: "mcp-service-example"}}))
	require.NoError(t, store.UpsertManualServiceGrant(ctx, subject, storeTestServiceID))

	grants, err := store.ListSubjectServiceGrants(ctx, subject.Sub)
	require.NoError(t, err)
	require.Len(t, grants, 1)
	require.Equal(t, "manual", grants[0].SourceGroup)

	require.NoError(t, store.DeleteManualServiceGrant(ctx, subject.Sub, storeTestServiceID))
	grants, err = store.ListSubjectServiceGrants(ctx, subject.Sub)
	require.NoError(t, err)
	require.Len(t, grants, 1)
	require.Equal(t, "mcp-service-example", grants[0].SourceGroup)
}
