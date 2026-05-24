package edge

import (
	"fmt"
	"strings"

	"dragonserver/mcp-platform/internal/catalog"
)

type ServiceDirectory struct {
	publicBaseURL string
	catalog       *CatalogCache
}

type ServiceResolution struct {
	Service                      catalog.ServiceCatalogEntry
	ServiceID                    string
	Scope                        string
	Resource                     string
	PublicURL                    string
	ProtectedResourceMetadataURL string
	AuthorizationServerIssuer    string
	AuthorizationEndpoint        string
	DeviceAuthorizationEndpoint  string
	RegistrationEndpoint         string
}

func NewServiceDirectory(publicBaseURL string, catalogCache *CatalogCache) *ServiceDirectory {
	return &ServiceDirectory{
		publicBaseURL: strings.TrimRight(strings.TrimSpace(publicBaseURL), "/"),
		catalog:       catalogCache,
	}
}

func (d *ServiceDirectory) ResolveByID(serviceID string) (ServiceResolution, bool) {
	if d == nil || d.catalog == nil {
		return ServiceResolution{}, false
	}
	service, ok := d.catalog.ServiceByID(strings.TrimSpace(serviceID))
	if !ok {
		return ServiceResolution{}, false
	}
	return d.resolve(service), true
}

func (d *ServiceDirectory) ResolveByPublicPath(path string) (ServiceResolution, bool) {
	if d == nil || d.catalog == nil {
		return ServiceResolution{}, false
	}
	service, ok := d.catalog.MatchPublicPath(path)
	if !ok {
		return ServiceResolution{}, false
	}
	return d.resolve(service), true
}

func (d *ServiceDirectory) ResolveWellKnownRef(path string, prefix string) (ServiceResolution, bool, error) {
	if path == prefix {
		return ServiceResolution{}, false, nil
	}
	if !strings.HasPrefix(path, prefix+"/") {
		return ServiceResolution{}, false, fmt.Errorf("unsupported service-scoped path")
	}

	serviceRef := strings.Trim(strings.TrimPrefix(path, prefix+"/"), "/")
	if serviceRef == "" {
		return ServiceResolution{}, true, fmt.Errorf("requested service is not registered")
	}
	if !strings.Contains(serviceRef, "/") {
		if resolution, ok := d.ResolveByID(serviceRef); ok {
			return resolution, true, nil
		}
	}
	if resolution, ok := d.ResolveByPublicPath("/" + serviceRef); ok {
		return resolution, true, nil
	}
	return ServiceResolution{}, true, fmt.Errorf("requested service is not registered")
}

func (d *ServiceDirectory) ResolveScope(scope string) (ServiceResolution, error) {
	serviceID, err := singleServiceFromScope(scope)
	if err != nil {
		return ServiceResolution{}, err
	}
	resolution, ok := d.ResolveByID(serviceID)
	if !ok {
		return ServiceResolution{}, fmt.Errorf("requested resource scope is not supported")
	}
	return resolution, nil
}

func (d *ServiceDirectory) ResolveResource(resource string) (ServiceResolution, error) {
	resource = strings.TrimRight(strings.TrimSpace(resource), "/")
	if resource == "" {
		return ServiceResolution{}, fmt.Errorf("resource indicator is required")
	}
	if d == nil || d.catalog == nil {
		return ServiceResolution{}, fmt.Errorf("service catalog is unavailable")
	}
	if snapshot := d.catalog.Current(); snapshot != nil {
		for _, service := range snapshot.entries {
			resolution := d.resolve(service)
			if resource == resolution.Resource {
				return resolution, nil
			}
		}
	}
	return ServiceResolution{}, fmt.Errorf("resource indicator is not registered on this edge")
}

func (d *ServiceDirectory) Scopes() []string {
	if d == nil || d.catalog == nil {
		return nil
	}
	return d.catalog.Scopes()
}

func (d *ServiceDirectory) Canonicalize(service catalog.ServiceCatalogEntry) ServiceResolution {
	return d.resolve(service)
}

func (d *ServiceDirectory) resolve(service catalog.ServiceCatalogEntry) ServiceResolution {
	serviceID := strings.TrimSpace(service.ServiceID)
	publicURL := d.publicBaseURL + service.PublicPath
	return ServiceResolution{
		Service:                      service,
		ServiceID:                    serviceID,
		Scope:                        "mcp:" + serviceID,
		Resource:                     publicURL,
		PublicURL:                    publicURL,
		ProtectedResourceMetadataURL: d.publicBaseURL + "/.well-known/oauth-protected-resource/" + serviceID,
		AuthorizationServerIssuer:    d.publicBaseURL + "/" + strings.Trim(serviceID, "/"),
		AuthorizationEndpoint:        d.publicBaseURL + "/oauth/authorize/" + serviceID,
		DeviceAuthorizationEndpoint:  d.publicBaseURL + "/oauth/device_authorization/" + serviceID,
		RegistrationEndpoint:         d.publicBaseURL + "/oauth/register/" + serviceID,
	}
}
