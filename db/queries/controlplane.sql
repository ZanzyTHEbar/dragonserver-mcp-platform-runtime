-- name: UpsertSubject :exec
INSERT INTO subjects (subject_sub, subject_key, preferred_username, email, display_name, account_binding_id, account_binding_claim, last_synced_at, updated_at)
VALUES (sqlc.arg(subject_sub), sqlc.arg(subject_key), sqlc.arg(preferred_username), sqlc.arg(email), sqlc.arg(display_name), sqlc.arg(account_binding_id), sqlc.arg(account_binding_claim), CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
ON CONFLICT(subject_sub) DO UPDATE SET
    subject_key = excluded.subject_key,
    preferred_username = excluded.preferred_username,
    email = excluded.email,
    display_name = excluded.display_name,
	account_binding_id = excluded.account_binding_id,
	account_binding_claim = excluded.account_binding_claim,
    last_synced_at = CURRENT_TIMESTAMP,
    updated_at = CURRENT_TIMESTAMP;

-- name: UpsertSubjectPreservingMetadata :exec
INSERT INTO subjects (subject_sub, subject_key, preferred_username, email, display_name, account_binding_id, account_binding_claim, last_synced_at, updated_at)
VALUES (sqlc.arg(subject_sub), sqlc.arg(subject_key), sqlc.arg(preferred_username), sqlc.arg(email), sqlc.arg(display_name), sqlc.arg(account_binding_id), sqlc.arg(account_binding_claim), CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
ON CONFLICT(subject_sub) DO UPDATE SET
    subject_key = subjects.subject_key,
    preferred_username = COALESCE(excluded.preferred_username, subjects.preferred_username),
    email = COALESCE(excluded.email, subjects.email),
    display_name = COALESCE(excluded.display_name, subjects.display_name),
	account_binding_id = COALESCE(excluded.account_binding_id, subjects.account_binding_id),
	account_binding_claim = COALESCE(excluded.account_binding_claim, subjects.account_binding_claim),
    updated_at = CURRENT_TIMESTAMP;

-- name: GetSubject :one
SELECT subject_sub,
       subject_key,
       preferred_username,
       email,
       display_name,
       account_binding_id,
       account_binding_claim
FROM subjects
WHERE subject_sub = sqlc.arg(subject_sub);

-- name: DeleteSubjectGrants :exec
DELETE FROM service_grants WHERE subject_sub = sqlc.arg(subject_sub);

-- name: DeleteSubjectGrantSources :exec
DELETE FROM service_grant_sources WHERE subject_sub = sqlc.arg(subject_sub);

-- name: DeleteSubjectSyncedGrantSources :exec
DELETE FROM service_grant_sources
WHERE subject_sub = sqlc.arg(subject_sub)
  AND source_group <> 'manual';

-- name: InsertServiceGrantSource :exec
INSERT INTO service_grant_sources (subject_sub, service_id, source_group, granted_at, last_synced_at)
VALUES (sqlc.arg(subject_sub), sqlc.arg(service_id), sqlc.arg(source_group), sqlc.arg(granted_at), sqlc.arg(last_synced_at))
ON CONFLICT(subject_sub, service_id, source_group) DO UPDATE SET
    last_synced_at = excluded.last_synced_at,
    updated_at = CURRENT_TIMESTAMP;

-- name: UpsertManualServiceGrantSource :exec
INSERT INTO service_grant_sources (subject_sub, service_id, source_group, granted_at, last_synced_at)
VALUES (sqlc.arg(subject_sub), sqlc.arg(service_id), 'manual', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
ON CONFLICT(subject_sub, service_id, source_group) DO UPDATE SET
    last_synced_at = CURRENT_TIMESTAMP,
    updated_at = CURRENT_TIMESTAMP;

-- name: DeleteManualServiceGrantSource :exec
DELETE FROM service_grant_sources
WHERE subject_sub = sqlc.arg(subject_sub)
  AND service_id = sqlc.arg(service_id)
  AND source_group = 'manual';

-- name: RebuildEffectiveServiceGrants :exec
DELETE FROM service_grants;

-- name: InsertEffectiveServiceGrantsFromSources :exec
INSERT INTO service_grants (subject_sub, service_id, source_group, granted_at, last_synced_at)
SELECT subject_sub,
       service_id,
       CASE WHEN SUM(CASE WHEN source_group = 'manual' THEN 1 ELSE 0 END) > 0 THEN 'manual' ELSE MIN(source_group) END AS source_group,
       MIN(granted_at) AS granted_at,
       MAX(last_synced_at) AS last_synced_at
FROM service_grant_sources
GROUP BY subject_sub, service_id;

-- name: ListSubjectServiceGrants :many
SELECT service_grants.subject_sub,
       service_grants.service_id,
       service_grants.source_group,
       service_grants.granted_at,
       service_grants.last_synced_at
FROM service_grants
JOIN service_catalog ON service_catalog.service_id = service_grants.service_id
WHERE service_grants.subject_sub = sqlc.arg(subject_sub)
  AND service_catalog.enabled = 1
ORDER BY service_grants.service_id;

-- name: CountSubjectServiceGrant :one
SELECT COUNT(*)
FROM service_grants
JOIN service_catalog ON service_catalog.service_id = service_grants.service_id
WHERE service_grants.subject_sub = sqlc.arg(subject_sub)
  AND service_grants.service_id = sqlc.arg(service_id)
  AND service_catalog.enabled = 1;

-- name: DeleteAllServiceGrants :exec
DELETE FROM service_grant_sources WHERE source_group <> 'manual';

-- name: DeleteStaleServiceGrants :exec
DELETE FROM service_grant_sources
WHERE source_group <> 'manual'
  AND subject_sub NOT IN (sqlc.slice(subject_subs));

-- name: ListDesiredTenantSpecs :many
SELECT subjects.subject_sub,
       subjects.subject_key,
       COALESCE(subjects.preferred_username, '') AS preferred_username,
       COALESCE(subjects.email, '') AS email,
       COALESCE(subjects.display_name, '') AS display_name,
       COALESCE(subjects.account_binding_id, '') AS account_binding_id,
       COALESCE(subjects.account_binding_claim, '') AS account_binding_claim,
       service_grants.service_id
FROM service_grants
JOIN subjects ON subjects.subject_sub = service_grants.subject_sub
JOIN service_catalog ON service_catalog.service_id = service_grants.service_id
WHERE service_catalog.enabled = 1
  AND service_catalog.source = 'builtin'
ORDER BY service_grants.service_id, subjects.subject_sub;

-- name: ListTenantInstances :many
SELECT tenant_id,
       subject_sub,
       service_id,
       subject_key,
       tenant_instance_name,
       internal_dns_name,
       desired_state,
       runtime_state,
       coolify_resource_id,
       coolify_application_id,
       upstream_url,
       secret_version,
       last_healthy_at,
       last_reconciled_at,
       last_error,
       metadata,
       attestation_state,
       last_attested_at,
       last_attestation_id,
       runtime_spec_id,
       created_at,
       updated_at
FROM tenant_instances
ORDER BY service_id, subject_sub;

-- name: GetTenantInstance :one
SELECT tenant_id,
       subject_sub,
       service_id,
       subject_key,
       tenant_instance_name,
       internal_dns_name,
       desired_state,
       runtime_state,
       coolify_resource_id,
       coolify_application_id,
       upstream_url,
       secret_version,
       last_healthy_at,
       last_reconciled_at,
       last_error,
       metadata,
       attestation_state,
       last_attested_at,
       last_attestation_id,
       runtime_spec_id,
       created_at,
       updated_at
FROM tenant_instances
WHERE tenant_id = sqlc.arg(tenant_id);

-- name: GetTenantInstanceBySubjectService :one
SELECT tenant_id,
       subject_sub,
       service_id,
       subject_key,
       tenant_instance_name,
       internal_dns_name,
       desired_state,
       runtime_state,
       coolify_resource_id,
       coolify_application_id,
       upstream_url,
       secret_version,
       last_healthy_at,
       last_reconciled_at,
       last_error,
       metadata,
       attestation_state,
       last_attested_at,
       last_attestation_id,
       runtime_spec_id,
       created_at,
       updated_at
FROM tenant_instances
WHERE subject_sub = sqlc.arg(subject_sub)
  AND service_id = sqlc.arg(service_id)
  AND desired_state <> 'deleted';

-- name: InsertTenantInstance :exec
INSERT INTO tenant_instances (tenant_id, subject_sub, service_id, subject_key, tenant_instance_name, internal_dns_name, desired_state, runtime_state)
VALUES (sqlc.arg(tenant_id), sqlc.arg(subject_sub), sqlc.arg(service_id), sqlc.arg(subject_key), sqlc.arg(tenant_instance_name), sqlc.arg(internal_dns_name), sqlc.arg(desired_state), sqlc.arg(runtime_state));

-- name: UpsertStaticTenantUpstream :exec
INSERT INTO tenant_instances (
    tenant_id,
    subject_sub,
    service_id,
    subject_key,
    tenant_instance_name,
    internal_dns_name,
    desired_state,
    runtime_state,
    upstream_url,
    last_healthy_at,
    last_error,
    metadata
)
VALUES (
    sqlc.arg(tenant_id),
    sqlc.arg(subject_sub),
    sqlc.arg(service_id),
    sqlc.arg(subject_key),
    sqlc.arg(tenant_instance_name),
    sqlc.arg(internal_dns_name),
    'enabled',
    'ready',
    sqlc.arg(upstream_url),
    sqlc.arg(last_healthy_at),
    NULL,
    json_object('runtime_mode', 'static_upstream')
)
ON CONFLICT(subject_sub, service_id) DO UPDATE SET
    subject_key = excluded.subject_key,
    tenant_instance_name = excluded.tenant_instance_name,
    internal_dns_name = excluded.internal_dns_name,
    desired_state = 'enabled',
    runtime_state = 'ready',
    coolify_resource_id = NULL,
    coolify_application_id = NULL,
    upstream_url = excluded.upstream_url,
    last_healthy_at = excluded.last_healthy_at,
    last_error = NULL,
    metadata = json_patch(tenant_instances.metadata, excluded.metadata),
    updated_at = CURRENT_TIMESTAMP;

-- name: MarkTenantDesiredDeleted :exec
UPDATE tenant_instances
SET desired_state = sqlc.arg(desired_state),
    updated_at = CURRENT_TIMESTAMP
WHERE tenant_id = sqlc.arg(tenant_id);

-- name: MarkTenantDesiredDisabledBySubjectService :execrows
UPDATE tenant_instances
SET desired_state = 'disabled',
    updated_at = CURRENT_TIMESTAMP
WHERE tenant_instances.subject_sub = sqlc.arg(target_subject_sub)
  AND tenant_instances.service_id = sqlc.arg(target_service_id)
  AND tenant_instances.desired_state <> 'deleted';

-- name: MarkTenantDesiredEnabledBySubjectServiceWithGrant :execrows
UPDATE tenant_instances
SET desired_state = 'enabled',
    updated_at = CURRENT_TIMESTAMP
WHERE tenant_instances.subject_sub = sqlc.arg(target_subject_sub)
  AND tenant_instances.service_id = sqlc.arg(target_service_id)
  AND tenant_instances.desired_state <> 'deleted'
  AND EXISTS (
    SELECT 1
    FROM service_grants
    JOIN service_catalog ON service_catalog.service_id = service_grants.service_id
    WHERE service_grants.subject_sub = tenant_instances.subject_sub
      AND service_grants.service_id = tenant_instances.service_id
      AND service_catalog.enabled = 1
  );

-- name: EnableTenantInstance :exec
UPDATE tenant_instances
SET subject_key = sqlc.arg(subject_key),
    tenant_instance_name = sqlc.arg(tenant_instance_name),
    internal_dns_name = sqlc.arg(internal_dns_name),
    desired_state = sqlc.arg(desired_state),
    runtime_state = sqlc.arg(runtime_state),
    last_error = NULLIF(sqlc.arg(last_error), ''),
    updated_at = CURRENT_TIMESTAMP
WHERE tenant_id = sqlc.arg(tenant_id);

-- name: InsertReconcileRun :exec
INSERT INTO reconcile_runs (run_id, tenant_id, desired_state, observed_state, action, status, details, started_at, finished_at)
VALUES (sqlc.arg(run_id), sqlc.arg(tenant_id), sqlc.arg(desired_state), sqlc.arg(observed_state), sqlc.arg(action), sqlc.arg(status), sqlc.arg(details), sqlc.arg(started_at), sqlc.arg(finished_at));

-- name: MarkTenantReconciled :exec
UPDATE tenant_instances
SET last_reconciled_at = sqlc.arg(last_reconciled_at),
    last_error = NULLIF(sqlc.arg(last_error), ''),
    updated_at = CURRENT_TIMESTAMP
WHERE tenant_id = sqlc.arg(tenant_id);

-- name: UpdateTenantRuntimeStatus :exec
UPDATE tenant_instances
SET runtime_state = sqlc.arg(runtime_state),
    coolify_resource_id = CASE WHEN CAST(sqlc.arg(clear_runtime_references) AS boolean) THEN NULL WHEN sqlc.arg(coolify_resource_id) = '' THEN coolify_resource_id ELSE sqlc.arg(coolify_resource_id) END,
    coolify_application_id = CASE WHEN sqlc.arg(clear_runtime_references) THEN NULL WHEN sqlc.arg(coolify_application_id) = '' THEN coolify_application_id ELSE sqlc.arg(coolify_application_id) END,
    upstream_url = CASE WHEN sqlc.arg(clear_runtime_references) THEN NULL WHEN sqlc.arg(upstream_url) = '' THEN upstream_url ELSE sqlc.arg(upstream_url) END,
    last_healthy_at = CASE WHEN sqlc.arg(clear_runtime_references) THEN NULL WHEN sqlc.narg(last_healthy_at) IS NULL THEN last_healthy_at ELSE sqlc.narg(last_healthy_at) END,
    last_error = NULLIF(sqlc.arg(last_error), ''),
    updated_at = CURRENT_TIMESTAMP
WHERE tenant_id = sqlc.arg(tenant_id);

-- name: DeleteTenantInstance :exec
DELETE FROM tenant_instances WHERE tenant_id = sqlc.arg(tenant_id);

-- name: UpsertMemoryBankProject :one
INSERT INTO memory_bank_projects (project_id, owner_subject_sub, owner_tenant_id, service_id, project_key, display_name, root_path, metadata, archived_at)
VALUES (sqlc.arg(project_id), sqlc.arg(owner_subject_sub), sqlc.arg(owner_tenant_id), sqlc.arg(service_id), sqlc.arg(project_key), sqlc.arg(display_name), sqlc.arg(root_path), sqlc.arg(metadata), sqlc.narg(archived_at))
ON CONFLICT(owner_subject_sub, service_id, project_key) DO UPDATE SET
    owner_tenant_id = excluded.owner_tenant_id,
    display_name = excluded.display_name,
    root_path = excluded.root_path,
    metadata = excluded.metadata,
    archived_at = COALESCE(excluded.archived_at, memory_bank_projects.archived_at),
    updated_at = CURRENT_TIMESTAMP
RETURNING project_id;

-- name: GetMemoryBankProject :one
SELECT project_id,
       owner_subject_sub,
       owner_tenant_id,
       service_id,
       project_key,
       display_name,
       root_path,
       metadata,
       archived_at,
       created_at,
       updated_at
FROM memory_bank_projects
WHERE project_id = sqlc.arg(project_id);

-- name: GetMemoryBankProjectByOwnerKey :one
SELECT project_id,
       owner_subject_sub,
       owner_tenant_id,
       service_id,
       project_key,
       display_name,
       root_path,
       metadata,
       archived_at,
       created_at,
       updated_at
FROM memory_bank_projects
WHERE owner_subject_sub = sqlc.arg(owner_subject_sub)
  AND service_id = sqlc.arg(service_id)
  AND project_key = sqlc.arg(project_key);

-- name: ListMemoryBankProjectsByOwner :many
SELECT project_id,
       owner_subject_sub,
       owner_tenant_id,
       service_id,
       project_key,
       display_name,
       root_path,
       metadata,
       archived_at,
       created_at,
       updated_at
FROM memory_bank_projects
WHERE owner_subject_sub = sqlc.arg(owner_subject_sub)
  AND service_id = sqlc.arg(service_id)
  AND (sqlc.arg(include_archived) OR archived_at IS NULL)
ORDER BY project_key;

-- name: InsertMemoryBankProjectShare :execrows
INSERT INTO memory_bank_project_shares (share_id, project_id, owner_subject_sub, collaborator_subject_sub, permission, state, source, created_by_subject_sub, expires_at, metadata)
SELECT sqlc.arg(share_id),
       sqlc.arg(project_id),
       sqlc.arg(owner_subject_sub),
       sqlc.arg(collaborator_subject_sub),
       sqlc.arg(permission),
       sqlc.arg(state),
       sqlc.arg(source),
       sqlc.arg(created_by_subject_sub),
       sqlc.narg(expires_at),
       sqlc.arg(metadata)
FROM memory_bank_projects
WHERE project_id = sqlc.arg(project_id)
  AND owner_subject_sub = sqlc.arg(owner_subject_sub)
  AND archived_at IS NULL;

-- name: ExpireMemoryBankProjectShares :execrows
UPDATE memory_bank_project_shares
SET state = 'expired',
    updated_at = CURRENT_TIMESTAMP
WHERE state IN ('pending', 'active')
  AND expires_at IS NOT NULL
  AND julianday(expires_at) <= julianday(sqlc.arg(now));

-- name: GetMemoryBankProjectShare :one
SELECT share_id,
       memory_bank_project_shares.project_id,
       memory_bank_project_shares.owner_subject_sub,
       memory_bank_project_shares.collaborator_subject_sub,
       memory_bank_project_shares.permission,
       memory_bank_project_shares.state,
       memory_bank_project_shares.source,
       memory_bank_project_shares.created_by_subject_sub,
       memory_bank_project_shares.accepted_at,
       memory_bank_project_shares.revoked_at,
       memory_bank_project_shares.expires_at,
       memory_bank_project_shares.metadata,
       memory_bank_project_shares.created_at,
       memory_bank_project_shares.updated_at
FROM memory_bank_project_shares
WHERE share_id = sqlc.arg(share_id);

-- name: ListMemoryBankProjectSharesForSubject :many
SELECT memory_bank_project_shares.share_id,
       memory_bank_project_shares.project_id,
       memory_bank_project_shares.owner_subject_sub,
       memory_bank_project_shares.collaborator_subject_sub,
       memory_bank_project_shares.permission,
       memory_bank_project_shares.state,
       memory_bank_project_shares.source,
       memory_bank_project_shares.created_by_subject_sub,
       memory_bank_project_shares.accepted_at,
       memory_bank_project_shares.revoked_at,
       memory_bank_project_shares.expires_at,
       memory_bank_project_shares.metadata,
       memory_bank_project_shares.created_at,
       memory_bank_project_shares.updated_at
FROM memory_bank_project_shares
JOIN memory_bank_projects ON memory_bank_projects.project_id = memory_bank_project_shares.project_id
WHERE memory_bank_project_shares.collaborator_subject_sub = sqlc.arg(collaborator_subject_sub)
  AND (sqlc.arg(include_inactive) OR (memory_bank_project_shares.state IN ('pending', 'active') AND (memory_bank_project_shares.expires_at IS NULL OR julianday(memory_bank_project_shares.expires_at) > julianday(sqlc.arg(now))) AND memory_bank_projects.archived_at IS NULL))
ORDER BY memory_bank_project_shares.updated_at DESC;

-- name: ListMemoryBankProjectSharesForSubjectService :many
SELECT memory_bank_project_shares.share_id,
       memory_bank_project_shares.project_id,
       memory_bank_project_shares.owner_subject_sub,
       memory_bank_project_shares.collaborator_subject_sub,
       memory_bank_project_shares.permission,
       memory_bank_project_shares.state,
       memory_bank_project_shares.source,
       memory_bank_project_shares.created_by_subject_sub,
       memory_bank_project_shares.accepted_at,
       memory_bank_project_shares.revoked_at,
       memory_bank_project_shares.expires_at,
       memory_bank_project_shares.metadata,
       memory_bank_project_shares.created_at,
       memory_bank_project_shares.updated_at
FROM memory_bank_project_shares
JOIN memory_bank_projects ON memory_bank_projects.project_id = memory_bank_project_shares.project_id
WHERE memory_bank_project_shares.collaborator_subject_sub = sqlc.arg(collaborator_subject_sub)
  AND memory_bank_projects.service_id = sqlc.arg(service_id)
  AND (sqlc.arg(include_inactive) OR (memory_bank_project_shares.state IN ('pending', 'active') AND (memory_bank_project_shares.expires_at IS NULL OR julianday(memory_bank_project_shares.expires_at) > julianday(sqlc.arg(now))) AND memory_bank_projects.archived_at IS NULL))
ORDER BY memory_bank_project_shares.updated_at DESC;

-- name: ActivateMemoryBankProjectShare :execrows
UPDATE memory_bank_project_shares
SET state = 'active',
    accepted_at = sqlc.arg(accepted_at),
    updated_at = CURRENT_TIMESTAMP
WHERE share_id = sqlc.arg(share_id)
  AND collaborator_subject_sub = sqlc.arg(collaborator_subject_sub)
  AND state IN ('pending', 'active')
  AND (expires_at IS NULL OR julianday(expires_at) > julianday(sqlc.arg(accepted_at)))
  AND EXISTS (
    SELECT 1
    FROM memory_bank_projects
    WHERE memory_bank_projects.project_id = memory_bank_project_shares.project_id
      AND memory_bank_projects.archived_at IS NULL
  );

-- name: RevokeMemoryBankProjectShare :execrows
UPDATE memory_bank_project_shares
SET state = 'revoked',
    revoked_at = sqlc.arg(revoked_at),
    updated_at = CURRENT_TIMESTAMP
WHERE share_id = sqlc.arg(share_id)
  AND state <> 'revoked';

-- name: InsertTenantRuntimeSpec :exec
INSERT INTO tenant_runtime_specs (spec_id, tenant_id, service_id, subject_sub, spec_version, compose_hash, env_contract_hash, secret_contract_hash, image_refs_json, network_policy_json, identity_context_hash, created_at)
VALUES (sqlc.arg(spec_id), sqlc.arg(tenant_id), sqlc.arg(service_id), sqlc.arg(subject_sub), sqlc.arg(spec_version), sqlc.arg(compose_hash), sqlc.arg(env_contract_hash), sqlc.arg(secret_contract_hash), sqlc.arg(image_refs_json), sqlc.arg(network_policy_json), sqlc.arg(identity_context_hash), sqlc.arg(created_at));

-- name: GetTenantRuntimeSpec :one
SELECT spec_id,
       tenant_id,
       service_id,
       subject_sub,
       spec_version,
       compose_hash,
       env_contract_hash,
       secret_contract_hash,
       image_refs_json,
       network_policy_json,
       identity_context_hash,
       created_at
FROM tenant_runtime_specs
WHERE spec_id = sqlc.arg(spec_id);

-- name: ListTenantRuntimeSpecsForTenant :many
SELECT spec_id,
       tenant_id,
       service_id,
       subject_sub,
       spec_version,
       compose_hash,
       env_contract_hash,
       secret_contract_hash,
       image_refs_json,
       network_policy_json,
       identity_context_hash,
       created_at
FROM tenant_runtime_specs
WHERE tenant_id = sqlc.arg(tenant_id)
ORDER BY created_at DESC;

-- name: InsertTenantRuntimeMeasurement :exec
INSERT INTO tenant_runtime_measurements (measurement_id, tenant_id, coolify_resource_id, container_id, source, image_ref, image_digest, compose_hash, env_contract_hash, network_json, ports_json, volumes_json, health_status, raw_summary_json, measured_at)
VALUES (sqlc.arg(measurement_id), sqlc.arg(tenant_id), sqlc.narg(coolify_resource_id), sqlc.narg(container_id), sqlc.arg(source), sqlc.narg(image_ref), sqlc.narg(image_digest), sqlc.narg(compose_hash), sqlc.narg(env_contract_hash), sqlc.arg(network_json), sqlc.arg(ports_json), sqlc.arg(volumes_json), sqlc.arg(health_status), sqlc.arg(raw_summary_json), sqlc.arg(measured_at));

-- name: GetTenantRuntimeMeasurement :one
SELECT measurement_id,
       tenant_id,
       coolify_resource_id,
       container_id,
       source,
       image_ref,
       image_digest,
       compose_hash,
       env_contract_hash,
       network_json,
       ports_json,
       volumes_json,
       health_status,
       raw_summary_json,
       measured_at
FROM tenant_runtime_measurements
WHERE measurement_id = sqlc.arg(measurement_id);

-- name: ListTenantRuntimeMeasurementsForTenant :many
SELECT measurement_id,
       tenant_id,
       coolify_resource_id,
       container_id,
       source,
       image_ref,
       image_digest,
       compose_hash,
       env_contract_hash,
       network_json,
       ports_json,
       volumes_json,
       health_status,
       raw_summary_json,
       measured_at
FROM tenant_runtime_measurements
WHERE tenant_id = sqlc.arg(tenant_id)
ORDER BY measured_at DESC;

-- name: InsertTenantRuntimeAttestation :exec
INSERT INTO tenant_runtime_attestations (attestation_id, tenant_id, spec_id, measurement_id, policy_version, verdict, failure_reasons_json, expires_at, created_at)
VALUES (sqlc.arg(attestation_id), sqlc.arg(tenant_id), sqlc.narg(spec_id), sqlc.narg(measurement_id), sqlc.arg(policy_version), sqlc.arg(verdict), sqlc.arg(failure_reasons_json), sqlc.narg(expires_at), sqlc.arg(created_at));

-- name: GetTenantRuntimeAttestation :one
SELECT attestation_id,
       tenant_id,
       spec_id,
       measurement_id,
       policy_version,
       verdict,
       failure_reasons_json,
       expires_at,
       created_at
FROM tenant_runtime_attestations
WHERE attestation_id = sqlc.arg(attestation_id);

-- name: GetLatestTenantRuntimeAttestation :one
SELECT attestation_id,
       tenant_id,
       spec_id,
       measurement_id,
       policy_version,
       verdict,
       failure_reasons_json,
       expires_at,
       created_at
FROM tenant_runtime_attestations
WHERE tenant_id = sqlc.arg(tenant_id)
ORDER BY created_at DESC, rowid DESC
LIMIT 1;

-- name: MarkTenantRuntimeSpec :exec
UPDATE tenant_instances
SET runtime_spec_id = sqlc.arg(spec_id),
    updated_at = CURRENT_TIMESTAMP
WHERE tenant_id = sqlc.arg(tenant_id);

-- name: MarkTenantAttestationSummary :exec
UPDATE tenant_instances
SET attestation_state = sqlc.arg(verdict),
    last_attested_at = sqlc.arg(attested_at),
    last_attestation_id = sqlc.arg(attestation_id),
    runtime_spec_id = CASE WHEN sqlc.narg(spec_id) IS NULL THEN runtime_spec_id ELSE sqlc.narg(spec_id) END,
    updated_at = CURRENT_TIMESTAMP
WHERE tenant_id = sqlc.arg(tenant_id);
