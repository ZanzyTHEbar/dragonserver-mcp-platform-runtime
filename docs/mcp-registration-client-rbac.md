# MCP Registration, Clients, and RBAC

The platform is service-agnostic. Core runtime code does not ship built-in MCP services or package-specific tenant images. Operators register each MCP service at deployment scope through the internal control-plane admin API, then grant subjects access through the configured identity source or manual grants.

## Service Catalog

Each service catalog entry defines the public edge path, upstream shape, transport behavior, health path, adapter requirement, optional identity-context mode, and required secret contract.

Register a service from a trusted operator channel:

```bash
curl -fsS http://mcp-control-plane:8081/v1/services/<serviceID> \
  -H "Authorization: Bearer <admin-token>" \
  -H 'Content-Type: application/json' \
  -X PUT \
  --data '{
    "display_name": "Example MCP",
    "upstream_service_name": "example-mcp",
    "transport_type": "streamable-http",
    "internal_port": 8080,
    "public_path": "/example/mcp",
    "internal_upstream_path": "/mcp",
    "health_path": "/health",
    "health_probe_expectation": "GET returns 2xx",
    "resource_profile": "small",
    "persistence_policy": "stateless",
    "adapter_requirement": "none",
    "secret_contract": [],
    "identity_context": {"mode":"none"}
  }'
```

The edge publishes each enabled service as:

```text
url      = https://<edge-domain>/<service-path>
resource = https://<edge-domain>/<service-path>
scope    = mcp:<serviceID>
```

## Grants

Identity-provider group names use the generic convention `mcp-service-<serviceID>`. A subject in that group receives the matching service grant during control-plane identity sync.

Manual grant:

```bash
curl -fsS http://mcp-control-plane:8081/v1/subjects/<subjectSub>/grants/<serviceID> \
  -H "Authorization: Bearer <admin-token>" \
  -X PUT
```

Manual grant deletion removes only the manual grant source. Identity-synced grant sources remain until the identity snapshot no longer contains them.

## Static Upstreams

Dynamic MCP services are bound per subject by registering a static upstream after the service is granted:

```bash
curl -fsS http://mcp-control-plane:8081/v1/subjects/<subjectSub>/services/<serviceID>/upstream \
  -H "Authorization: Bearer <admin-token>" \
  -H 'Content-Type: application/json' \
  -X PUT \
  --data '{"upstream_url":"https://mcp.internal.example/mcp"}'
```

Static upstreams are admin-trusted egress. Validate target ownership and network reachability before binding them.

## Tenant Lifecycle

Tenant lifecycle endpoints are separate from grant deletion. Use `suspend` to disable compute while preserving durable data, and `resume` only when the subject still has the service grant.

```bash
curl -fsS http://mcp-control-plane:8081/v1/subjects/<subjectSub>/services/<serviceID>/suspend \
  -H "Authorization: Bearer <admin-token>" \
  -X PUT

curl -fsS http://mcp-control-plane:8081/v1/subjects/<subjectSub>/services/<serviceID>/resume \
  -H "Authorization: Bearer <admin-token>" \
  -X PUT
```

## OAuth Clients

Clients request exactly one service scope and the matching RFC 8707 resource.

```text
scope=mcp:<serviceID>
resource=https://<edge-domain>/<service-path>
```

The edge enforces:

1. The opaque edge token is valid.
2. The token resource matches the target service resource.
3. The token scope includes `mcp:<serviceID>`.
4. The subject still has an enabled grant for the service.

## Identity Context

When `identity_context.mode` is `signed-headers`, `mcp-edge` strips inbound identity headers and injects trusted `X-MCP-Identity-*` headers after OAuth, resource, scope, grant, and tenant resolution checks. Configure `MCP_EDGE_IDENTITY_HEADER_SECRET_PATH` on the edge and mount the same secret into downstream services that verify the signature.

## Project Shares

The project/share admin endpoints are generic and scoped by `<serviceID>`:

```text
/v1/subjects/<subjectSub>/services/<serviceID>/projects
/v1/subjects/<subjectSub>/services/<serviceID>/projects/<projectKey>/shares
/v1/subjects/<subjectSub>/services/<serviceID>/shares/<shareID>/accept
```

Use these only for services whose upstream data model understands the configured project root and sharing semantics. Project names are not a security boundary unless the upstream enforces identity or ACLs.

## Smoke Checks

1. Register a service.
2. Grant a test subject through `mcp-service-<serviceID>` or the manual grant API.
3. Bind a static upstream if the service is not managed by a deployment integration.
4. Confirm unauthenticated service access returns `401` with the service protected-resource metadata URL.
5. Complete OAuth requesting `mcp:<serviceID>` and the matching resource.
6. Confirm access succeeds for the granted service and fails for a different service.
