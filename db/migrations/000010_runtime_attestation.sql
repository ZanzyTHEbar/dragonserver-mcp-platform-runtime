-- +goose Up
ALTER TABLE tenant_instances ADD COLUMN attestation_state TEXT NOT NULL DEFAULT 'unknown' CHECK(attestation_state IN ('trusted', 'warning', 'untrusted', 'unknown'));
ALTER TABLE tenant_instances ADD COLUMN last_attested_at TEXT;
ALTER TABLE tenant_instances ADD COLUMN last_attestation_id BLOB CHECK(last_attestation_id IS NULL OR length(last_attestation_id) = 16);
ALTER TABLE tenant_instances ADD COLUMN runtime_spec_id BLOB CHECK(runtime_spec_id IS NULL OR length(runtime_spec_id) = 16);

CREATE TABLE IF NOT EXISTS tenant_runtime_specs (
    spec_id BLOB PRIMARY KEY CHECK(length(spec_id) = 16),
    tenant_id BLOB NOT NULL REFERENCES tenant_instances(tenant_id) ON DELETE CASCADE CHECK(length(tenant_id) = 16),
    service_id TEXT NOT NULL REFERENCES service_catalog(service_id) ON DELETE CASCADE,
    subject_sub TEXT NOT NULL REFERENCES subjects(subject_sub) ON DELETE CASCADE,
    spec_version TEXT NOT NULL,
    compose_hash TEXT NOT NULL,
    env_contract_hash TEXT NOT NULL,
    secret_contract_hash TEXT NOT NULL,
    image_refs_json TEXT NOT NULL DEFAULT '[]' CHECK(json_valid(image_refs_json)),
    network_policy_json TEXT NOT NULL DEFAULT '{}' CHECK(json_valid(network_policy_json)),
    identity_context_hash TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(spec_id, tenant_id),
    FOREIGN KEY(tenant_id, subject_sub, service_id) REFERENCES tenant_instances(tenant_id, subject_sub, service_id) ON DELETE CASCADE
) STRICT;

CREATE TABLE IF NOT EXISTS tenant_runtime_measurements (
    measurement_id BLOB PRIMARY KEY CHECK(length(measurement_id) = 16),
    tenant_id BLOB NOT NULL REFERENCES tenant_instances(tenant_id) ON DELETE CASCADE CHECK(length(tenant_id) = 16),
    coolify_resource_id TEXT,
    container_id TEXT,
    source TEXT NOT NULL CHECK(source IN ('coolify_api', 'docker_api', 'http_probe', 'static_upstream', 'control_plane')),
    image_ref TEXT,
    image_digest TEXT,
    compose_hash TEXT,
    env_contract_hash TEXT,
    network_json TEXT NOT NULL DEFAULT '{}' CHECK(json_valid(network_json)),
    ports_json TEXT NOT NULL DEFAULT '{}' CHECK(json_valid(ports_json)),
    volumes_json TEXT NOT NULL DEFAULT '{}' CHECK(json_valid(volumes_json)),
    health_status TEXT NOT NULL DEFAULT 'unknown' CHECK(health_status IN ('healthy', 'unhealthy', 'unknown')),
    raw_summary_json TEXT NOT NULL DEFAULT '{}' CHECK(json_valid(raw_summary_json)),
    measured_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(measurement_id, tenant_id)
) STRICT;

CREATE TABLE IF NOT EXISTS tenant_runtime_attestations (
    attestation_id BLOB PRIMARY KEY CHECK(length(attestation_id) = 16),
    tenant_id BLOB NOT NULL REFERENCES tenant_instances(tenant_id) ON DELETE CASCADE CHECK(length(tenant_id) = 16),
    spec_id BLOB REFERENCES tenant_runtime_specs(spec_id) ON DELETE SET NULL CHECK(spec_id IS NULL OR length(spec_id) = 16),
    measurement_id BLOB REFERENCES tenant_runtime_measurements(measurement_id) ON DELETE SET NULL CHECK(measurement_id IS NULL OR length(measurement_id) = 16),
    policy_version TEXT NOT NULL,
    verdict TEXT NOT NULL CHECK(verdict IN ('trusted', 'warning', 'untrusted', 'unknown')),
    failure_reasons_json TEXT NOT NULL DEFAULT '[]' CHECK(json_valid(failure_reasons_json)),
    expires_at TEXT,
    created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY(spec_id, tenant_id) REFERENCES tenant_runtime_specs(spec_id, tenant_id),
    FOREIGN KEY(measurement_id, tenant_id) REFERENCES tenant_runtime_measurements(measurement_id, tenant_id)
) STRICT;

CREATE INDEX IF NOT EXISTS idx_tenant_runtime_specs_tenant_id ON tenant_runtime_specs(tenant_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_tenant_runtime_specs_service_id ON tenant_runtime_specs(service_id);
CREATE INDEX IF NOT EXISTS idx_tenant_runtime_measurements_tenant_id ON tenant_runtime_measurements(tenant_id, measured_at DESC);
CREATE INDEX IF NOT EXISTS idx_tenant_runtime_measurements_source ON tenant_runtime_measurements(source, measured_at DESC);
CREATE INDEX IF NOT EXISTS idx_tenant_runtime_attestations_tenant_id ON tenant_runtime_attestations(tenant_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_tenant_runtime_attestations_verdict ON tenant_runtime_attestations(verdict, created_at DESC);

-- +goose Down
DROP TABLE IF EXISTS tenant_runtime_attestations;
DROP TABLE IF EXISTS tenant_runtime_measurements;
DROP TABLE IF EXISTS tenant_runtime_specs;
ALTER TABLE tenant_instances DROP COLUMN runtime_spec_id;
ALTER TABLE tenant_instances DROP COLUMN last_attestation_id;
ALTER TABLE tenant_instances DROP COLUMN last_attested_at;
ALTER TABLE tenant_instances DROP COLUMN attestation_state;
