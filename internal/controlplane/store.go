package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"dragonserver/mcp-platform/internal/catalog"
	"dragonserver/mcp-platform/internal/domain"
	"dragonserver/mcp-platform/internal/ids"
	platformsqlite "dragonserver/mcp-platform/internal/platform/sqlite"
	"dragonserver/mcp-platform/internal/platform/sqlite/platformdb"

	"github.com/rs/zerolog"
)

type Store struct {
	db      *sql.DB
	queries *platformdb.Queries
	logger  zerolog.Logger
}

type desiredTenantSpec struct {
	subject   domain.Subject
	serviceID string
}

type tenantStore interface {
	ListTenantInstances(context.Context) ([]TenantInstance, error)
	RecordReconcileRun(context.Context, ReconcileRunInput) error
	MarkTenantReconciled(context.Context, ids.UUID, time.Time, string) error
	DeleteTenantInstance(context.Context, ids.UUID) error
}

type ControlPlaneLock struct {
	mu       sync.Mutex
	store    *Store
	holderID string
	released bool
}

const controlPlaneLeaseName = "mcp-control-plane"

const controlPlaneLeaseTTL = 2 * time.Minute

const controlPlaneLeaseRenewInterval = controlPlaneLeaseTTL / 3

var ErrSubjectServiceGrantNotFound = errors.New("subject service grant not found")
var ErrTenantInstanceNotFound = errors.New("tenant instance not found")
var ErrMemoryBankProjectNotShareable = errors.New("memory bank project is not shareable")
var ErrMemoryBankInvalidMetadata = errors.New("memory bank metadata must be valid JSON")
var ErrMemoryBankShareExpired = errors.New("memory bank share expires_at must be in the future")
var ErrCatalogBuiltinMutation = errors.New("builtin service catalog entries cannot be changed through the admin API")
var ErrCatalogPathConflict = errors.New("service public path conflicts with an existing enabled service")

func NewStore(ctx context.Context, databaseURL string, logger zerolog.Logger) (*Store, error) {
	db, err := platformsqlite.Open(ctx, databaseURL)
	if err != nil {
		return nil, err
	}
	store := &Store{db: db, queries: platformdb.New(db), logger: logger}
	if err := store.Ping(ctx); err != nil {
		store.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() {
	if s.db != nil {
		_ = s.db.Close()
	}
}

func (s *Store) Ping(ctx context.Context) error {
	if err := s.db.PingContext(ctx); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}
	return nil
}

func (s *Store) AcquireControlPlaneLock(ctx context.Context) (*ControlPlaneLock, error) {
	holderID := ids.New().String()
	now := time.Now().UTC()
	holder, err := s.queries.AcquireControlPlaneLease(ctx, platformdb.AcquireControlPlaneLeaseParams{
		LeaseName: controlPlaneLeaseName,
		HolderID:  holderID,
		ExpiresAt: now.Add(controlPlaneLeaseTTL).UnixNano(),
		Now:       now.UnixNano(),
	})
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("control-plane lease is already held")
		}
		return nil, fmt.Errorf("acquire control-plane lease: %w", err)
	}
	if holder != holderID {
		return nil, fmt.Errorf("control-plane lease is held by another instance")
	}
	return &ControlPlaneLock{store: s, holderID: holderID}, nil
}

func (l *ControlPlaneLock) Release(ctx context.Context) error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.released {
		return nil
	}
	if l.store != nil {
		if err := l.store.queries.ReleaseControlPlaneLease(ctx, platformdb.ReleaseControlPlaneLeaseParams{LeaseName: controlPlaneLeaseName, HolderID: l.holderID}); err != nil {
			return fmt.Errorf("release control-plane lease: %w", err)
		}
	}
	l.released = true
	return nil
}

func (l *ControlPlaneLock) Held(ctx context.Context) bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.released || l.store == nil {
		return false
	}
	now := time.Now().UTC()
	holder, err := l.store.queries.AcquireControlPlaneLease(ctx, platformdb.AcquireControlPlaneLeaseParams{
		LeaseName: controlPlaneLeaseName,
		HolderID:  l.holderID,
		ExpiresAt: now.Add(controlPlaneLeaseTTL).UnixNano(),
		Now:       now.UnixNano(),
	})
	if err != nil || holder != l.holderID {
		l.released = true
		return false
	}
	return true
}

func (s *Store) RunMigrations(ctx context.Context) error {
	return platformsqlite.RunMigrations(ctx, s.db)
}

func (s *Store) SeedServiceCatalog(ctx context.Context) error {
	entries := catalog.DefaultCatalogV1()
	serviceIDs := make([]string, 0, len(entries))
	for _, entry := range entries {
		serviceIDs = append(serviceIDs, entry.ServiceID)
		secretContract, err := json.Marshal(entry.SecretContract)
		if err != nil {
			return fmt.Errorf("marshal secret contract for %s: %w", entry.ServiceID, err)
		}
		identityContext, err := json.Marshal(entry.IdentityContext.Normalized())
		if err != nil {
			return fmt.Errorf("marshal identity context for %s: %w", entry.ServiceID, err)
		}
		if err := s.queries.UpsertServiceCatalogEntry(ctx, platformdb.UpsertServiceCatalogEntryParams{
			ServiceID:              entry.ServiceID,
			DisplayName:            entry.DisplayName,
			UpstreamServiceName:    entry.UpstreamServiceName,
			TransportType:          string(entry.TransportType),
			InternalPort:           int64(entry.InternalPort),
			PublicPath:             entry.PublicPath,
			InternalUpstreamPath:   entry.InternalUpstreamPath,
			HealthPath:             entry.HealthPath,
			HealthProbeExpectation: entry.HealthProbeExpectation,
			ResourceProfile:        entry.ResourceProfile,
			PersistencePolicy:      entry.PersistencePolicy,
			AdapterRequirement:     string(entry.AdapterRequirement),
			SecretContract:         string(secretContract),
			IdentityContext:        string(identityContext),
			Enabled:                1,
			Source:                 "builtin",
		}); err != nil {
			return fmt.Errorf("seed service catalog entry %s: %w", entry.ServiceID, err)
		}
	}
	if len(serviceIDs) > 0 {
		if err := s.queries.DisableServiceCatalogEntriesNotIn(ctx, platformdb.DisableServiceCatalogEntriesNotInParams{ServiceIds: serviceIDs}); err != nil {
			return fmt.Errorf("disable stale service catalog entries: %w", err)
		}
	} else if err := s.queries.DisableAllBuiltinServiceCatalogEntries(ctx); err != nil {
		return fmt.Errorf("disable stale service catalog entries: %w", err)
	}
	return nil
}

func (s *Store) ListServiceCatalog(ctx context.Context) ([]ServiceCatalogAdminEntry, error) {
	records, err := s.queries.ListServiceCatalog(ctx)
	if err != nil {
		return nil, fmt.Errorf("list service catalog: %w", err)
	}
	return convertServiceCatalogAdminEntries(records)
}

func (s *Store) GetServiceCatalogAdminEntry(ctx context.Context, serviceID string) (ServiceCatalogAdminEntry, error) {
	record, err := s.queries.GetServiceCatalogEntry(ctx, platformdb.GetServiceCatalogEntryParams{ServiceID: serviceID})
	if err != nil {
		return ServiceCatalogAdminEntry{}, fmt.Errorf("get service catalog entry %s: %w", serviceID, err)
	}
	return convertServiceCatalogAdminEntry(record)
}

func (s *Store) ListEnabledServiceIDs(ctx context.Context) ([]string, error) {
	serviceIDs, err := s.queries.ListEnabledServiceIDs(ctx)
	if err != nil {
		return nil, fmt.Errorf("list enabled service IDs: %w", err)
	}
	return serviceIDs, nil
}

func (s *Store) GetEnabledServiceCatalogEntry(ctx context.Context, serviceID string) (catalog.ServiceCatalogEntry, error) {
	record, err := s.queries.GetEnabledServiceCatalogEntry(ctx, platformdb.GetEnabledServiceCatalogEntryParams{ServiceID: serviceID})
	if err != nil {
		return catalog.ServiceCatalogEntry{}, fmt.Errorf("get enabled service catalog entry %s: %w", serviceID, err)
	}
	return convertEnabledServiceCatalogRecord(record)
}

func (s *Store) UpsertAdminServiceCatalogEntry(ctx context.Context, entry catalog.ServiceCatalogEntry) error {
	secretContract, err := json.Marshal(entry.SecretContract)
	if err != nil {
		return fmt.Errorf("marshal secret contract for %s: %w", entry.ServiceID, err)
	}
	identityContext, err := json.Marshal(entry.IdentityContext.Normalized())
	if err != nil {
		return fmt.Errorf("marshal identity context for %s: %w", entry.ServiceID, err)
	}
	return s.withTx(ctx, func(q *platformdb.Queries) error {
		if existing, err := q.GetServiceCatalogEntry(ctx, platformdb.GetServiceCatalogEntryParams{ServiceID: entry.ServiceID}); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("load existing service catalog entry %s: %w", entry.ServiceID, err)
		} else if err == nil && existing.Source == "builtin" {
			return ErrCatalogBuiltinMutation
		}
		if err := ensureNoPublicPathConflict(ctx, q, entry.ServiceID, entry.PublicPath); err != nil {
			return err
		}
		if err := q.UpsertServiceCatalogEntry(ctx, platformdb.UpsertServiceCatalogEntryParams{
			ServiceID:              entry.ServiceID,
			DisplayName:            entry.DisplayName,
			UpstreamServiceName:    entry.UpstreamServiceName,
			TransportType:          string(entry.TransportType),
			InternalPort:           int64(entry.InternalPort),
			PublicPath:             entry.PublicPath,
			InternalUpstreamPath:   entry.InternalUpstreamPath,
			HealthPath:             entry.HealthPath,
			HealthProbeExpectation: entry.HealthProbeExpectation,
			ResourceProfile:        entry.ResourceProfile,
			PersistencePolicy:      entry.PersistencePolicy,
			AdapterRequirement:     string(entry.AdapterRequirement),
			SecretContract:         string(secretContract),
			IdentityContext:        string(identityContext),
			Enabled:                1,
			Source:                 "admin_api",
		}); err != nil {
			return fmt.Errorf("upsert admin service catalog entry %s: %w", entry.ServiceID, err)
		}
		return nil
	})

}

func (s *Store) DisableServiceCatalogEntry(ctx context.Context, serviceID string) error {
	return s.withTx(ctx, func(q *platformdb.Queries) error {
		existing, err := q.GetServiceCatalogEntry(ctx, platformdb.GetServiceCatalogEntryParams{ServiceID: serviceID})
		if err != nil {
			return fmt.Errorf("load existing service catalog entry %s: %w", serviceID, err)
		}
		if existing.Source == "builtin" {
			return ErrCatalogBuiltinMutation
		}
		if err := q.DisableServiceCatalogEntry(ctx, platformdb.DisableServiceCatalogEntryParams{ServiceID: serviceID}); err != nil {
			return fmt.Errorf("disable service catalog entry %s: %w", serviceID, err)
		}
		return nil
	})
}

func (s *Store) UpsertSubject(ctx context.Context, subject domain.Subject) error {
	if err := s.queries.UpsertSubject(ctx, platformdb.UpsertSubjectParams{
		SubjectSub:          subject.Sub,
		SubjectKey:          subject.SubjectKey,
		PreferredUsername:   sqlNullString(subject.PreferredUsername),
		Email:               sqlNullString(subject.Email),
		DisplayName:         sqlNullString(subject.DisplayName),
		AccountBindingID:    sqlNullString(subject.AccountBindingID),
		AccountBindingClaim: sqlNullString(subject.AccountBindingClaim),
	}); err != nil {
		return fmt.Errorf("upsert subject %s: %w", subject.Sub, err)
	}
	return nil
}

func (s *Store) ReplaceSubjectGrants(ctx context.Context, subjectSub string, grants []ServiceGrant) error {
	return s.withTx(ctx, func(q *platformdb.Queries) error {
		if err := q.DeleteSubjectGrants(ctx, platformdb.DeleteSubjectGrantsParams{SubjectSub: subjectSub}); err != nil {
			return fmt.Errorf("delete existing grants for %s: %w", subjectSub, err)
		}
		if err := q.DeleteSubjectGrantSources(ctx, platformdb.DeleteSubjectGrantSourcesParams{SubjectSub: subjectSub}); err != nil {
			return fmt.Errorf("delete existing grant sources for %s: %w", subjectSub, err)
		}
		if err := insertGrants(ctx, q, subjectSub, grants); err != nil {
			return err
		}
		return rebuildEffectiveServiceGrants(ctx, q)
	})
}

func (s *Store) ListSubjectServiceGrants(ctx context.Context, subjectSub string) ([]ServiceGrant, error) {
	rows, err := s.queries.ListSubjectServiceGrants(ctx, platformdb.ListSubjectServiceGrantsParams{SubjectSub: subjectSub})
	if err != nil {
		return nil, fmt.Errorf("list service grants for %s: %w", subjectSub, err)
	}
	grants := make([]ServiceGrant, 0, len(rows))
	for _, row := range rows {
		grant := ServiceGrant{
			SubjectSub:  row.SubjectSub,
			ServiceID:   row.ServiceID,
			SourceGroup: row.SourceGroup,
		}
		grantedAt, err := parseSQLiteTime(row.GrantedAt)
		if err != nil {
			return nil, fmt.Errorf("parse grant granted_at: %w", err)
		}
		lastSyncedAt, err := parseSQLiteTime(row.LastSyncedAt)
		if err != nil {
			return nil, fmt.Errorf("parse grant last_synced_at: %w", err)
		}
		grant.GrantedAt = grantedAt
		grant.LastSyncedAt = lastSyncedAt
		grants = append(grants, grant)
	}
	return grants, nil
}

func (s *Store) UpsertManualServiceGrant(ctx context.Context, subject domain.Subject, serviceID string) error {
	return s.withTx(ctx, func(q *platformdb.Queries) error {
		if _, err := q.GetEnabledServiceCatalogEntry(ctx, platformdb.GetEnabledServiceCatalogEntryParams{ServiceID: serviceID}); err != nil {
			return fmt.Errorf("load enabled service %s: %w", serviceID, err)
		}
		if subject.SubjectKey == "" {
			subject.SubjectKey = domain.DeriveSubjectKey(subject.Sub)
		}
		if err := q.UpsertSubjectPreservingMetadata(ctx, platformdb.UpsertSubjectPreservingMetadataParams{
			SubjectSub:          subject.Sub,
			SubjectKey:          subject.SubjectKey,
			PreferredUsername:   sqlNullString(subject.PreferredUsername),
			Email:               sqlNullString(subject.Email),
			DisplayName:         sqlNullString(subject.DisplayName),
			AccountBindingID:    sqlNullString(subject.AccountBindingID),
			AccountBindingClaim: sqlNullString(subject.AccountBindingClaim),
		}); err != nil {
			return fmt.Errorf("upsert subject %s for manual grant: %w", subject.Sub, err)
		}
		if err := q.UpsertManualServiceGrantSource(ctx, platformdb.UpsertManualServiceGrantSourceParams{SubjectSub: subject.Sub, ServiceID: serviceID}); err != nil {
			return fmt.Errorf("upsert manual grant %s/%s: %w", subject.Sub, serviceID, err)
		}
		return rebuildEffectiveServiceGrants(ctx, q)
	})
}

func (s *Store) DeleteManualServiceGrant(ctx context.Context, subjectSub string, serviceID string) error {
	return s.withTx(ctx, func(q *platformdb.Queries) error {
		if err := q.DeleteManualServiceGrantSource(ctx, platformdb.DeleteManualServiceGrantSourceParams{SubjectSub: subjectSub, ServiceID: serviceID}); err != nil {
			return fmt.Errorf("delete manual service grant %s/%s: %w", subjectSub, serviceID, err)
		}
		return rebuildEffectiveServiceGrants(ctx, q)
	})
}

func (s *Store) SubjectServiceGranted(ctx context.Context, subjectSub string, serviceID string) (bool, error) {
	grantCount, err := s.queries.CountSubjectServiceGrant(ctx, platformdb.CountSubjectServiceGrantParams{SubjectSub: subjectSub, ServiceID: serviceID})
	if err != nil {
		return false, fmt.Errorf("count service grant %s/%s: %w", subjectSub, serviceID, err)
	}
	return grantCount > 0, nil
}

func (s *Store) UpsertStaticTenantUpstream(ctx context.Context, subject domain.Subject, serviceID string, upstreamURL string, verifiedAt time.Time) error {
	return s.withTx(ctx, func(q *platformdb.Queries) error {
		if _, err := q.GetEnabledServiceCatalogEntry(ctx, platformdb.GetEnabledServiceCatalogEntryParams{ServiceID: serviceID}); err != nil {
			return fmt.Errorf("load enabled service %s: %w", serviceID, err)
		}
		if subject.SubjectKey == "" {
			subject.SubjectKey = domain.DeriveSubjectKey(subject.Sub)
		}
		if err := q.UpsertSubjectPreservingMetadata(ctx, platformdb.UpsertSubjectPreservingMetadataParams{
			SubjectSub:          subject.Sub,
			SubjectKey:          subject.SubjectKey,
			PreferredUsername:   sqlNullString(subject.PreferredUsername),
			Email:               sqlNullString(subject.Email),
			DisplayName:         sqlNullString(subject.DisplayName),
			AccountBindingID:    sqlNullString(subject.AccountBindingID),
			AccountBindingClaim: sqlNullString(subject.AccountBindingClaim),
		}); err != nil {
			return fmt.Errorf("upsert subject %s for static upstream: %w", subject.Sub, err)
		}
		persistedSubject, err := q.GetSubject(ctx, platformdb.GetSubjectParams{SubjectSub: subject.Sub})
		if err != nil {
			return fmt.Errorf("load subject %s for static upstream: %w", subject.Sub, err)
		}
		subject.SubjectKey = persistedSubject.SubjectKey
		grantCount, err := q.CountSubjectServiceGrant(ctx, platformdb.CountSubjectServiceGrantParams{SubjectSub: subject.Sub, ServiceID: serviceID})
		if err != nil {
			return fmt.Errorf("count service grant %s/%s: %w", subject.Sub, serviceID, err)
		}
		if grantCount == 0 {
			return ErrSubjectServiceGrantNotFound
		}
		tenantInstanceName := domain.BuildTenantInstanceName(serviceID, subject.SubjectKey)
		if err := q.UpsertStaticTenantUpstream(ctx, platformdb.UpsertStaticTenantUpstreamParams{
			TenantID:           ids.New().Bytes(),
			SubjectSub:         subject.Sub,
			ServiceID:          serviceID,
			SubjectKey:         subject.SubjectKey,
			TenantInstanceName: tenantInstanceName,
			InternalDnsName:    tenantInstanceName,
			UpstreamUrl:        sql.NullString{String: upstreamURL, Valid: true},
			LastHealthyAt:      sql.NullString{String: formatSQLiteTime(verifiedAt), Valid: true},
		}); err != nil {
			return fmt.Errorf("upsert static upstream %s/%s: %w", subject.Sub, serviceID, err)
		}
		return nil
	})
}

func (s *Store) SyncSubjectGrantSnapshot(ctx context.Context, subjects []domain.Subject, grants []ServiceGrant) error {
	return s.withTx(ctx, func(q *platformdb.Queries) error {
		subjectsBySub := make(map[string]domain.Subject, len(subjects))
		for _, subject := range subjects {
			subjectsBySub[subject.Sub] = subject
		}
		subjectSubs := mapsSortedKeys(subjectsBySub)
		for _, subjectSub := range subjectSubs {
			subject := subjectsBySub[subjectSub]
			if err := q.UpsertSubject(ctx, platformdb.UpsertSubjectParams{
				SubjectSub:          subject.Sub,
				SubjectKey:          subject.SubjectKey,
				PreferredUsername:   sqlNullString(subject.PreferredUsername),
				Email:               sqlNullString(subject.Email),
				DisplayName:         sqlNullString(subject.DisplayName),
				AccountBindingID:    sqlNullString(subject.AccountBindingID),
				AccountBindingClaim: sqlNullString(subject.AccountBindingClaim),
			}); err != nil {
				return fmt.Errorf("upsert subject %s during snapshot sync: %w", subject.Sub, err)
			}
		}
		if len(subjectSubs) == 0 {
			if err := q.DeleteAllServiceGrants(ctx); err != nil {
				return fmt.Errorf("clear service grants for empty snapshot: %w", err)
			}
		} else if err := q.DeleteStaleServiceGrants(ctx, platformdb.DeleteStaleServiceGrantsParams{SubjectSubs: subjectSubs}); err != nil {
			return fmt.Errorf("delete stale service grants: %w", err)
		}
		grantsBySubject := make(map[string][]ServiceGrant)
		for _, grant := range grants {
			grantsBySubject[grant.SubjectSub] = append(grantsBySubject[grant.SubjectSub], grant)
		}
		for _, subjectSub := range subjectSubs {
			if err := q.DeleteSubjectSyncedGrantSources(ctx, platformdb.DeleteSubjectSyncedGrantSourcesParams{SubjectSub: subjectSub}); err != nil {
				return fmt.Errorf("delete existing synced grants for %s during snapshot sync: %w", subjectSub, err)
			}
			if err := insertGrants(ctx, q, subjectSub, grantsBySubject[subjectSub]); err != nil {
				return err
			}
		}
		return rebuildEffectiveServiceGrants(ctx, q)
	})
}

func (s *Store) ReconcileDesiredTenants(ctx context.Context) error {
	return s.withTx(ctx, func(q *platformdb.Queries) error {
		desiredSpecs, err := loadDesiredTenantSpecs(ctx, q)
		if err != nil {
			return err
		}
		currentTenants, err := loadTenantInstances(ctx, q)
		if err != nil {
			return err
		}
		for _, key := range mapsSortedKeys(desiredSpecs) {
			spec := desiredSpecs[key]
			tenant, ok := currentTenants[key]
			if !ok {
				if err := insertTenantInstance(ctx, q, spec); err != nil {
					return err
				}
				continue
			}
			if tenant.DesiredState == domain.TenantDesiredStateDisabled || tenant.DesiredState == domain.TenantDesiredStateDeleted {
				continue
			}
			if err := enableTenantInstance(ctx, q, tenant, spec); err != nil {
				return err
			}
		}
		for _, key := range mapsSortedKeys(currentTenants) {
			tenant := currentTenants[key]
			if _, ok := desiredSpecs[key]; ok || tenant.DesiredState == domain.TenantDesiredStateDeleted || tenantRuntimeMode(tenant) == runtimeModeStaticUpstream {
				continue
			}
			if err := q.MarkTenantDesiredDeleted(ctx, platformdb.MarkTenantDesiredDeletedParams{TenantID: tenant.TenantID.Bytes(), DesiredState: string(domain.TenantDesiredStateDeleted)}); err != nil {
				return fmt.Errorf("mark tenant %s as deleted: %w", tenant.TenantID, err)
			}
		}
		return nil
	})
}

func (s *Store) SuspendTenantInstance(ctx context.Context, subjectSub string, serviceID string) error {
	rows, err := s.queries.MarkTenantDesiredDisabledBySubjectService(ctx, platformdb.MarkTenantDesiredDisabledBySubjectServiceParams{
		TargetSubjectSub: subjectSub,
		TargetServiceID:  serviceID,
	})
	if err != nil {
		return fmt.Errorf("suspend tenant %s/%s: %w", subjectSub, serviceID, err)
	}
	if rows == 0 {
		return ErrTenantInstanceNotFound
	}
	return nil
}

func (s *Store) ResumeTenantInstance(ctx context.Context, subjectSub string, serviceID string) error {
	rows, err := s.queries.MarkTenantDesiredEnabledBySubjectServiceWithGrant(ctx, platformdb.MarkTenantDesiredEnabledBySubjectServiceWithGrantParams{
		TargetSubjectSub: subjectSub,
		TargetServiceID:  serviceID,
	})
	if err != nil {
		return fmt.Errorf("resume tenant %s/%s: %w", subjectSub, serviceID, err)
	}
	if rows == 0 {
		grantCount, err := s.queries.CountSubjectServiceGrant(ctx, platformdb.CountSubjectServiceGrantParams{SubjectSub: subjectSub, ServiceID: serviceID})
		if err != nil {
			return fmt.Errorf("count service grant for %s/%s: %w", subjectSub, serviceID, err)
		}
		if grantCount == 0 {
			return ErrSubjectServiceGrantNotFound
		}
		return ErrTenantInstanceNotFound
	}
	return nil
}

func (s *Store) ListTenantInstances(ctx context.Context) ([]TenantInstance, error) {
	records, err := s.queries.ListTenantInstances(ctx)
	if err != nil {
		return nil, fmt.Errorf("list tenant instances: %w", err)
	}
	return convertTenantInstances(records)
}

func (s *Store) GetTenantInstance(ctx context.Context, tenantID ids.UUID) (TenantInstance, error) {
	record, err := s.queries.GetTenantInstance(ctx, platformdb.GetTenantInstanceParams{TenantID: tenantID.Bytes()})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return TenantInstance{}, ErrTenantInstanceNotFound
		}
		return TenantInstance{}, fmt.Errorf("get tenant instance %s: %w", tenantID, err)
	}
	tenants, err := convertTenantInstances([]platformdb.ListTenantInstancesRow{platformdb.ListTenantInstancesRow(record)})
	if err != nil {
		return TenantInstance{}, err
	}
	return tenants[0], nil
}

func (s *Store) GetTenantInstanceBySubjectService(ctx context.Context, subjectSub string, serviceID string) (TenantInstance, error) {
	record, err := s.queries.GetTenantInstanceBySubjectService(ctx, platformdb.GetTenantInstanceBySubjectServiceParams{SubjectSub: subjectSub, ServiceID: serviceID})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return TenantInstance{}, ErrTenantInstanceNotFound
		}
		return TenantInstance{}, fmt.Errorf("get tenant instance for %s/%s: %w", subjectSub, serviceID, err)
	}
	tenants, err := convertTenantInstances([]platformdb.ListTenantInstancesRow{platformdb.ListTenantInstancesRow(record)})
	if err != nil {
		return TenantInstance{}, err
	}
	return tenants[0], nil
}

func (s *Store) RecordReconcileRun(ctx context.Context, input ReconcileRunInput) error {
	detailsJSON, err := json.Marshal(input.Details)
	if err != nil {
		return fmt.Errorf("marshal reconcile details: %w", err)
	}
	if err := s.queries.InsertReconcileRun(ctx, platformdb.InsertReconcileRunParams{
		RunID:         ids.New().Bytes(),
		TenantID:      input.TenantID.Bytes(),
		DesiredState:  string(input.DesiredState),
		ObservedState: sql.NullString{String: string(input.ObservedState), Valid: input.ObservedState != ""},
		Action:        input.Action,
		Status:        input.Status,
		Details:       string(detailsJSON),
		StartedAt:     formatSQLiteTime(input.StartedAt),
		FinishedAt:    sqlNullTime(input.FinishedAt),
	}); err != nil {
		return fmt.Errorf("insert reconcile run for tenant %s: %w", input.TenantID, err)
	}
	return nil
}

func (s *Store) MarkTenantReconciled(ctx context.Context, tenantID ids.UUID, reconciledAt time.Time, lastError string) error {
	if err := s.queries.MarkTenantReconciled(ctx, platformdb.MarkTenantReconciledParams{TenantID: tenantID.Bytes(), LastReconciledAt: sqlNullTime(reconciledAt), LastError: lastError}); err != nil {
		return fmt.Errorf("mark tenant %s reconciled: %w", tenantID, err)
	}
	return nil
}

func (s *Store) UpdateTenantRuntimeStatus(ctx context.Context, update TenantRuntimeUpdate) error {
	var lastHealthyAt any
	if update.LastHealthyAt != nil {
		lastHealthyAt = formatSQLiteTime(*update.LastHealthyAt)
	}
	if err := s.queries.UpdateTenantRuntimeStatus(ctx, platformdb.UpdateTenantRuntimeStatusParams{
		TenantID:               update.TenantID.Bytes(),
		RuntimeState:           string(update.RuntimeState),
		CoolifyResourceID:      update.CoolifyResourceID,
		CoolifyApplicationID:   update.CoolifyApplicationID,
		UpstreamUrl:            update.UpstreamURL,
		LastHealthyAt:          lastHealthyAt,
		ClearRuntimeReferences: update.ClearRuntimeReferences,
		LastError:              update.LastError,
	}); err != nil {
		return fmt.Errorf("update tenant runtime status for %s: %w", update.TenantID, err)
	}
	return nil
}

func (s *Store) DeleteTenantInstance(ctx context.Context, tenantID ids.UUID) error {
	if err := s.queries.DeleteTenantInstance(ctx, platformdb.DeleteTenantInstanceParams{TenantID: tenantID.Bytes()}); err != nil {
		return fmt.Errorf("delete tenant instance %s: %w", tenantID, err)
	}
	return nil
}

func (s *Store) UpsertMemoryBankProject(ctx context.Context, project MemoryBankProject) (ids.UUID, error) {
	if project.ProjectID.IsZero() {
		project.ProjectID = ids.New()
	}
	tenant, err := s.GetTenantInstance(ctx, project.OwnerTenantID)
	if err != nil {
		return ids.UUID{}, err
	}
	if tenant.SubjectSub != project.OwnerSubjectSub || tenant.ServiceID != project.ServiceID {
		return ids.UUID{}, fmt.Errorf("memory bank project owner tenant mismatch")
	}
	metadata, err := normalizeMemoryBankMetadata(project.Metadata)
	if err != nil {
		return ids.UUID{}, err
	}
	persistedIDBytes, err := s.queries.UpsertMemoryBankProject(ctx, platformdb.UpsertMemoryBankProjectParams{
		ProjectID:       project.ProjectID.Bytes(),
		OwnerSubjectSub: project.OwnerSubjectSub,
		OwnerTenantID:   project.OwnerTenantID.Bytes(),
		ServiceID:       project.ServiceID,
		ProjectKey:      project.ProjectKey,
		DisplayName:     project.DisplayName,
		RootPath:        project.RootPath,
		Metadata:        metadata,
		ArchivedAt:      sqlNullTimePtr(project.ArchivedAt),
	})
	if err != nil {
		return ids.UUID{}, fmt.Errorf("upsert memory bank project %s/%s: %w", project.OwnerSubjectSub, project.ProjectKey, err)
	}
	persistedID, err := ids.ParseBytes(persistedIDBytes)
	if err != nil {
		return ids.UUID{}, fmt.Errorf("parse memory bank project id: %w", err)
	}
	return persistedID, nil
}

func (s *Store) GetMemoryBankProject(ctx context.Context, projectID ids.UUID) (MemoryBankProject, error) {
	record, err := s.queries.GetMemoryBankProject(ctx, platformdb.GetMemoryBankProjectParams{ProjectID: projectID.Bytes()})
	if err != nil {
		return MemoryBankProject{}, fmt.Errorf("get memory bank project %s: %w", projectID, err)
	}
	return convertMemoryBankProject(record)
}

func (s *Store) GetMemoryBankProjectByOwnerKey(ctx context.Context, ownerSubjectSub string, serviceID string, projectKey string) (MemoryBankProject, error) {
	record, err := s.queries.GetMemoryBankProjectByOwnerKey(ctx, platformdb.GetMemoryBankProjectByOwnerKeyParams{OwnerSubjectSub: ownerSubjectSub, ServiceID: serviceID, ProjectKey: projectKey})
	if err != nil {
		return MemoryBankProject{}, fmt.Errorf("get memory bank project %s/%s/%s: %w", ownerSubjectSub, serviceID, projectKey, err)
	}
	return convertMemoryBankProject(platformdb.MemoryBankProject(record))
}

func (s *Store) ListMemoryBankProjectsByOwner(ctx context.Context, ownerSubjectSub string, serviceID string, includeArchived bool) ([]MemoryBankProject, error) {
	records, err := s.queries.ListMemoryBankProjectsByOwner(ctx, platformdb.ListMemoryBankProjectsByOwnerParams{
		OwnerSubjectSub: ownerSubjectSub,
		ServiceID:       serviceID,
		IncludeArchived: includeArchived,
	})
	if err != nil {
		return nil, fmt.Errorf("list memory bank projects for %s/%s: %w", ownerSubjectSub, serviceID, err)
	}
	return convertMemoryBankProjects(records)
}

func (s *Store) CreateMemoryBankProjectShare(ctx context.Context, share MemoryBankProjectShare) (ids.UUID, error) {
	if share.ShareID.IsZero() {
		share.ShareID = ids.New()
	}
	if share.State == "" {
		share.State = "pending"
	}
	if share.Source == "" {
		share.Source = "oauth_consent"
	}
	now := time.Now().UTC()
	if share.ExpiresAt != nil && !share.ExpiresAt.After(now) {
		return ids.UUID{}, ErrMemoryBankShareExpired
	}
	metadata, err := normalizeMemoryBankMetadata(share.Metadata)
	if err != nil {
		return ids.UUID{}, err
	}
	if err := s.withTx(ctx, func(q *platformdb.Queries) error {
		if _, err := q.ExpireMemoryBankProjectShares(ctx, platformdb.ExpireMemoryBankProjectSharesParams{Now: formatSQLiteTime(now)}); err != nil {
			return fmt.Errorf("expire memory bank project shares: %w", err)
		}
		rows, err := q.InsertMemoryBankProjectShare(ctx, platformdb.InsertMemoryBankProjectShareParams{
			ShareID:                share.ShareID.Bytes(),
			ProjectID:              share.ProjectID.Bytes(),
			OwnerSubjectSub:        share.OwnerSubjectSub,
			CollaboratorSubjectSub: share.CollaboratorSubjectSub,
			Permission:             share.Permission,
			State:                  share.State,
			Source:                 share.Source,
			CreatedBySubjectSub:    share.CreatedBySubjectSub,
			ExpiresAt:              sqlNullTimePtr(share.ExpiresAt),
			Metadata:               metadata,
		})
		if err != nil {
			return fmt.Errorf("create memory bank project share %s: %w", share.ProjectID, err)
		}
		if rows == 0 {
			return ErrMemoryBankProjectNotShareable
		}
		return nil
	}); err != nil {
		return ids.UUID{}, err
	}
	return share.ShareID, nil
}

func normalizeMemoryBankMetadata(metadata json.RawMessage) (string, error) {
	raw := strings.TrimSpace(string(metadata))
	if raw == "" {
		return "{}", nil
	}
	if !json.Valid([]byte(raw)) {
		return "", ErrMemoryBankInvalidMetadata
	}
	return raw, nil
}

func (s *Store) GetMemoryBankProjectShare(ctx context.Context, shareID ids.UUID) (MemoryBankProjectShare, error) {
	record, err := s.queries.GetMemoryBankProjectShare(ctx, platformdb.GetMemoryBankProjectShareParams{ShareID: shareID.Bytes()})
	if err != nil {
		return MemoryBankProjectShare{}, fmt.Errorf("get memory bank project share %s: %w", shareID, err)
	}
	return convertMemoryBankProjectShare(record)
}

func (s *Store) ListMemoryBankProjectSharesForSubject(ctx context.Context, collaboratorSubjectSub string, includeInactive bool) ([]MemoryBankProjectShare, error) {
	now := time.Now().UTC()
	if err := s.expireMemoryBankProjectShares(ctx, now); err != nil {
		return nil, err
	}
	records, err := s.queries.ListMemoryBankProjectSharesForSubject(ctx, platformdb.ListMemoryBankProjectSharesForSubjectParams{
		CollaboratorSubjectSub: collaboratorSubjectSub,
		IncludeInactive:        includeInactive,
		Now:                    formatSQLiteTime(now),
	})
	if err != nil {
		return nil, fmt.Errorf("list memory bank shares for %s: %w", collaboratorSubjectSub, err)
	}
	return convertMemoryBankProjectShares(records)
}

func (s *Store) ListMemoryBankProjectSharesForSubjectService(ctx context.Context, collaboratorSubjectSub string, serviceID string, includeInactive bool) ([]MemoryBankProjectShare, error) {
	now := time.Now().UTC()
	if err := s.expireMemoryBankProjectShares(ctx, now); err != nil {
		return nil, err
	}
	records, err := s.queries.ListMemoryBankProjectSharesForSubjectService(ctx, platformdb.ListMemoryBankProjectSharesForSubjectServiceParams{
		CollaboratorSubjectSub: collaboratorSubjectSub,
		ServiceID:              serviceID,
		IncludeInactive:        includeInactive,
		Now:                    formatSQLiteTime(now),
	})
	if err != nil {
		return nil, fmt.Errorf("list memory bank shares for %s/%s: %w", collaboratorSubjectSub, serviceID, err)
	}
	return convertMemoryBankProjectShares(records)
}

func (s *Store) ActivateMemoryBankProjectShare(ctx context.Context, shareID ids.UUID, collaboratorSubjectSub string, acceptedAt time.Time) (bool, error) {
	if err := s.expireMemoryBankProjectShares(ctx, acceptedAt); err != nil {
		return false, err
	}
	rows, err := s.queries.ActivateMemoryBankProjectShare(ctx, platformdb.ActivateMemoryBankProjectShareParams{
		ShareID:                shareID.Bytes(),
		CollaboratorSubjectSub: collaboratorSubjectSub,
		AcceptedAt:             sqlNullTime(acceptedAt),
	})
	if err != nil {
		return false, fmt.Errorf("activate memory bank project share %s: %w", shareID, err)
	}
	return rows > 0, nil
}

func (s *Store) expireMemoryBankProjectShares(ctx context.Context, now time.Time) error {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if _, err := s.queries.ExpireMemoryBankProjectShares(ctx, platformdb.ExpireMemoryBankProjectSharesParams{Now: formatSQLiteTime(now)}); err != nil {
		return fmt.Errorf("expire memory bank project shares: %w", err)
	}
	return nil
}

func (s *Store) RevokeMemoryBankProjectShare(ctx context.Context, shareID ids.UUID, revokedAt time.Time) (bool, error) {
	rows, err := s.queries.RevokeMemoryBankProjectShare(ctx, platformdb.RevokeMemoryBankProjectShareParams{
		ShareID:   shareID.Bytes(),
		RevokedAt: sqlNullTime(revokedAt),
	})
	if err != nil {
		return false, fmt.Errorf("revoke memory bank project share %s: %w", shareID, err)
	}
	return rows > 0, nil
}

func (s *Store) RecordTenantRuntimeSpec(ctx context.Context, spec TenantRuntimeSpec) (ids.UUID, error) {
	if err := s.withTx(ctx, func(q *platformdb.Queries) error {
		prepared, params, err := s.prepareTenantRuntimeSpec(ctx, q, spec)
		if err != nil {
			return err
		}
		spec = prepared
		if err := q.InsertTenantRuntimeSpec(ctx, params); err != nil {
			return fmt.Errorf("insert tenant runtime spec %s: %w", spec.TenantID, err)
		}
		if err := q.MarkTenantRuntimeSpec(ctx, platformdb.MarkTenantRuntimeSpecParams{SpecID: spec.SpecID.Bytes(), TenantID: spec.TenantID.Bytes()}); err != nil {
			return fmt.Errorf("mark tenant runtime spec %s: %w", spec.TenantID, err)
		}
		return nil
	}); err != nil {
		return ids.UUID{}, err
	}
	return spec.SpecID, nil
}

func (s *Store) GetTenantRuntimeSpec(ctx context.Context, specID ids.UUID) (TenantRuntimeSpec, error) {
	record, err := s.queries.GetTenantRuntimeSpec(ctx, platformdb.GetTenantRuntimeSpecParams{SpecID: specID.Bytes()})
	if err != nil {
		return TenantRuntimeSpec{}, fmt.Errorf("get tenant runtime spec %s: %w", specID, err)
	}
	return convertTenantRuntimeSpec(record)
}

func (s *Store) ListTenantRuntimeSpecsForTenant(ctx context.Context, tenantID ids.UUID) ([]TenantRuntimeSpec, error) {
	records, err := s.queries.ListTenantRuntimeSpecsForTenant(ctx, platformdb.ListTenantRuntimeSpecsForTenantParams{TenantID: tenantID.Bytes()})
	if err != nil {
		return nil, fmt.Errorf("list tenant runtime specs %s: %w", tenantID, err)
	}
	return convertTenantRuntimeSpecs(records)
}

func (s *Store) RecordTenantRuntimeMeasurement(ctx context.Context, measurement TenantRuntimeMeasurement) (ids.UUID, error) {
	measurement, params := prepareTenantRuntimeMeasurement(measurement)
	if err := s.queries.InsertTenantRuntimeMeasurement(ctx, params); err != nil {
		return ids.UUID{}, fmt.Errorf("insert tenant runtime measurement %s: %w", measurement.TenantID, err)
	}
	return measurement.MeasurementID, nil
}

func (s *Store) GetTenantRuntimeMeasurement(ctx context.Context, measurementID ids.UUID) (TenantRuntimeMeasurement, error) {
	record, err := s.queries.GetTenantRuntimeMeasurement(ctx, platformdb.GetTenantRuntimeMeasurementParams{MeasurementID: measurementID.Bytes()})
	if err != nil {
		return TenantRuntimeMeasurement{}, fmt.Errorf("get tenant runtime measurement %s: %w", measurementID, err)
	}
	return convertTenantRuntimeMeasurement(record)
}

func (s *Store) ListTenantRuntimeMeasurementsForTenant(ctx context.Context, tenantID ids.UUID) ([]TenantRuntimeMeasurement, error) {
	records, err := s.queries.ListTenantRuntimeMeasurementsForTenant(ctx, platformdb.ListTenantRuntimeMeasurementsForTenantParams{TenantID: tenantID.Bytes()})
	if err != nil {
		return nil, fmt.Errorf("list tenant runtime measurements %s: %w", tenantID, err)
	}
	return convertTenantRuntimeMeasurements(records)
}

func (s *Store) RecordTenantRuntimeAttestation(ctx context.Context, attestation TenantRuntimeAttestation) (ids.UUID, error) {
	if err := s.withTx(ctx, func(q *platformdb.Queries) error {
		prepared, params := prepareTenantRuntimeAttestation(attestation)
		attestation = prepared
		if err := q.InsertTenantRuntimeAttestation(ctx, params); err != nil {
			return fmt.Errorf("insert tenant runtime attestation %s: %w", attestation.TenantID, err)
		}
		if err := q.MarkTenantAttestationSummary(ctx, platformdb.MarkTenantAttestationSummaryParams{
			Verdict:       attestation.Verdict,
			AttestedAt:    sqlNullTime(attestation.CreatedAt),
			AttestationID: attestation.AttestationID.Bytes(),
			SpecID:        nullableUUIDBytes(attestation.SpecID),
			TenantID:      attestation.TenantID.Bytes(),
		}); err != nil {
			return fmt.Errorf("mark tenant attestation summary %s: %w", attestation.TenantID, err)
		}
		return nil
	}); err != nil {
		return ids.UUID{}, err
	}
	return attestation.AttestationID, nil
}

func (s *Store) RecordTenantRuntimeObservation(ctx context.Context, observation TenantRuntimeObservation) error {
	return s.withTx(ctx, func(q *platformdb.Queries) error {
		spec, specParams, err := s.prepareTenantRuntimeSpec(ctx, q, observation.Spec)
		if err != nil {
			return err
		}
		if observation.Measurement.TenantID.IsZero() {
			observation.Measurement.TenantID = spec.TenantID
		}
		if observation.Attestation.TenantID.IsZero() {
			observation.Attestation.TenantID = spec.TenantID
		}
		if err := q.InsertTenantRuntimeSpec(ctx, specParams); err != nil {
			return fmt.Errorf("insert tenant runtime spec %s: %w", spec.TenantID, err)
		}
		measurement, measurementParams := prepareTenantRuntimeMeasurement(observation.Measurement)
		if err := q.InsertTenantRuntimeMeasurement(ctx, measurementParams); err != nil {
			return fmt.Errorf("insert tenant runtime measurement %s: %w", measurement.TenantID, err)
		}
		observation.Attestation.SpecID = spec.SpecID
		observation.Attestation.MeasurementID = measurement.MeasurementID
		attestation, attestationParams := prepareTenantRuntimeAttestation(observation.Attestation)
		if err := q.InsertTenantRuntimeAttestation(ctx, attestationParams); err != nil {
			return fmt.Errorf("insert tenant runtime attestation %s: %w", attestation.TenantID, err)
		}
		if err := q.MarkTenantAttestationSummary(ctx, platformdb.MarkTenantAttestationSummaryParams{
			Verdict:       attestation.Verdict,
			AttestedAt:    sqlNullTime(attestation.CreatedAt),
			AttestationID: attestation.AttestationID.Bytes(),
			SpecID:        nullableUUIDBytes(spec.SpecID),
			TenantID:      spec.TenantID.Bytes(),
		}); err != nil {
			return fmt.Errorf("mark tenant attestation summary %s: %w", spec.TenantID, err)
		}
		return nil
	})
}

func (s *Store) prepareTenantRuntimeSpec(ctx context.Context, q *platformdb.Queries, spec TenantRuntimeSpec) (TenantRuntimeSpec, platformdb.InsertTenantRuntimeSpecParams, error) {
	if spec.SpecID.IsZero() {
		spec.SpecID = ids.New()
	}
	record, err := q.GetTenantInstance(ctx, platformdb.GetTenantInstanceParams{TenantID: spec.TenantID.Bytes()})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return TenantRuntimeSpec{}, platformdb.InsertTenantRuntimeSpecParams{}, ErrTenantInstanceNotFound
		}
		return TenantRuntimeSpec{}, platformdb.InsertTenantRuntimeSpecParams{}, fmt.Errorf("get tenant instance %s: %w", spec.TenantID, err)
	}
	if spec.ServiceID != "" && spec.ServiceID != record.ServiceID || spec.SubjectSub != "" && spec.SubjectSub != record.SubjectSub {
		return TenantRuntimeSpec{}, platformdb.InsertTenantRuntimeSpecParams{}, fmt.Errorf("tenant runtime spec identity mismatch")
	}
	spec.ServiceID = record.ServiceID
	spec.SubjectSub = record.SubjectSub
	if spec.CreatedAt.IsZero() {
		spec.CreatedAt = time.Now().UTC()
	}
	return spec, platformdb.InsertTenantRuntimeSpecParams{
		SpecID:              spec.SpecID.Bytes(),
		TenantID:            spec.TenantID.Bytes(),
		ServiceID:           spec.ServiceID,
		SubjectSub:          spec.SubjectSub,
		SpecVersion:         spec.SpecVersion,
		ComposeHash:         spec.ComposeHash,
		EnvContractHash:     spec.EnvContractHash,
		SecretContractHash:  spec.SecretContractHash,
		ImageRefsJson:       jsonRawOrDefault(spec.ImageRefsJSON, "[]"),
		NetworkPolicyJson:   jsonRawOrDefault(spec.NetworkPolicyJSON, "{}"),
		IdentityContextHash: spec.IdentityContextHash,
		CreatedAt:           formatSQLiteTime(spec.CreatedAt),
	}, nil
}

func prepareTenantRuntimeMeasurement(measurement TenantRuntimeMeasurement) (TenantRuntimeMeasurement, platformdb.InsertTenantRuntimeMeasurementParams) {
	if measurement.MeasurementID.IsZero() {
		measurement.MeasurementID = ids.New()
	}
	if measurement.MeasuredAt.IsZero() {
		measurement.MeasuredAt = time.Now().UTC()
	}
	if measurement.HealthStatus == "" {
		measurement.HealthStatus = "unknown"
	}
	return measurement, platformdb.InsertTenantRuntimeMeasurementParams{
		MeasurementID:     measurement.MeasurementID.Bytes(),
		TenantID:          measurement.TenantID.Bytes(),
		CoolifyResourceID: sqlNullString(measurement.CoolifyResourceID),
		ContainerID:       sqlNullString(measurement.ContainerID),
		Source:            measurement.Source,
		ImageRef:          sqlNullString(measurement.ImageRef),
		ImageDigest:       sqlNullString(measurement.ImageDigest),
		ComposeHash:       sqlNullString(measurement.ComposeHash),
		EnvContractHash:   sqlNullString(measurement.EnvContractHash),
		NetworkJson:       jsonRawOrDefault(measurement.NetworkJSON, "{}"),
		PortsJson:         jsonRawOrDefault(measurement.PortsJSON, "{}"),
		VolumesJson:       jsonRawOrDefault(measurement.VolumesJSON, "{}"),
		HealthStatus:      measurement.HealthStatus,
		RawSummaryJson:    jsonRawOrDefault(measurement.RawSummaryJSON, "{}"),
		MeasuredAt:        formatSQLiteTime(measurement.MeasuredAt),
	}
}

func prepareTenantRuntimeAttestation(attestation TenantRuntimeAttestation) (TenantRuntimeAttestation, platformdb.InsertTenantRuntimeAttestationParams) {
	if attestation.AttestationID.IsZero() {
		attestation.AttestationID = ids.New()
	}
	if attestation.CreatedAt.IsZero() {
		attestation.CreatedAt = time.Now().UTC()
	}
	if attestation.Verdict == "" {
		attestation.Verdict = "unknown"
	}
	return attestation, platformdb.InsertTenantRuntimeAttestationParams{
		AttestationID:      attestation.AttestationID.Bytes(),
		TenantID:           attestation.TenantID.Bytes(),
		SpecID:             nullableUUIDBytes(attestation.SpecID),
		MeasurementID:      nullableUUIDBytes(attestation.MeasurementID),
		PolicyVersion:      attestation.PolicyVersion,
		Verdict:            attestation.Verdict,
		FailureReasonsJson: jsonRawOrDefault(attestation.FailureReasonsJSON, "[]"),
		ExpiresAt:          sqlNullTimePtr(attestation.ExpiresAt),
		CreatedAt:          formatSQLiteTime(attestation.CreatedAt),
	}
}

func (s *Store) GetTenantRuntimeAttestation(ctx context.Context, attestationID ids.UUID) (TenantRuntimeAttestation, error) {
	record, err := s.queries.GetTenantRuntimeAttestation(ctx, platformdb.GetTenantRuntimeAttestationParams{AttestationID: attestationID.Bytes()})
	if err != nil {
		return TenantRuntimeAttestation{}, fmt.Errorf("get tenant runtime attestation %s: %w", attestationID, err)
	}
	return convertTenantRuntimeAttestation(record)
}

func (s *Store) GetLatestTenantRuntimeAttestation(ctx context.Context, tenantID ids.UUID) (TenantRuntimeAttestation, error) {
	record, err := s.queries.GetLatestTenantRuntimeAttestation(ctx, platformdb.GetLatestTenantRuntimeAttestationParams{TenantID: tenantID.Bytes()})
	if err != nil {
		return TenantRuntimeAttestation{}, fmt.Errorf("get latest tenant runtime attestation %s: %w", tenantID, err)
	}
	return convertTenantRuntimeAttestation(record)
}

func (s *Store) withTx(ctx context.Context, fn func(*platformdb.Queries) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(s.queries.WithTx(tx)); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func insertGrants(ctx context.Context, q *platformdb.Queries, subjectSub string, grants []ServiceGrant) error {
	for _, grant := range grants {
		grantedAt := grant.GrantedAt
		if grantedAt.IsZero() {
			grantedAt = time.Now().UTC()
		}
		lastSyncedAt := grant.LastSyncedAt
		if lastSyncedAt.IsZero() {
			lastSyncedAt = time.Now().UTC()
		}
		if err := q.InsertServiceGrantSource(ctx, platformdb.InsertServiceGrantSourceParams{SubjectSub: subjectSub, ServiceID: grant.ServiceID, SourceGroup: grant.SourceGroup, GrantedAt: formatSQLiteTime(grantedAt), LastSyncedAt: formatSQLiteTime(lastSyncedAt)}); err != nil {
			return fmt.Errorf("insert grant %s/%s: %w", subjectSub, grant.ServiceID, err)
		}
	}
	return nil
}

func rebuildEffectiveServiceGrants(ctx context.Context, q *platformdb.Queries) error {
	if err := q.RebuildEffectiveServiceGrants(ctx); err != nil {
		return fmt.Errorf("clear effective service grants: %w", err)
	}
	if err := q.InsertEffectiveServiceGrantsFromSources(ctx); err != nil {
		return fmt.Errorf("insert effective service grants: %w", err)
	}
	return nil
}

func loadDesiredTenantSpecs(ctx context.Context, q *platformdb.Queries) (map[string]desiredTenantSpec, error) {
	rows, err := q.ListDesiredTenantSpecs(ctx)
	if err != nil {
		return nil, fmt.Errorf("load desired tenant specs: %w", err)
	}
	desiredSpecs := make(map[string]desiredTenantSpec, len(rows))
	for _, row := range rows {
		spec := desiredTenantSpec{subject: domain.Subject{Sub: row.SubjectSub, SubjectKey: row.SubjectKey, PreferredUsername: row.PreferredUsername, Email: row.Email, DisplayName: row.DisplayName, AccountBindingID: row.AccountBindingID, AccountBindingClaim: row.AccountBindingClaim}, serviceID: row.ServiceID}
		desiredSpecs[tenantMapKey(row.SubjectSub, row.ServiceID)] = spec
	}
	return desiredSpecs, nil
}

func loadTenantInstances(ctx context.Context, q *platformdb.Queries) (map[string]TenantInstance, error) {
	records, err := q.ListTenantInstances(ctx)
	if err != nil {
		return nil, fmt.Errorf("load current tenant instances: %w", err)
	}
	tenants, err := convertTenantInstances(records)
	if err != nil {
		return nil, err
	}
	tenantMap := make(map[string]TenantInstance, len(tenants))
	for _, tenant := range tenants {
		tenantMap[tenantMapKey(tenant.SubjectSub, tenant.ServiceID)] = tenant
	}
	return tenantMap, nil
}

func insertTenantInstance(ctx context.Context, q *platformdb.Queries, spec desiredTenantSpec) error {
	tenantInstanceName := domain.BuildTenantInstanceName(spec.serviceID, spec.subject.SubjectKey)
	if err := q.InsertTenantInstance(ctx, platformdb.InsertTenantInstanceParams{
		TenantID:           ids.New().Bytes(),
		SubjectSub:         spec.subject.Sub,
		ServiceID:          spec.serviceID,
		SubjectKey:         spec.subject.SubjectKey,
		TenantInstanceName: tenantInstanceName,
		InternalDnsName:    tenantInstanceName,
		DesiredState:       string(domain.TenantDesiredStateEnabled),
		RuntimeState:       string(domain.TenantRuntimeStateProvisioning),
	}); err != nil {
		return fmt.Errorf("insert tenant instance %s/%s: %w", spec.subject.Sub, spec.serviceID, err)
	}
	return nil
}

func enableTenantInstance(ctx context.Context, q *platformdb.Queries, tenant TenantInstance, spec desiredTenantSpec) error {
	tenantInstanceName := domain.BuildTenantInstanceName(spec.serviceID, spec.subject.SubjectKey)
	runtimeState := tenant.RuntimeState
	lastError := tenant.LastError
	if tenant.SubjectKey != spec.subject.SubjectKey || tenant.TenantInstanceName != tenantInstanceName || tenant.InternalDNSName != tenantInstanceName {
		runtimeState = domain.TenantRuntimeStateDegraded
		lastError = "tenant identity drift detected; reprovision required"
	}
	if err := q.EnableTenantInstance(ctx, platformdb.EnableTenantInstanceParams{
		TenantID:           tenant.TenantID.Bytes(),
		SubjectKey:         spec.subject.SubjectKey,
		TenantInstanceName: tenantInstanceName,
		InternalDnsName:    tenantInstanceName,
		DesiredState:       string(domain.TenantDesiredStateEnabled),
		RuntimeState:       string(runtimeState),
		LastError:          lastError,
	}); err != nil {
		return fmt.Errorf("enable tenant instance %s: %w", tenant.TenantID, err)
	}
	return nil
}

func ensureNoPublicPathConflict(ctx context.Context, q *platformdb.Queries, serviceID string, publicPath string) error {
	records, err := q.ListServiceCatalog(ctx)
	if err != nil {
		return fmt.Errorf("list service catalog for path conflict check: %w", err)
	}
	publicPath = strings.TrimRight(publicPath, "/")
	for _, record := range records {
		if record.ServiceID == serviceID || record.Enabled == 0 {
			continue
		}
		existingPath := strings.TrimRight(record.PublicPath, "/")
		if publicPath == existingPath || strings.HasPrefix(publicPath, existingPath+"/") || strings.HasPrefix(existingPath, publicPath+"/") {
			return fmt.Errorf("%w: %s overlaps %s", ErrCatalogPathConflict, publicPath, existingPath)
		}
	}
	return nil
}

func convertServiceCatalogAdminEntries(records []platformdb.ListServiceCatalogRow) ([]ServiceCatalogAdminEntry, error) {
	entries := make([]ServiceCatalogAdminEntry, 0, len(records))
	for _, record := range records {
		entry := ServiceCatalogAdminEntry{
			ServiceID:              record.ServiceID,
			DisplayName:            record.DisplayName,
			UpstreamServiceName:    record.UpstreamServiceName,
			TransportType:          catalog.TransportType(record.TransportType),
			InternalPort:           int(record.InternalPort),
			PublicPath:             record.PublicPath,
			InternalUpstreamPath:   record.InternalUpstreamPath,
			HealthPath:             record.HealthPath,
			HealthProbeExpectation: record.HealthProbeExpectation,
			ResourceProfile:        record.ResourceProfile,
			PersistencePolicy:      record.PersistencePolicy,
			AdapterRequirement:     catalog.AdapterRequirement(record.AdapterRequirement),
			IdentityContext:        catalog.IdentityContextConfig{},
			Enabled:                record.Enabled != 0,
			Source:                 record.Source,
		}
		if err := json.Unmarshal([]byte(record.SecretContract), &entry.SecretContract); err != nil {
			return nil, fmt.Errorf("decode secret contract for %s: %w", entry.ServiceID, err)
		}
		if err := json.Unmarshal([]byte(record.IdentityContext), &entry.IdentityContext); err != nil {
			return nil, fmt.Errorf("decode identity context for %s: %w", entry.ServiceID, err)
		}
		entry.IdentityContext = entry.IdentityContext.Normalized()
		entries = append(entries, entry)
	}
	return entries, nil
}

func convertServiceCatalogAdminEntry(record platformdb.GetServiceCatalogEntryRow) (ServiceCatalogAdminEntry, error) {
	entry := ServiceCatalogAdminEntry{
		ServiceID:              record.ServiceID,
		DisplayName:            record.DisplayName,
		UpstreamServiceName:    record.UpstreamServiceName,
		TransportType:          catalog.TransportType(record.TransportType),
		InternalPort:           int(record.InternalPort),
		PublicPath:             record.PublicPath,
		InternalUpstreamPath:   record.InternalUpstreamPath,
		HealthPath:             record.HealthPath,
		HealthProbeExpectation: record.HealthProbeExpectation,
		ResourceProfile:        record.ResourceProfile,
		PersistencePolicy:      record.PersistencePolicy,
		AdapterRequirement:     catalog.AdapterRequirement(record.AdapterRequirement),
		IdentityContext:        catalog.IdentityContextConfig{},
		Enabled:                record.Enabled != 0,
		Source:                 record.Source,
	}
	if err := json.Unmarshal([]byte(record.SecretContract), &entry.SecretContract); err != nil {
		return ServiceCatalogAdminEntry{}, fmt.Errorf("decode secret contract for %s: %w", entry.ServiceID, err)
	}
	if err := json.Unmarshal([]byte(record.IdentityContext), &entry.IdentityContext); err != nil {
		return ServiceCatalogAdminEntry{}, fmt.Errorf("decode identity context for %s: %w", entry.ServiceID, err)
	}
	entry.IdentityContext = entry.IdentityContext.Normalized()
	return entry, nil
}

func convertEnabledServiceCatalogRecord(record platformdb.GetEnabledServiceCatalogEntryRow) (catalog.ServiceCatalogEntry, error) {
	entry := catalog.ServiceCatalogEntry{
		ServiceID:              record.ServiceID,
		DisplayName:            record.DisplayName,
		UpstreamServiceName:    record.UpstreamServiceName,
		TransportType:          catalog.TransportType(record.TransportType),
		InternalPort:           int(record.InternalPort),
		PublicPath:             record.PublicPath,
		InternalUpstreamPath:   record.InternalUpstreamPath,
		HealthPath:             record.HealthPath,
		HealthProbeExpectation: record.HealthProbeExpectation,
		ResourceProfile:        record.ResourceProfile,
		PersistencePolicy:      record.PersistencePolicy,
		AdapterRequirement:     catalog.AdapterRequirement(record.AdapterRequirement),
		IdentityContext:        catalog.IdentityContextConfig{},
	}
	if err := json.Unmarshal([]byte(record.SecretContract), &entry.SecretContract); err != nil {
		return catalog.ServiceCatalogEntry{}, fmt.Errorf("decode secret contract for %s: %w", entry.ServiceID, err)
	}
	if err := json.Unmarshal([]byte(record.IdentityContext), &entry.IdentityContext); err != nil {
		return catalog.ServiceCatalogEntry{}, fmt.Errorf("decode identity context for %s: %w", entry.ServiceID, err)
	}
	entry.IdentityContext = entry.IdentityContext.Normalized()
	return entry, nil
}

func convertTenantInstances(records []platformdb.ListTenantInstancesRow) ([]TenantInstance, error) {
	tenants := make([]TenantInstance, 0, len(records))
	for _, record := range records {
		tenantID, err := ids.ParseBytes(record.TenantID)
		if err != nil {
			return nil, fmt.Errorf("parse tenant id: %w", err)
		}
		createdAt, err := parseSQLiteTime(record.CreatedAt)
		if err != nil {
			return nil, fmt.Errorf("parse tenant created_at: %w", err)
		}
		updatedAt, err := parseSQLiteTime(record.UpdatedAt)
		if err != nil {
			return nil, fmt.Errorf("parse tenant updated_at: %w", err)
		}
		tenant := TenantInstance{
			TenantID:             tenantID,
			SubjectSub:           record.SubjectSub,
			ServiceID:            record.ServiceID,
			SubjectKey:           record.SubjectKey,
			TenantInstanceName:   record.TenantInstanceName,
			InternalDNSName:      record.InternalDnsName,
			DesiredState:         domain.TenantDesiredState(record.DesiredState),
			RuntimeState:         domain.TenantRuntimeState(record.RuntimeState),
			CoolifyResourceID:    record.CoolifyResourceID.String,
			CoolifyApplicationID: record.CoolifyApplicationID.String,
			UpstreamURL:          record.UpstreamUrl.String,
			SecretVersion:        record.SecretVersion.String,
			LastError:            record.LastError.String,
			Metadata:             json.RawMessage(record.Metadata),
			AttestationState:     record.AttestationState,
			CreatedAt:            createdAt,
			UpdatedAt:            updatedAt,
		}
		if value, ok, err := parseSQLiteNullTime(record.LastHealthyAt); err != nil {
			return nil, fmt.Errorf("parse tenant last_healthy_at: %w", err)
		} else if ok {
			tenant.LastHealthyAt = &value
		}
		if value, ok, err := parseSQLiteNullTime(record.LastReconciledAt); err != nil {
			return nil, fmt.Errorf("parse tenant last_reconciled_at: %w", err)
		} else if ok {
			tenant.LastReconciledAt = &value
		}
		if value, ok, err := parseSQLiteNullTime(record.LastAttestedAt); err != nil {
			return nil, fmt.Errorf("parse tenant last_attested_at: %w", err)
		} else if ok {
			tenant.LastAttestedAt = &value
		}
		if len(record.LastAttestationID) > 0 {
			lastAttestationID, err := ids.ParseBytes(record.LastAttestationID)
			if err != nil {
				return nil, fmt.Errorf("parse tenant last_attestation_id: %w", err)
			}
			tenant.LastAttestationID = lastAttestationID
		}
		if len(record.RuntimeSpecID) > 0 {
			runtimeSpecID, err := ids.ParseBytes(record.RuntimeSpecID)
			if err != nil {
				return nil, fmt.Errorf("parse tenant runtime_spec_id: %w", err)
			}
			tenant.RuntimeSpecID = runtimeSpecID
		}
		tenants = append(tenants, tenant)
	}
	return tenants, nil
}

func convertMemoryBankProjects(records []platformdb.MemoryBankProject) ([]MemoryBankProject, error) {
	projects := make([]MemoryBankProject, 0, len(records))
	for _, record := range records {
		project, err := convertMemoryBankProject(record)
		if err != nil {
			return nil, err
		}
		projects = append(projects, project)
	}
	return projects, nil
}

func convertMemoryBankProject(record platformdb.MemoryBankProject) (MemoryBankProject, error) {
	projectID, err := ids.ParseBytes(record.ProjectID)
	if err != nil {
		return MemoryBankProject{}, fmt.Errorf("parse memory bank project id: %w", err)
	}
	ownerTenantID, err := ids.ParseBytes(record.OwnerTenantID)
	if err != nil {
		return MemoryBankProject{}, fmt.Errorf("parse memory bank owner tenant id: %w", err)
	}
	createdAt, err := parseSQLiteTime(record.CreatedAt)
	if err != nil {
		return MemoryBankProject{}, fmt.Errorf("parse memory bank project created_at: %w", err)
	}
	updatedAt, err := parseSQLiteTime(record.UpdatedAt)
	if err != nil {
		return MemoryBankProject{}, fmt.Errorf("parse memory bank project updated_at: %w", err)
	}
	project := MemoryBankProject{
		ProjectID:       projectID,
		OwnerSubjectSub: record.OwnerSubjectSub,
		OwnerTenantID:   ownerTenantID,
		ServiceID:       record.ServiceID,
		ProjectKey:      record.ProjectKey,
		DisplayName:     record.DisplayName,
		RootPath:        record.RootPath,
		Metadata:        json.RawMessage(record.Metadata),
		CreatedAt:       createdAt,
		UpdatedAt:       updatedAt,
	}
	if archivedAt, ok, err := parseSQLiteNullTime(record.ArchivedAt); err != nil {
		return MemoryBankProject{}, fmt.Errorf("parse memory bank project archived_at: %w", err)
	} else if ok {
		project.ArchivedAt = &archivedAt
	}
	return project, nil
}

func convertMemoryBankProjectShares(records []platformdb.MemoryBankProjectShare) ([]MemoryBankProjectShare, error) {
	shares := make([]MemoryBankProjectShare, 0, len(records))
	for _, record := range records {
		share, err := convertMemoryBankProjectShare(record)
		if err != nil {
			return nil, err
		}
		shares = append(shares, share)
	}
	return shares, nil
}

func convertMemoryBankProjectShare(record platformdb.MemoryBankProjectShare) (MemoryBankProjectShare, error) {
	shareID, err := ids.ParseBytes(record.ShareID)
	if err != nil {
		return MemoryBankProjectShare{}, fmt.Errorf("parse memory bank share id: %w", err)
	}
	projectID, err := ids.ParseBytes(record.ProjectID)
	if err != nil {
		return MemoryBankProjectShare{}, fmt.Errorf("parse memory bank share project id: %w", err)
	}
	createdAt, err := parseSQLiteTime(record.CreatedAt)
	if err != nil {
		return MemoryBankProjectShare{}, fmt.Errorf("parse memory bank share created_at: %w", err)
	}
	updatedAt, err := parseSQLiteTime(record.UpdatedAt)
	if err != nil {
		return MemoryBankProjectShare{}, fmt.Errorf("parse memory bank share updated_at: %w", err)
	}
	share := MemoryBankProjectShare{
		ShareID:                shareID,
		ProjectID:              projectID,
		OwnerSubjectSub:        record.OwnerSubjectSub,
		CollaboratorSubjectSub: record.CollaboratorSubjectSub,
		Permission:             record.Permission,
		State:                  record.State,
		Source:                 record.Source,
		CreatedBySubjectSub:    record.CreatedBySubjectSub,
		Metadata:               json.RawMessage(record.Metadata),
		CreatedAt:              createdAt,
		UpdatedAt:              updatedAt,
	}
	if acceptedAt, ok, err := parseSQLiteNullTime(record.AcceptedAt); err != nil {
		return MemoryBankProjectShare{}, fmt.Errorf("parse memory bank share accepted_at: %w", err)
	} else if ok {
		share.AcceptedAt = &acceptedAt
	}
	if revokedAt, ok, err := parseSQLiteNullTime(record.RevokedAt); err != nil {
		return MemoryBankProjectShare{}, fmt.Errorf("parse memory bank share revoked_at: %w", err)
	} else if ok {
		share.RevokedAt = &revokedAt
	}
	if expiresAt, ok, err := parseSQLiteNullTime(record.ExpiresAt); err != nil {
		return MemoryBankProjectShare{}, fmt.Errorf("parse memory bank share expires_at: %w", err)
	} else if ok {
		share.ExpiresAt = &expiresAt
	}
	return share, nil
}

func convertTenantRuntimeSpecs(records []platformdb.TenantRuntimeSpec) ([]TenantRuntimeSpec, error) {
	specs := make([]TenantRuntimeSpec, 0, len(records))
	for _, record := range records {
		spec, err := convertTenantRuntimeSpec(record)
		if err != nil {
			return nil, err
		}
		specs = append(specs, spec)
	}
	return specs, nil
}

func convertTenantRuntimeSpec(record platformdb.TenantRuntimeSpec) (TenantRuntimeSpec, error) {
	specID, err := ids.ParseBytes(record.SpecID)
	if err != nil {
		return TenantRuntimeSpec{}, fmt.Errorf("parse runtime spec id: %w", err)
	}
	tenantID, err := ids.ParseBytes(record.TenantID)
	if err != nil {
		return TenantRuntimeSpec{}, fmt.Errorf("parse runtime spec tenant id: %w", err)
	}
	createdAt, err := parseSQLiteTime(record.CreatedAt)
	if err != nil {
		return TenantRuntimeSpec{}, fmt.Errorf("parse runtime spec created_at: %w", err)
	}
	return TenantRuntimeSpec{
		SpecID:              specID,
		TenantID:            tenantID,
		ServiceID:           record.ServiceID,
		SubjectSub:          record.SubjectSub,
		SpecVersion:         record.SpecVersion,
		ComposeHash:         record.ComposeHash,
		EnvContractHash:     record.EnvContractHash,
		SecretContractHash:  record.SecretContractHash,
		ImageRefsJSON:       json.RawMessage(record.ImageRefsJson),
		NetworkPolicyJSON:   json.RawMessage(record.NetworkPolicyJson),
		IdentityContextHash: record.IdentityContextHash,
		CreatedAt:           createdAt,
	}, nil
}

func convertTenantRuntimeMeasurements(records []platformdb.TenantRuntimeMeasurement) ([]TenantRuntimeMeasurement, error) {
	measurements := make([]TenantRuntimeMeasurement, 0, len(records))
	for _, record := range records {
		measurement, err := convertTenantRuntimeMeasurement(record)
		if err != nil {
			return nil, err
		}
		measurements = append(measurements, measurement)
	}
	return measurements, nil
}

func convertTenantRuntimeMeasurement(record platformdb.TenantRuntimeMeasurement) (TenantRuntimeMeasurement, error) {
	measurementID, err := ids.ParseBytes(record.MeasurementID)
	if err != nil {
		return TenantRuntimeMeasurement{}, fmt.Errorf("parse runtime measurement id: %w", err)
	}
	tenantID, err := ids.ParseBytes(record.TenantID)
	if err != nil {
		return TenantRuntimeMeasurement{}, fmt.Errorf("parse runtime measurement tenant id: %w", err)
	}
	measuredAt, err := parseSQLiteTime(record.MeasuredAt)
	if err != nil {
		return TenantRuntimeMeasurement{}, fmt.Errorf("parse runtime measurement measured_at: %w", err)
	}
	return TenantRuntimeMeasurement{
		MeasurementID:     measurementID,
		TenantID:          tenantID,
		CoolifyResourceID: record.CoolifyResourceID.String,
		ContainerID:       record.ContainerID.String,
		Source:            record.Source,
		ImageRef:          record.ImageRef.String,
		ImageDigest:       record.ImageDigest.String,
		ComposeHash:       record.ComposeHash.String,
		EnvContractHash:   record.EnvContractHash.String,
		NetworkJSON:       json.RawMessage(record.NetworkJson),
		PortsJSON:         json.RawMessage(record.PortsJson),
		VolumesJSON:       json.RawMessage(record.VolumesJson),
		HealthStatus:      record.HealthStatus,
		RawSummaryJSON:    json.RawMessage(record.RawSummaryJson),
		MeasuredAt:        measuredAt,
	}, nil
}

func convertTenantRuntimeAttestation(record platformdb.TenantRuntimeAttestation) (TenantRuntimeAttestation, error) {
	attestationID, err := ids.ParseBytes(record.AttestationID)
	if err != nil {
		return TenantRuntimeAttestation{}, fmt.Errorf("parse runtime attestation id: %w", err)
	}
	tenantID, err := ids.ParseBytes(record.TenantID)
	if err != nil {
		return TenantRuntimeAttestation{}, fmt.Errorf("parse runtime attestation tenant id: %w", err)
	}
	createdAt, err := parseSQLiteTime(record.CreatedAt)
	if err != nil {
		return TenantRuntimeAttestation{}, fmt.Errorf("parse runtime attestation created_at: %w", err)
	}
	attestation := TenantRuntimeAttestation{
		AttestationID:      attestationID,
		TenantID:           tenantID,
		PolicyVersion:      record.PolicyVersion,
		Verdict:            record.Verdict,
		FailureReasonsJSON: json.RawMessage(record.FailureReasonsJson),
		CreatedAt:          createdAt,
	}
	if len(record.SpecID) > 0 {
		specID, err := ids.ParseBytes(record.SpecID)
		if err != nil {
			return TenantRuntimeAttestation{}, fmt.Errorf("parse runtime attestation spec id: %w", err)
		}
		attestation.SpecID = specID
	}
	if len(record.MeasurementID) > 0 {
		measurementID, err := ids.ParseBytes(record.MeasurementID)
		if err != nil {
			return TenantRuntimeAttestation{}, fmt.Errorf("parse runtime attestation measurement id: %w", err)
		}
		attestation.MeasurementID = measurementID
	}
	if expiresAt, ok, err := parseSQLiteNullTime(record.ExpiresAt); err != nil {
		return TenantRuntimeAttestation{}, fmt.Errorf("parse runtime attestation expires_at: %w", err)
	} else if ok {
		attestation.ExpiresAt = &expiresAt
	}
	return attestation, nil
}

func tenantMapKey(subjectSub string, serviceID string) string { return subjectSub + "::" + serviceID }

func mapsSortedKeys[T any](input map[string]T) []string {
	keys := make([]string, 0, len(input))
	for key := range input {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

func sqlNullString(value string) sql.NullString {
	return sql.NullString{String: value, Valid: value != ""}
}

func sqlNullTime(value time.Time) sql.NullString {
	if value.IsZero() {
		return sql.NullString{}
	}
	return sql.NullString{String: formatSQLiteTime(value), Valid: true}
}

func sqlNullTimePtr(value *time.Time) sql.NullString {
	if value == nil || value.IsZero() {
		return sql.NullString{}
	}
	return sql.NullString{String: formatSQLiteTime(*value), Valid: true}
}

func nullableUUIDBytes(value ids.UUID) []byte {
	if value.IsZero() {
		return nil
	}
	return value.Bytes()
}

func jsonRawOrDefault(value json.RawMessage, defaultValue string) string {
	if strings.TrimSpace(string(value)) == "" {
		return defaultValue
	}
	return string(value)
}

func formatSQLiteTime(value time.Time) string { return value.UTC().Format(time.RFC3339Nano) }

func parseSQLiteNullTime(value sql.NullString) (time.Time, bool, error) {
	if !value.Valid || value.String == "" {
		return time.Time{}, false, nil
	}
	parsed, err := parseSQLiteTime(value.String)
	return parsed, err == nil, err
}

func parseSQLiteTime(value string) (time.Time, error) {
	if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return parsed, nil
	}
	return time.ParseInLocation("2006-01-02 15:04:05", value, time.UTC)
}
