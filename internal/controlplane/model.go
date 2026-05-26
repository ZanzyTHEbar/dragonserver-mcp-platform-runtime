package controlplane

import (
	"encoding/json"
	"time"

	"dragonserver/mcp-platform/internal/catalog"
	"dragonserver/mcp-platform/internal/domain"
	"dragonserver/mcp-platform/internal/ids"
)

type ServiceCatalogAdminEntry struct {
	ServiceID              string                        `json:"service_id"`
	DisplayName            string                        `json:"display_name"`
	UpstreamServiceName    string                        `json:"upstream_service_name"`
	TransportType          catalog.TransportType         `json:"transport_type"`
	InternalPort           int                           `json:"internal_port"`
	PublicPath             string                        `json:"public_path"`
	InternalUpstreamPath   string                        `json:"internal_upstream_path"`
	HealthPath             string                        `json:"health_path"`
	HealthProbeExpectation string                        `json:"health_probe_expectation"`
	ResourceProfile        string                        `json:"resource_profile"`
	PersistencePolicy      string                        `json:"persistence_policy"`
	AdapterRequirement     catalog.AdapterRequirement    `json:"adapter_requirement"`
	SecretContract         []catalog.SecretDefinition    `json:"secret_contract"`
	IdentityContext        catalog.IdentityContextConfig `json:"identity_context"`
	Enabled                bool                          `json:"enabled"`
	Source                 string                        `json:"source"`
}

type ServiceGrant struct {
	SubjectSub   string    `json:"subject_sub"`
	ServiceID    string    `json:"service_id"`
	SourceGroup  string    `json:"source_group"`
	GrantedAt    time.Time `json:"granted_at"`
	LastSyncedAt time.Time `json:"last_synced_at"`
}

type StaticUpstreamBinding struct {
	SubjectSub  string    `json:"subject_sub"`
	ServiceID   string    `json:"service_id"`
	UpstreamURL string    `json:"upstream_url"`
	VerifiedAt  time.Time `json:"verified_at"`
}

type MemoryBankProject struct {
	ProjectID       ids.UUID        `json:"project_id"`
	OwnerSubjectSub string          `json:"owner_subject_sub"`
	OwnerTenantID   ids.UUID        `json:"owner_tenant_id"`
	ServiceID       string          `json:"service_id"`
	ProjectKey      string          `json:"project_key"`
	DisplayName     string          `json:"display_name"`
	RootPath        string          `json:"root_path"`
	Metadata        json.RawMessage `json:"metadata"`
	ArchivedAt      *time.Time      `json:"archived_at,omitempty"`
	CreatedAt       time.Time       `json:"created_at"`
	UpdatedAt       time.Time       `json:"updated_at"`
}

type MemoryBankProjectShare struct {
	ShareID                ids.UUID        `json:"share_id"`
	ProjectID              ids.UUID        `json:"project_id"`
	OwnerSubjectSub        string          `json:"owner_subject_sub"`
	CollaboratorSubjectSub string          `json:"collaborator_subject_sub"`
	Permission             string          `json:"permission"`
	State                  string          `json:"state"`
	Source                 string          `json:"source"`
	CreatedBySubjectSub    string          `json:"created_by_subject_sub"`
	AcceptedAt             *time.Time      `json:"accepted_at,omitempty"`
	RevokedAt              *time.Time      `json:"revoked_at,omitempty"`
	ExpiresAt              *time.Time      `json:"expires_at,omitempty"`
	Metadata               json.RawMessage `json:"metadata"`
	CreatedAt              time.Time       `json:"created_at"`
	UpdatedAt              time.Time       `json:"updated_at"`
}

type TenantRuntimeSpec struct {
	SpecID              ids.UUID        `json:"spec_id"`
	TenantID            ids.UUID        `json:"tenant_id"`
	ServiceID           string          `json:"service_id"`
	SubjectSub          string          `json:"subject_sub"`
	SpecVersion         string          `json:"spec_version"`
	ComposeHash         string          `json:"compose_hash"`
	EnvContractHash     string          `json:"env_contract_hash"`
	SecretContractHash  string          `json:"secret_contract_hash"`
	ImageRefsJSON       json.RawMessage `json:"image_refs_json"`
	NetworkPolicyJSON   json.RawMessage `json:"network_policy_json"`
	IdentityContextHash string          `json:"identity_context_hash"`
	CreatedAt           time.Time       `json:"created_at"`
}

type TenantRuntimeMeasurement struct {
	MeasurementID     ids.UUID        `json:"measurement_id"`
	TenantID          ids.UUID        `json:"tenant_id"`
	CoolifyResourceID string          `json:"coolify_resource_id,omitempty"`
	ContainerID       string          `json:"container_id,omitempty"`
	Source            string          `json:"source"`
	ImageRef          string          `json:"image_ref,omitempty"`
	ImageDigest       string          `json:"image_digest,omitempty"`
	ComposeHash       string          `json:"compose_hash,omitempty"`
	EnvContractHash   string          `json:"env_contract_hash,omitempty"`
	NetworkJSON       json.RawMessage `json:"network_json"`
	PortsJSON         json.RawMessage `json:"ports_json"`
	VolumesJSON       json.RawMessage `json:"volumes_json"`
	HealthStatus      string          `json:"health_status"`
	RawSummaryJSON    json.RawMessage `json:"raw_summary_json"`
	MeasuredAt        time.Time       `json:"measured_at"`
}

type TenantRuntimeAttestation struct {
	AttestationID      ids.UUID        `json:"attestation_id"`
	TenantID           ids.UUID        `json:"tenant_id"`
	SpecID             ids.UUID        `json:"spec_id,omitempty"`
	MeasurementID      ids.UUID        `json:"measurement_id,omitempty"`
	PolicyVersion      string          `json:"policy_version"`
	Verdict            string          `json:"verdict"`
	FailureReasonsJSON json.RawMessage `json:"failure_reasons_json"`
	ExpiresAt          *time.Time      `json:"expires_at,omitempty"`
	CreatedAt          time.Time       `json:"created_at"`
}

type TenantRuntimeObservation struct {
	Spec        TenantRuntimeSpec
	Measurement TenantRuntimeMeasurement
	Attestation TenantRuntimeAttestation
}

type TenantInstance struct {
	TenantID             ids.UUID
	SubjectSub           string
	ServiceID            string
	SubjectKey           string
	TenantInstanceName   string
	InternalDNSName      string
	DesiredState         domain.TenantDesiredState
	RuntimeState         domain.TenantRuntimeState
	CoolifyResourceID    string
	CoolifyApplicationID string
	UpstreamURL          string
	SecretVersion        string
	LastHealthyAt        *time.Time
	LastReconciledAt     *time.Time
	LastError            string
	Metadata             json.RawMessage
	AttestationState     string
	LastAttestedAt       *time.Time
	LastAttestationID    ids.UUID
	RuntimeSpecID        ids.UUID
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

type TenantRuntimeUpdate struct {
	TenantID               ids.UUID
	RuntimeState           domain.TenantRuntimeState
	CoolifyResourceID      string
	CoolifyApplicationID   string
	UpstreamURL            string
	LastHealthyAt          *time.Time
	ClearRuntimeReferences bool
	LastError              string
}

type ReconcileAction string

const (
	ReconcileActionNoop    ReconcileAction = "noop"
	ReconcileActionEnsure  ReconcileAction = "ensure_present"
	ReconcileActionEnable  ReconcileAction = "enable"
	ReconcileActionDisable ReconcileAction = "disable"
	ReconcileActionDelete  ReconcileAction = "delete"
)

type TenantPlan struct {
	Action ReconcileAction
	Reason string
}

type ReconcileRunInput struct {
	TenantID      ids.UUID
	DesiredState  domain.TenantDesiredState
	ObservedState domain.TenantRuntimeState
	Action        string
	Status        string
	Details       map[string]any
	StartedAt     time.Time
	FinishedAt    time.Time
}

type ReconcileSummary struct {
	Scanned   int
	Applied   int
	Noop      int
	Deferred  int
	Failures  int
	LastRunAt time.Time
}
