package policy

import "strings"

type CapabilityRisk string

const (
	RiskRead      CapabilityRisk = "read"
	RiskWrite     CapabilityRisk = "write"
	RiskAdmin     CapabilityRisk = "admin"
	RiskSensitive CapabilityRisk = "sensitive"
)

type DefaultCapabilityPolicy string

const (
	DefaultAllowWithServiceGrant DefaultCapabilityPolicy = "allow_with_service_grant"
	DefaultRequireExplicitGrant  DefaultCapabilityPolicy = "require_explicit_grant"
	DefaultDeny                  DefaultCapabilityPolicy = "deny"
)

type Capability struct {
	ServiceID     string
	Type          CapabilityType
	Name          string
	Scope         string
	Risk          CapabilityRisk
	DefaultPolicy DefaultCapabilityPolicy
	Metadata      map[string]any
}

func ServiceScope(serviceID string) string {
	serviceID = strings.TrimSpace(serviceID)
	if serviceID == "" {
		return ""
	}
	return "mcp:" + serviceID
}

func ToolScope(serviceID string, toolName string) string {
	return capabilityScope(serviceID, "tool", toolName)
}

func PromptScope(serviceID string, promptName string) string {
	return capabilityScope(serviceID, "prompt", promptName)
}

func ResourceScope(serviceID string, resourcePattern string) string {
	return capabilityScope(serviceID, "resource", resourcePattern)
}

func capabilityScope(serviceID string, kind string, name string) string {
	serviceID = strings.TrimSpace(serviceID)
	name = strings.TrimSpace(name)
	if serviceID == "" || name == "" {
		return ""
	}
	return "mcp:" + serviceID + ":" + kind + ":" + name
}

func ScopeIncludes(scope string, targetScope string) bool {
	targetScope = strings.TrimSpace(targetScope)
	if targetScope == "" {
		return false
	}
	for _, entry := range strings.Fields(scope) {
		if entry == targetScope {
			return true
		}
	}
	return false
}

func ServiceIDsFromScope(scope string) []string {
	seen := make(map[string]struct{})
	ids := make([]string, 0)
	for _, entry := range strings.Fields(scope) {
		if !strings.HasPrefix(entry, "mcp:") {
			continue
		}
		serviceID := strings.TrimSpace(strings.TrimPrefix(entry, "mcp:"))
		if cut := strings.Index(serviceID, ":"); cut >= 0 {
			serviceID = serviceID[:cut]
		}
		if serviceID == "" {
			continue
		}
		if _, ok := seen[serviceID]; ok {
			continue
		}
		seen[serviceID] = struct{}{}
		ids = append(ids, serviceID)
	}
	return ids
}

func RequestedServiceScopes(scope string) ([]string, bool) {
	if strings.TrimSpace(scope) == "" {
		return nil, false
	}
	seen := make(map[string]struct{})
	ids := make([]string, 0, len(strings.Fields(scope)))
	for _, entry := range strings.Fields(scope) {
		if !strings.HasPrefix(entry, "mcp:") {
			return nil, false
		}
		serviceID := strings.TrimSpace(strings.TrimPrefix(entry, "mcp:"))
		if serviceID == "" || strings.Contains(serviceID, ":") {
			return nil, false
		}
		if _, ok := seen[serviceID]; ok {
			continue
		}
		seen[serviceID] = struct{}{}
		ids = append(ids, serviceID)
	}
	return ids, len(ids) > 0
}
