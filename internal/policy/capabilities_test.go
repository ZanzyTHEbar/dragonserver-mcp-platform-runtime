package policy

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRequestedServiceScopes(t *testing.T) {
	t.Parallel()

	serviceIDs, valid := RequestedServiceScopes("mcp:example-a mcp:example-b mcp:example-a")
	require.True(t, valid)
	require.Equal(t, []string{"example-a", "example-b"}, serviceIDs)

	_, valid = RequestedServiceScopes("")
	require.False(t, valid)

	_, valid = RequestedServiceScopes("openid")
	require.False(t, valid)

	_, valid = RequestedServiceScopes("mcp:example-a:tool:createRecipe")
	require.False(t, valid)
}

func TestScopeIncludes(t *testing.T) {
	t.Parallel()

	require.True(t, ScopeIncludes("mcp:example-a mcp:example-b", "mcp:example-b"))
	require.False(t, ScopeIncludes("mcp:example-a", "mcp:example-b"))
	require.False(t, ScopeIncludes("mcp:example-a", ""))
}
