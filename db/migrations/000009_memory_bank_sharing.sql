-- +goose Up
ALTER TABLE oauth_sessions ADD COLUMN authorization_details TEXT NOT NULL DEFAULT '[]' CHECK(json_valid(authorization_details));
ALTER TABLE oauth_sessions ADD COLUMN policy_binding_id TEXT;
ALTER TABLE oauth_sessions ADD COLUMN share_id BLOB CHECK(share_id IS NULL OR length(share_id) = 16);

CREATE UNIQUE INDEX IF NOT EXISTS idx_tenant_instances_identity ON tenant_instances(tenant_id, subject_sub, service_id);

CREATE TABLE IF NOT EXISTS memory_bank_projects (
    project_id BLOB PRIMARY KEY CHECK(length(project_id) = 16),
    owner_subject_sub TEXT NOT NULL REFERENCES subjects(subject_sub) ON DELETE CASCADE,
    owner_tenant_id BLOB NOT NULL REFERENCES tenant_instances(tenant_id) ON DELETE CASCADE CHECK(length(owner_tenant_id) = 16),
    service_id TEXT NOT NULL REFERENCES service_catalog(service_id) ON DELETE CASCADE,
    project_key TEXT NOT NULL,
    display_name TEXT NOT NULL,
    root_path TEXT NOT NULL,
    metadata TEXT NOT NULL DEFAULT '{}' CHECK(json_valid(metadata)),
    archived_at TEXT,
    created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(owner_subject_sub, service_id, project_key),
    UNIQUE(project_id, owner_subject_sub),
    FOREIGN KEY(owner_tenant_id, owner_subject_sub, service_id) REFERENCES tenant_instances(tenant_id, subject_sub, service_id) ON DELETE CASCADE
) STRICT;

CREATE TABLE IF NOT EXISTS memory_bank_project_shares (
    share_id BLOB PRIMARY KEY CHECK(length(share_id) = 16),
    project_id BLOB NOT NULL REFERENCES memory_bank_projects(project_id) ON DELETE CASCADE CHECK(length(project_id) = 16),
    owner_subject_sub TEXT NOT NULL REFERENCES subjects(subject_sub) ON DELETE CASCADE,
    collaborator_subject_sub TEXT NOT NULL REFERENCES subjects(subject_sub) ON DELETE CASCADE,
    permission TEXT NOT NULL CHECK(permission IN ('view', 'edit')),
    state TEXT NOT NULL CHECK(state IN ('pending', 'active', 'revoked', 'expired')),
    source TEXT NOT NULL CHECK(source IN ('oauth_consent', 'admin', 'migration')),
    created_by_subject_sub TEXT NOT NULL REFERENCES subjects(subject_sub) ON DELETE CASCADE,
    accepted_at TEXT,
    revoked_at TEXT,
    expires_at TEXT,
    metadata TEXT NOT NULL DEFAULT '{}' CHECK(json_valid(metadata)),
    created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY(project_id, owner_subject_sub) REFERENCES memory_bank_projects(project_id, owner_subject_sub) ON DELETE CASCADE
) STRICT;

CREATE UNIQUE INDEX IF NOT EXISTS idx_memory_bank_project_shares_active
ON memory_bank_project_shares(project_id, collaborator_subject_sub)
WHERE state IN ('pending', 'active');

CREATE TABLE IF NOT EXISTS memory_bank_share_projections (
    projection_id BLOB PRIMARY KEY CHECK(length(projection_id) = 16),
    share_id BLOB NOT NULL UNIQUE REFERENCES memory_bank_project_shares(share_id) ON DELETE CASCADE CHECK(length(share_id) = 16),
    project_id BLOB NOT NULL REFERENCES memory_bank_projects(project_id) ON DELETE CASCADE CHECK(length(project_id) = 16),
    collaborator_subject_sub TEXT NOT NULL REFERENCES subjects(subject_sub) ON DELETE CASCADE,
    permission TEXT NOT NULL CHECK(permission IN ('view', 'edit')),
    runtime_state TEXT NOT NULL CHECK(runtime_state IN ('pending', 'provisioning', 'ready', 'degraded', 'disabled', 'deleting')),
    upstream_url TEXT,
    coolify_resource_id TEXT,
    last_healthy_at TEXT,
    last_error TEXT,
    metadata TEXT NOT NULL DEFAULT '{}' CHECK(json_valid(metadata)),
    created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
) STRICT;

CREATE INDEX IF NOT EXISTS idx_memory_bank_projects_owner ON memory_bank_projects(owner_subject_sub, service_id);
CREATE INDEX IF NOT EXISTS idx_memory_bank_project_shares_collaborator ON memory_bank_project_shares(collaborator_subject_sub, state);
CREATE INDEX IF NOT EXISTS idx_memory_bank_project_shares_project ON memory_bank_project_shares(project_id, state);
CREATE INDEX IF NOT EXISTS idx_memory_bank_share_projections_state ON memory_bank_share_projections(runtime_state);

-- +goose Down
DROP TABLE IF EXISTS memory_bank_share_projections;
DROP INDEX IF EXISTS idx_memory_bank_project_shares_project;
DROP INDEX IF EXISTS idx_memory_bank_project_shares_collaborator;
DROP INDEX IF EXISTS idx_memory_bank_projects_owner;
DROP TABLE IF EXISTS memory_bank_project_shares;
DROP TABLE IF EXISTS memory_bank_projects;
DROP INDEX IF EXISTS idx_tenant_instances_identity;
ALTER TABLE oauth_sessions DROP COLUMN share_id;
ALTER TABLE oauth_sessions DROP COLUMN policy_binding_id;
ALTER TABLE oauth_sessions DROP COLUMN authorization_details;
