package edge

import (
	"context"
	"fmt"

	"dragonserver/mcp-platform/internal/policy"
)

type policyGrantAuthorizer struct {
	engine *policy.ServiceGrantEngine
}

func newPolicyGrantAuthorizer(authorizer policy.ServiceGrantAuthorizer) (*policyGrantAuthorizer, error) {
	if authorizer == nil {
		return nil, fmt.Errorf("policy grant authorizer requires a service grant source")
	}
	return &policyGrantAuthorizer{engine: policy.NewServiceGrantEngine(authorizer)}, nil
}

func (a *policyGrantAuthorizer) Allowed(ctx context.Context, subjectSub string, serviceID string) (bool, error) {
	decision, err := a.engine.Evaluate(ctx, policy.Request{
		Subject: policy.SubjectRef{Sub: subjectSub},
		Service: policy.ServiceRef{ID: serviceID},
		Capability: policy.CapabilityRef{
			Type: policy.CapabilityService,
			Name: serviceID,
		},
		Action: policy.ActionServiceInvoke,
	})
	if err != nil {
		return false, err
	}
	return decision.Allowed(), nil
}

func (a *policyGrantAuthorizer) AllowedScopes(ctx context.Context, subjectSub string, scope string) (bool, error) {
	decision, err := a.engine.EvaluateScopes(ctx, subjectSub, scope)
	if err != nil {
		return false, err
	}
	return decision.Allowed(), nil
}
