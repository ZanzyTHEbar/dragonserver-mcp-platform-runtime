package edge

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestObserveMCPRequestRestoresBodyAndFindsTool(t *testing.T) {
	t.Parallel()

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"createRecipe"}}`
	req, err := http.NewRequest(http.MethodPost, "/example-a/mcp", strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")

	observation, restored, err := observeMCPRequest(req)
	require.NoError(t, err)
	require.Equal(t, "tools/call", observation.Method)
	require.Equal(t, "createRecipe", observation.ToolName)

	restoredBody, err := io.ReadAll(restored.Body)
	require.NoError(t, err)
	require.Equal(t, body, string(restoredBody))
}

func TestObserveMCPRequestSkipsOversizedBodiesWithoutReading(t *testing.T) {
	t.Parallel()

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"createRecipe"}}`
	req, err := http.NewRequest(http.MethodPost, "/example-a/mcp", strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = maxInspectableMCPBodyBytes + 1

	observation, restored, err := observeMCPRequest(req)
	require.NoError(t, err)
	require.Empty(t, observation.Method)

	restoredBody, err := io.ReadAll(restored.Body)
	require.NoError(t, err)
	require.Equal(t, body, string(restoredBody))
}

func TestDecodeMCPObservationBatch(t *testing.T) {
	t.Parallel()

	observation, err := decodeMCPObservation([]byte(`[
		{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}},
		{"jsonrpc":"2.0","id":2,"method":"resources/read","params":{"uri":"project://fixture"}},
		{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"search_notes"}},
		{"jsonrpc":"2.0","id":4,"method":"prompts/get","params":{"name":"weekly_summary"}}
	]`))
	require.NoError(t, err)
	require.True(t, observation.Batch)
	require.Equal(t, 4, observation.BatchSize)
	require.Equal(t, "initialize", observation.Method)
	require.Equal(t, []string{"initialize", "resources/read", "tools/call", "prompts/get"}, observation.Methods)
	require.Equal(t, "project://fixture", observation.Resource)
	require.Equal(t, []string{"project://fixture"}, observation.Resources)
	require.Equal(t, "search_notes", observation.ToolName)
	require.Equal(t, []string{"search_notes"}, observation.ToolNames)
	require.Equal(t, "weekly_summary", observation.Prompt)
	require.Equal(t, []string{"weekly_summary"}, observation.Prompts)
}
