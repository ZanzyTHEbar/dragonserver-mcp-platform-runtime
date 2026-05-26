package policy

import (
	"context"
	"fmt"
)

type ServiceGrantAuthorizer interface {
	Allowed(ctx context.Context, subjectSub string, serviceID string) (bool, error)
	AllowedScopes(ctx context.Context, subjectSub string, scope string) (bool, error)
}

type ServiceGrantEngine struct {
	Authorizer ServiceGrantAuthorizer
}

func NewServiceGrantEngine(authorizer ServiceGrantAuthorizer) *ServiceGrantEngine {
	return &ServiceGrantEngine{Authorizer: authorizer}
}

func (e *ServiceGrantEngine) Evaluate(ctx context.Context, req Request) (Decision, error) {
	if e == nil || e.Authorizer == nil {
		return Decision{}, fmt.Errorf("service grant policy authorizer is required")
	}
	if req.Subject.Sub == "" {
		return Deny("missing_subject"), nil
	}
	serviceID := req.Service.ID
	if serviceID == "" && req.Capability.Type == CapabilityService {
		serviceID = req.Capability.Name
	}
	if serviceID == "" {
		return Deny("missing_service"), nil
	}
	allowed, err := e.Authorizer.Allowed(ctx, req.Subject.Sub, serviceID)
	if err != nil {
		return Decision{}, err
	}
	if !allowed {
		return Deny("service_not_granted"), nil
	}
	return Allow("service_grant"), nil
}

func (e *ServiceGrantEngine) EvaluateScopes(ctx context.Context, subjectSub string, scope string) (Decision, error) {
	if e == nil || e.Authorizer == nil {
		return Decision{}, fmt.Errorf("service grant policy authorizer is required")
	}
	if subjectSub == "" {
		return Deny("missing_subject"), nil
	}
	allowed, err := e.Authorizer.AllowedScopes(ctx, subjectSub, scope)
	if err != nil {
		return Decision{}, err
	}
	if !allowed {
		return Deny("scope_not_granted"), nil
	}
	return Allow("service_scope_grant"), nil
}
