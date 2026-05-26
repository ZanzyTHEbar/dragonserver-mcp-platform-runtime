package controlplane

import "context"

type IdentitySyncProvider interface {
	SyncStore(context.Context, *Store) error
}

type CoolifyProvider interface {
	CreateService(context.Context, CoolifyCreateServiceRequest) (CoolifyCreateServiceResponse, error)
	GetService(context.Context, string) (CoolifyService, error)
	UpdateService(context.Context, string, CoolifyUpdateServiceRequest) error
	ListServiceEnvs(context.Context, string) ([]CoolifyEnvVar, error)
	UpdateServiceEnvsBulk(context.Context, string, []CoolifyEnvVar) ([]CoolifyEnvVar, error)
	RestartService(context.Context, string, bool) (CoolifyQueuedActionResponse, error)
	StartService(context.Context, string) (CoolifyQueuedActionResponse, error)
	StopService(context.Context, string, bool) (CoolifyQueuedActionResponse, error)
	DeleteService(context.Context, string, CoolifyDeleteServiceOptions) (CoolifyQueuedActionResponse, error)
}

var _ IdentitySyncProvider = (*AuthentikClient)(nil)
var _ CoolifyProvider = (*CoolifyClient)(nil)
var _ SecretResolver = (*InfisicalClient)(nil)
