package controlplane

import (
	"context"

	"github.com/rs/zerolog"
)

type DependencyClients struct {
	Infisical SecretResolver
	Authentik IdentitySyncProvider
	Coolify   CoolifyProvider
}

func NewDependencyClients(ctx context.Context, cfg Config, logger zerolog.Logger) (*DependencyClients, error) {
	infisicalClient, err := NewInfisicalClient(cfg, logger)
	if err != nil {
		return nil, err
	}

	authentikClient, err := NewAuthentikClientFromConfig(ctx, cfg, infisicalClient, logger)
	if err != nil {
		return nil, err
	}

	coolifyClient, err := NewCoolifyClientFromConfig(ctx, cfg, infisicalClient, logger)
	if err != nil {
		return nil, err
	}

	return &DependencyClients{
		Infisical: infisicalClient,
		Authentik: authentikClient,
		Coolify:   coolifyClient,
	}, nil
}
