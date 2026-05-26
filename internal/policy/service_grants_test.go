package policy

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

type stubServiceGrantAuthorizer struct {
	allowed map[string]bool
}

func (a stubServiceGrantAuthorizer) Allowed(_ context.Context, subjectSub string, serviceID string) (bool, error) {
	return a.allowed[subjectSub+"\x00"+serviceID], nil
}

func (a stubServiceGrantAuthorizer) AllowedScopes(_ context.Context, _ string, _ string) (bool, error) {
	return false, nil
}

func TestServiceGrantEngineServiceFallbackOnlyForServiceCapability(t *testing.T) {
	t.Parallel()

	engine := NewServiceGrantEngine(stubServiceGrantAuthorizer{allowed: map[string]bool{
		"subject-1\x00example-a": true,
	}})

	decision, err := engine.Evaluate(context.Background(), Request{
		Subject: SubjectRef{Sub: "subject-1"},
		Capability: CapabilityRef{
			Type: CapabilityService,
			Name: "example-a",
		},
	})
	require.NoError(t, err)
	require.True(t, decision.Allowed())

	decision, err = engine.Evaluate(context.Background(), Request{
		Subject: SubjectRef{Sub: "subject-1"},
		Capability: CapabilityRef{
			Type: CapabilityTool,
			Name: "example-a",
		},
	})
	require.NoError(t, err)
	require.False(t, decision.Allowed())
	require.Equal(t, "missing_service", decision.Reason)
}

func TestServiceGrantEngineFineGrainedCapabilityUsesExplicitService(t *testing.T) {
	t.Parallel()

	engine := NewServiceGrantEngine(stubServiceGrantAuthorizer{allowed: map[string]bool{
		"subject-1\x00example-a": true,
	}})

	decision, err := engine.Evaluate(context.Background(), Request{
		Subject: SubjectRef{Sub: "subject-1"},
		Service: ServiceRef{ID: "example-a"},
		Capability: CapabilityRef{
			Type: CapabilityTool,
			Name: "search_notes",
		},
		Action: ActionToolCall,
	})
	require.NoError(t, err)
	require.True(t, decision.Allowed())
}
