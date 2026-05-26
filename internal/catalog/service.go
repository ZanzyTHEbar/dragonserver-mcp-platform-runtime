package catalog

type TransportType string

const (
	TransportTypeStreamableHTTP TransportType = "streamable-http"
	TransportTypeSSE            TransportType = "sse"
)

type AdapterRequirement string

const (
	AdapterRequirementNone                AdapterRequirement = "none"
	AdapterRequirementPathTranslation     AdapterRequirement = "path-translation"
	AdapterRequirementSSEToStreamableHTTP AdapterRequirement = "sse-to-streamable-http"
)

type SecretDefinition struct {
	Key      string
	Required bool
}

type IdentityContextMode string

const (
	IdentityContextModeNone          IdentityContextMode = "none"
	IdentityContextModeSignedHeaders IdentityContextMode = "signed-headers"
)

type IdentityContextConfig struct {
	Mode IdentityContextMode `json:"mode"`
}

type CapabilityType string

const (
	CapabilityTypeService  CapabilityType = "service"
	CapabilityTypeTool     CapabilityType = "tool"
	CapabilityTypePrompt   CapabilityType = "prompt"
	CapabilityTypeResource CapabilityType = "resource"
)

type CapabilityRisk string

const (
	CapabilityRiskRead      CapabilityRisk = "read"
	CapabilityRiskWrite     CapabilityRisk = "write"
	CapabilityRiskAdmin     CapabilityRisk = "admin"
	CapabilityRiskSensitive CapabilityRisk = "sensitive"
)

type DefaultCapabilityPolicy string

const (
	DefaultCapabilityAllowWithServiceGrant DefaultCapabilityPolicy = "allow_with_service_grant"
	DefaultCapabilityRequireExplicitGrant  DefaultCapabilityPolicy = "require_explicit_grant"
	DefaultCapabilityDeny                  DefaultCapabilityPolicy = "deny"
)

type ServiceCapability struct {
	Type          CapabilityType          `json:"type"`
	Name          string                  `json:"name"`
	Scope         string                  `json:"scope"`
	Risk          CapabilityRisk          `json:"risk"`
	DefaultPolicy DefaultCapabilityPolicy `json:"default_policy"`
	Metadata      map[string]any          `json:"metadata,omitempty"`
}

func (c IdentityContextConfig) Normalized() IdentityContextConfig {
	if c.Mode == "" {
		c.Mode = IdentityContextModeNone
	}
	return c
}

func (c IdentityContextConfig) Enabled() bool {
	return c.Normalized().Mode != IdentityContextModeNone
}

type ServiceCatalogEntry struct {
	ServiceID              string
	DisplayName            string
	UpstreamServiceName    string
	TransportType          TransportType
	InternalPort           int
	PublicPath             string
	InternalUpstreamPath   string
	HealthPath             string
	HealthProbeExpectation string
	ResourceProfile        string
	PersistencePolicy      string
	AdapterRequirement     AdapterRequirement
	SecretContract         []SecretDefinition
	IdentityContext        IdentityContextConfig
	Capabilities           []ServiceCapability
}

func DefaultCatalogV1() []ServiceCatalogEntry {
	return nil
}
