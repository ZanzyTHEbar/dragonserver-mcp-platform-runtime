package policy

import (
	"context"
	"time"
)

type Engine interface {
	Evaluate(context.Context, Request) (Decision, error)
}

type Effect string

const (
	EffectAllow Effect = "allow"
	EffectDeny  Effect = "deny"
)

type Action string

const (
	ActionOAuthAuthorize     Action = "oauth.authorize"
	ActionOAuthDeviceApprove Action = "oauth.device.approve"
	ActionOAuthTokenRefresh  Action = "oauth.token.refresh"

	ActionServiceInvoke    Action = "mcp.service.invoke"
	ActionToolCall         Action = "mcp.tool.call"
	ActionPromptGet        Action = "mcp.prompt.get"
	ActionResourceRead     Action = "mcp.resource.read"
	ActionStreamOpen       Action = "mcp.stream.open"
	ActionStreamResume     Action = "mcp.stream.resume"
	ActionSessionTerminate Action = "mcp.session.terminate"
)

type CapabilityType string

const (
	CapabilityService  CapabilityType = "service"
	CapabilityTool     CapabilityType = "tool"
	CapabilityPrompt   CapabilityType = "prompt"
	CapabilityResource CapabilityType = "resource"
)

type Request struct {
	DecisionID string

	Subject SubjectRef
	Client  ClientRef
	Session SessionRef
	Service ServiceRef

	Capability CapabilityRef
	Action     Action
	Transport  TransportContext
	Request    RequestContext
	Attributes map[string]any
}

type SubjectRef struct {
	Sub        string
	Key        string
	Groups     []string
	Attributes map[string]any
}

type ClientRef struct {
	ID         string
	Name       string
	Attributes map[string]any
}

type SessionRef struct {
	ID                  string
	Scope               string
	Resource            string
	AuthorizationDetail string
	IssuedAt            time.Time
	ExpiresAt           time.Time
}

type ServiceRef struct {
	ID       string
	Resource string
	Scope    string
	Path     string
}

type CapabilityRef struct {
	Type      CapabilityType
	Name      string
	Scope     string
	Arguments map[string]any
}

type TransportContext struct {
	HTTPMethod string
	Path       string
	RemoteAddr string
	UserAgent  string
	Headers    map[string]string
}

type RequestContext struct {
	MCPMethod string
	ToolName  string
	Prompt    string
	Resource  string
	SessionID string
	Batch     bool
}

type Decision struct {
	Effect      Effect
	Reason      string
	Obligations []Obligation
	Audit       map[string]any
}

type Obligation struct {
	Type       string
	Attributes map[string]any
}

func Allow(reason string) Decision {
	return Decision{Effect: EffectAllow, Reason: reason}
}

func Deny(reason string) Decision {
	return Decision{Effect: EffectDeny, Reason: reason}
}

func (d Decision) Allowed() bool {
	return d.Effect == EffectAllow
}
