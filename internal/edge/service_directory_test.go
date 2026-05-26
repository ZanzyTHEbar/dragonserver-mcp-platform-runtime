package edge

import (
	"context"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

func TestServiceDirectoryCanonicalizesServiceMetadata(t *testing.T) {
	t.Parallel()

	store := newMutableCatalogStore(t)
	cache := NewCatalogCache(store, zerolog.Nop())
	require.NoError(t, cache.Refresh(context.Background()))
	directory := NewServiceDirectory("https://mcp.example.com/", cache)

	resolution, ok := directory.ResolveByID("example-a")
	require.True(t, ok)
	require.Equal(t, "example-a", resolution.ServiceID)
	require.Equal(t, "mcp:example-a", resolution.Scope)
	require.Equal(t, "https://mcp.example.com/example-a/mcp", resolution.Resource)
	require.Equal(t, resolution.Resource, resolution.PublicURL)
	require.Equal(t, "https://mcp.example.com/.well-known/oauth-protected-resource/example-a", resolution.ProtectedResourceMetadataURL)
	require.Equal(t, "https://mcp.example.com/example-a", resolution.AuthorizationServerIssuer)
	require.Equal(t, "https://mcp.example.com/oauth/authorize/example-a", resolution.AuthorizationEndpoint)
	require.Equal(t, "https://mcp.example.com/oauth/device_authorization/example-a", resolution.DeviceAuthorizationEndpoint)
	require.Equal(t, "https://mcp.example.com/oauth/register/example-a", resolution.RegistrationEndpoint)
}

func TestServiceDirectoryResolvesByPathScopeResourceAndWellKnownRef(t *testing.T) {
	t.Parallel()

	store := newMutableCatalogStore(t)
	cache := NewCatalogCache(store, zerolog.Nop())
	require.NoError(t, cache.Refresh(context.Background()))
	directory := NewServiceDirectory("https://mcp.example.com", cache)

	pathResolution, ok := directory.ResolveByPublicPath("/example-b/mcp/tools/list")
	require.True(t, ok)
	require.Equal(t, "example-b", pathResolution.ServiceID)

	scopeResolution, err := directory.ResolveScope("mcp:example-b")
	require.NoError(t, err)
	require.Equal(t, pathResolution, scopeResolution)

	resourceResolution, err := directory.ResolveResource("https://mcp.example.com/example-b/mcp/")
	require.NoError(t, err)
	require.Equal(t, pathResolution, resourceResolution)

	wellKnownByID, scoped, err := directory.ResolveWellKnownRef("/.well-known/oauth-protected-resource/example-b", "/.well-known/oauth-protected-resource")
	require.NoError(t, err)
	require.True(t, scoped)
	require.Equal(t, pathResolution, wellKnownByID)

	wellKnownByPath, scoped, err := directory.ResolveWellKnownRef("/.well-known/oauth-protected-resource/example-b/mcp", "/.well-known/oauth-protected-resource")
	require.NoError(t, err)
	require.True(t, scoped)
	require.Equal(t, pathResolution, wellKnownByPath)

	_, scoped, err = directory.ResolveWellKnownRef("/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource")
	require.NoError(t, err)
	require.False(t, scoped)
}

func TestServiceDirectoryRejectsUnknownResource(t *testing.T) {
	t.Parallel()

	store := newMutableCatalogStore(t)
	cache := NewCatalogCache(store, zerolog.Nop())
	require.NoError(t, cache.Refresh(context.Background()))
	directory := NewServiceDirectory("https://mcp.example.com", cache)

	_, err := directory.ResolveResource("https://mcp.example.com/missing/mcp")
	require.ErrorContains(t, err, "resource indicator is not registered")
}
