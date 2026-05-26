package edge

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dragonserver/mcp-platform/internal/controlplane"
	"dragonserver/mcp-platform/internal/domain"
	"dragonserver/mcp-platform/internal/ids"
	platformsqlite "dragonserver/mcp-platform/internal/platform/sqlite"
	"dragonserver/mcp-platform/internal/platform/sqlite/platformdb"

	"github.com/go-oauth2/oauth2/v4/models"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

func TestSQLiteEdgeStateStoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()
	databaseURL := "file:" + filepath.Join(t.TempDir(), "mcp-platform.db")

	cpStore, err := controlplane.NewStore(ctx, databaseURL, logger)
	require.NoError(t, err)
	require.NoError(t, cpStore.RunMigrations(ctx))
	require.NoError(t, cpStore.SeedServiceCatalog(ctx))
	registerSQLiteEdgeTestService(t, ctx, cpStore, databaseURL, "example-a", false)
	registerSQLiteEdgeTestService(t, ctx, cpStore, databaseURL, "example-b", false)

	secretPath := filepath.Join(t.TempDir(), "session-key")
	require.NoError(t, os.WriteFile(secretPath, []byte("test-session-encryption-key"), 0o600))

	storeValue, err := newSQLiteEdgeStateStore(ctx, Config{PlatformDatabaseURL: databaseURL, SessionEncryptionKeyPath: secretPath}, logger)
	require.NoError(t, err)
	t.Cleanup(func() { _ = storeValue.Close() })
	t.Cleanup(cpStore.Close)

	entries, err := storeValue.ListEnabledServiceCatalog(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, entries)

	now := time.Now().UTC().Round(0)
	claims := IdentityClaims{Sub: "sqlite-fixture-user", Email: "fixture@example.com", Name: "Fixture User", PreferredUsername: "fixture-user", AccountBindingID: "stable-user-id", AccountBindingClaim: "dragonserver_user_id", Groups: []string{"mcp-users", "mcp-service-example-a"}}
	require.NoError(t, storeValue.UpsertSubject(ctx, claims))
	gotClaims, ok, err := storeValue.GetSubjectIdentity(ctx, claims.Sub)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, claims.Sub, gotClaims.Sub)
	require.Equal(t, domain.DeriveSubjectKey(claims.Sub), gotClaims.SubjectKey)
	require.Equal(t, claims.Email, gotClaims.Email)
	require.Equal(t, claims.Name, gotClaims.Name)
	require.Equal(t, claims.PreferredUsername, gotClaims.PreferredUsername)
	require.Equal(t, claims.AccountBindingID, gotClaims.AccountBindingID)
	require.Equal(t, claims.AccountBindingClaim, gotClaims.AccountBindingClaim)
	require.NoError(t, cpStore.ReplaceSubjectGrants(ctx, claims.Sub, []controlplane.ServiceGrant{{SubjectSub: claims.Sub, ServiceID: "example-a", SourceGroup: "mcp-service-example-a", GrantedAt: now, LastSyncedAt: now}}))

	allowed, err := storeValue.Allowed(ctx, claims.Sub, "example-a")
	require.NoError(t, err)
	require.True(t, allowed)
	allowed, err = storeValue.AllowedScopes(ctx, claims.Sub, "mcp:example-a")
	require.NoError(t, err)
	require.True(t, allowed)

	pending := pendingLogin{State: "state-1", ReturnTo: "/oauth/authorize", Nonce: "nonce-1", Expiry: now.Add(time.Minute)}
	require.NoError(t, storeValue.PutPendingLogin(ctx, pending))
	gotPending, ok, err := storeValue.GetPendingLogin(ctx, pending.State, now)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, pending, gotPending)
	require.NoError(t, storeValue.DeletePendingLogin(ctx, pending.State))

	session := browserSession{Claims: claims, Expiry: now.Add(time.Hour)}
	require.NoError(t, storeValue.PutBrowserSession(ctx, "session-1", session))
	gotSession, ok, err := storeValue.GetBrowserSession(ctx, "session-1", now)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, session, gotSession)

	client := registeredClient{ID: "client-1", Name: "Example Client", RedirectURIs: []string{"https://example.com/callback"}, GrantTypes: []string{"authorization_code", "refresh_token"}, ResponseTypes: []string{"code"}, TokenEndpointAuthMethod: tokenEndpointAuthMethodClientBasic, Secret: "super-secret", Scopes: []string{"mcp:example-a"}}
	require.NoError(t, storeValue.CreateClient(ctx, client, claims.Sub))
	clientInfo, err := storeValue.GetByID(ctx, client.ID)
	require.NoError(t, err)
	require.Empty(t, clientInfo.GetSecret())
	confidential, ok := clientInfo.(confidentialClient)
	require.True(t, ok)
	require.True(t, confidential.VerifyPassword(client.Secret))
	require.False(t, confidential.VerifyPassword("wrong-secret"))
	require.True(t, clientAllowsScope(clientInfo, "mcp:example-a"))
	require.False(t, clientAllowsScope(clientInfo, "mcp:example-b"))
	require.True(t, clientAllowsGrant(clientInfo, "authorization_code"))
	require.False(t, clientAllowsGrant(clientInfo, "urn:ietf:params:oauth:grant-type:device_code"))

	deviceID := ids.New()
	require.ErrorContains(t, storeValue.CreateDeviceAuthorization(ctx, deviceAuthorization{}), "device authorization id is required")
	require.NoError(t, storeValue.CreateDeviceAuthorization(ctx, deviceAuthorization{
		ID:              deviceID,
		ClientID:        client.ID,
		ServiceID:       "example-a",
		Resource:        "https://mcp.example.com/example-a/mcp",
		Scope:           "mcp:example-a",
		DeviceCodeHash:  hashOpaqueValue("sqlite-device-code"),
		UserCodeHash:    hashOpaqueValue(normalizeUserCode("WXYZ-1234")),
		UserCodeDisplay: "WXYZ-1234",
		ExpiresAt:       now.Add(10 * time.Minute),
		CreatedAt:       now,
	}))
	loadedDevice, ok, err := storeValue.GetDeviceAuthorizationByDeviceCode(ctx, "sqlite-device-code")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, deviceAuthorizationStatusPending, loadedDevice.Status)
	require.Equal(t, "WXYZ-1234", loadedDevice.UserCodeDisplay)
	require.Equal(t, 5*time.Second, loadedDevice.Interval)
	loadedDevice, ok, err = storeValue.GetDeviceAuthorizationByUserCode(ctx, "wxyz 1234")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, deviceID, loadedDevice.ID)
	updated, err := storeValue.ApproveDeviceAuthorization(ctx, deviceID, "", now.Add(time.Second))
	require.ErrorContains(t, err, "requires subject")
	require.False(t, updated)
	updated, err = storeValue.UpdateDeviceAuthorizationPoll(ctx, deviceID, now.Add(time.Second))
	require.NoError(t, err)
	require.True(t, updated)
	updated, err = storeValue.ApproveDeviceAuthorization(ctx, deviceID, claims.Sub, now.Add(2*time.Second))
	require.NoError(t, err)
	require.True(t, updated)
	loadedDevice, ok, err = storeValue.GetDeviceAuthorizationByDeviceCode(ctx, "sqlite-device-code")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, deviceAuthorizationStatusApproved, loadedDevice.Status)
	require.NotNil(t, loadedDevice.SubjectSub)
	require.Equal(t, claims.Sub, *loadedDevice.SubjectSub)
	require.Equal(t, int64(1), loadedDevice.PollCount)
	require.NotNil(t, loadedDevice.LastPollAt)
	require.NotNil(t, loadedDevice.ApprovedAt)
	updated, err = storeValue.ConsumeDeviceAuthorization(ctx, deviceID, now.Add(3*time.Second))
	require.NoError(t, err)
	require.True(t, updated)
	loadedDevice, ok, err = storeValue.GetDeviceAuthorizationByDeviceCode(ctx, "sqlite-device-code")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, deviceAuthorizationStatusConsumed, loadedDevice.Status)
	require.NotNil(t, loadedDevice.ConsumedAt)

	atomicDeviceID := ids.New()
	require.NoError(t, storeValue.CreateDeviceAuthorization(ctx, deviceAuthorization{
		ID:              atomicDeviceID,
		ClientID:        client.ID,
		ServiceID:       "example-a",
		Resource:        "https://mcp.example.com/example-a/mcp",
		Scope:           "mcp:example-a",
		DeviceCodeHash:  hashOpaqueValue("sqlite-atomic-device-code"),
		UserCodeHash:    hashOpaqueValue(normalizeUserCode("ATOM-1234")),
		UserCodeDisplay: "ATOM-1234",
		ExpiresAt:       now.Add(10 * time.Minute),
		CreatedAt:       now,
	}))
	updated, err = storeValue.ApproveDeviceAuthorization(ctx, atomicDeviceID, claims.Sub, now.Add(4*time.Second))
	require.NoError(t, err)
	require.True(t, updated)

	duplicateToken := models.NewToken()
	duplicateToken.SetClientID(client.ID)
	duplicateToken.SetUserID(claims.Sub)
	duplicateToken.SetScope("mcp:example-a")
	duplicateToken.SetAccess("duplicate-device-access")
	duplicateToken.SetAccessCreateAt(now)
	duplicateToken.SetAccessExpiresIn(time.Hour)
	setTokenInfoResource(duplicateToken, "https://mcp.example.com/example-a/mcp")
	require.NoError(t, storeValue.Create(ctx, duplicateToken))

	deviceToken := models.NewToken()
	deviceToken.SetClientID(client.ID)
	deviceToken.SetUserID(claims.Sub)
	deviceToken.SetScope("mcp:example-a")
	deviceToken.SetAccess("duplicate-device-access")
	deviceToken.SetAccessCreateAt(now)
	deviceToken.SetAccessExpiresIn(time.Hour)
	setTokenInfoResource(deviceToken, "https://mcp.example.com/example-a/mcp")
	setTokenInfoIssuedVia(deviceToken, oauthGrantDeviceCode)
	updated, err = storeValue.ConsumeDeviceAuthorizationAndCreateToken(ctx, atomicDeviceID, now.Add(5*time.Second), deviceToken)
	require.Error(t, err)
	require.False(t, updated)
	loadedDevice, ok, err = storeValue.GetDeviceAuthorizationByDeviceCode(ctx, "sqlite-atomic-device-code")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, deviceAuthorizationStatusApproved, loadedDevice.Status)

	deviceToken.SetAccess("unique-device-access")
	updated, err = storeValue.ConsumeDeviceAuthorizationAndCreateToken(ctx, atomicDeviceID, now.Add(6*time.Second), deviceToken)
	require.NoError(t, err)
	require.True(t, updated)
	loadedDevice, ok, err = storeValue.GetDeviceAuthorizationByDeviceCode(ctx, "sqlite-atomic-device-code")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, deviceAuthorizationStatusConsumed, loadedDevice.Status)
	loadedDeviceToken, err := storeValue.GetByAccess(ctx, "unique-device-access")
	require.NoError(t, err)
	require.Equal(t, oauthGrantDeviceCode, tokenInfoIssuedVia(loadedDeviceToken))

	expiredID := ids.New()
	require.NoError(t, storeValue.CreateDeviceAuthorization(ctx, deviceAuthorization{
		ID:              expiredID,
		ClientID:        client.ID,
		ServiceID:       "example-a",
		Resource:        "https://mcp.example.com/example-a/mcp",
		Scope:           "mcp:example-a",
		DeviceCodeHash:  hashOpaqueValue("sqlite-expired-device-code"),
		UserCodeHash:    hashOpaqueValue(normalizeUserCode("EEEE-0000")),
		UserCodeDisplay: "EEEE-0000",
		Interval:        5 * time.Second,
		ExpiresAt:       now.Add(-time.Second),
		CreatedAt:       now.Add(-10 * time.Minute),
	}))
	expiredCount, err := storeValue.MarkExpiredDeviceAuthorizations(ctx, now)
	require.NoError(t, err)
	require.Equal(t, int64(1), expiredCount)
	updated, err = storeValue.ApproveDeviceAuthorization(ctx, expiredID, claims.Sub, now)
	require.NoError(t, err)
	require.False(t, updated)
	prunedCount, err := storeValue.PruneExpiredDeviceAuthorizations(ctx, now.Add(time.Second))
	require.NoError(t, err)
	require.Equal(t, int64(1), prunedCount)

	token := models.NewToken()
	token.SetClientID(client.ID)
	token.SetUserID(claims.Sub)
	token.SetScope("mcp:example-a")
	token.SetAccess("access-token")
	token.SetAccessCreateAt(now)
	token.SetAccessExpiresIn(time.Hour)
	token.SetRefresh("refresh-token")
	token.SetRefreshCreateAt(now)
	token.SetRefreshExpiresIn(2 * time.Hour)
	setTokenInfoResource(token, "https://mcp.example.com/example-a/mcp")
	policyBindingID := "policy-binding-1"
	shareID := ids.New()
	setTokenInfoAuthorizationMetadata(token, `[]`, &policyBindingID, &shareID)
	require.NoError(t, storeValue.Create(ctx, token))

	accessToken, err := storeValue.GetByAccess(ctx, "access-token")
	require.NoError(t, err)
	require.Equal(t, "access-token", accessToken.GetAccess())
	require.Equal(t, "https://mcp.example.com/example-a/mcp", tokenInfoResource(accessToken))
	require.Equal(t, `[]`, tokenInfoAuthorizationDetails(accessToken))
	require.Equal(t, policyBindingID, tokenInfoPolicyBindingID(accessToken))
	loadedShareID, err := tokenInfoShareID(accessToken)
	require.NoError(t, err)
	require.NotNil(t, loadedShareID)
	require.Equal(t, shareID, *loadedShareID)
	wrongResourceCtx := context.WithValue(ctx, expectedResourceContextKey{}, "https://mcp.example.com/example-b/mcp")
	_, err = storeValue.GetByRefresh(wrongResourceCtx, "refresh-token")
	require.ErrorContains(t, err, "token was not issued for this MCP resource")
	rightResourceCtx := context.WithValue(ctx, expectedResourceContextKey{}, "https://mcp.example.com/example-a/mcp")
	refreshToken, err := storeValue.GetByRefresh(rightResourceCtx, "refresh-token")
	require.NoError(t, err)
	require.Equal(t, "access-token", refreshToken.GetAccess())
	require.Equal(t, "refresh-token", refreshToken.GetRefresh())
	require.Equal(t, "https://mcp.example.com/example-a/mcp", tokenInfoResource(refreshToken))
	require.Equal(t, `[]`, tokenInfoAuthorizationDetails(refreshToken))
	require.Equal(t, policyBindingID, tokenInfoPolicyBindingID(refreshToken))

	codeToken := models.NewToken()
	codeToken.SetClientID(client.ID)
	codeToken.SetUserID(claims.Sub)
	codeToken.SetRedirectURI(client.RedirectURIs[0])
	codeToken.SetScope("mcp:example-a")
	codeToken.SetCode("auth-code")
	codeToken.SetCodeCreateAt(now)
	codeToken.SetCodeExpiresIn(time.Minute)
	setTokenInfoResource(codeToken, "https://mcp.example.com/example-a/mcp")
	setTokenInfoAuthorizationMetadata(codeToken, `[]`, nil, nil)
	require.NoError(t, storeValue.Create(ctx, codeToken))
	_, err = storeValue.GetByCode(wrongResourceCtx, "auth-code")
	require.ErrorContains(t, err, "token was not issued for this MCP resource")
	loadedCode, err := storeValue.GetByCode(rightResourceCtx, "auth-code")
	require.NoError(t, err)
	require.Equal(t, "auth-code", loadedCode.GetCode())
	require.Equal(t, "https://mcp.example.com/example-a/mcp", tokenInfoResource(loadedCode))
	require.Equal(t, `[]`, tokenInfoAuthorizationDetails(loadedCode))

	invalidMetadataToken := models.NewToken()
	invalidMetadataToken.SetClientID(client.ID)
	invalidMetadataToken.SetUserID(claims.Sub)
	invalidMetadataToken.SetScope("mcp:example-a")
	invalidMetadataToken.SetAccess("invalid-metadata-access-token")
	invalidMetadataToken.SetAccessCreateAt(now)
	invalidMetadataToken.SetAccessExpiresIn(time.Hour)
	setTokenInfoResource(invalidMetadataToken, "https://mcp.example.com/example-a/mcp")
	setTokenInfoAuthorizationMetadata(invalidMetadataToken, `{"type":"not-an-array"}`, nil, nil)
	require.ErrorContains(t, storeValue.Create(ctx, invalidMetadataToken), "authorization_details must be valid JSON array")

	unsupportedMetadataToken := models.NewToken()
	unsupportedMetadataToken.SetClientID(client.ID)
	unsupportedMetadataToken.SetUserID(claims.Sub)
	unsupportedMetadataToken.SetScope("mcp:example-a")
	unsupportedMetadataToken.SetAccess("unsupported-metadata-access-token")
	unsupportedMetadataToken.SetAccessCreateAt(now)
	unsupportedMetadataToken.SetAccessExpiresIn(time.Hour)
	setTokenInfoResource(unsupportedMetadataToken, "https://mcp.example.com/example-a/mcp")
	setTokenInfoAuthorizationMetadata(unsupportedMetadataToken, `[{"type":"example_project","project_key":"ops-notes"}]`, nil, nil)
	require.ErrorContains(t, storeValue.Create(ctx, unsupportedMetadataToken), "authorization_details contains unsupported entries")

	deniedToken := models.NewToken()
	deniedToken.SetClientID(client.ID)
	deniedToken.SetUserID(claims.Sub)
	deniedToken.SetScope("mcp:example-b")
	deniedToken.SetAccess("denied-access-token")
	deniedToken.SetAccessCreateAt(now)
	deniedToken.SetAccessExpiresIn(time.Hour)
	deniedToken.SetRefresh("denied-refresh-token")
	deniedToken.SetRefreshCreateAt(now)
	deniedToken.SetRefreshExpiresIn(2 * time.Hour)
	require.NoError(t, storeValue.Create(ctx, deniedToken))
	_, err = storeValue.GetByRefresh(ctx, "denied-refresh-token")
	require.ErrorContains(t, err, "oauth client is not registered for requested scope")

	otherClient := registeredClient{ID: "client-other", Name: "Other", RedirectURIs: []string{"https://example.com/other"}, GrantTypes: []string{"authorization_code", "refresh_token"}, ResponseTypes: []string{"code"}, TokenEndpointAuthMethod: tokenEndpointAuthMethodNone, Scopes: []string{"mcp:example-a"}}
	require.NoError(t, storeValue.CreateClient(ctx, otherClient, claims.Sub))
	crossClientToken := models.NewToken()
	crossClientToken.SetClientID(client.ID)
	crossClientToken.SetUserID(claims.Sub)
	crossClientToken.SetScope("mcp:example-a")
	crossClientToken.SetAccess("cross-client-access-token")
	crossClientToken.SetAccessCreateAt(now)
	crossClientToken.SetAccessExpiresIn(time.Hour)
	crossClientToken.SetRefresh("cross-client-refresh-token")
	crossClientToken.SetRefreshCreateAt(now)
	crossClientToken.SetRefreshExpiresIn(2 * time.Hour)
	require.NoError(t, storeValue.Create(ctx, crossClientToken))
	wrongClientCtx := context.WithValue(ctx, refreshClientIDContextKey{}, otherClient.ID)
	_, err = storeValue.GetByRefresh(wrongClientCtx, "cross-client-refresh-token")
	require.ErrorContains(t, err, "refresh token was not issued to this OAuth client")

	revokedClient := registeredClient{ID: "client-2", Name: "Example Client 2", RedirectURIs: []string{"https://example.com/other-callback"}, GrantTypes: []string{"authorization_code", "refresh_token"}, ResponseTypes: []string{"code"}, TokenEndpointAuthMethod: tokenEndpointAuthMethodNone, Scopes: []string{"mcp:example-a"}}
	require.NoError(t, storeValue.CreateClient(ctx, revokedClient, claims.Sub))
	revokedToken := models.NewToken()
	revokedToken.SetClientID(revokedClient.ID)
	revokedToken.SetUserID(claims.Sub)
	revokedToken.SetScope("mcp:example-a")
	revokedToken.SetAccess("revoked-access-token")
	revokedToken.SetAccessCreateAt(now)
	revokedToken.SetAccessExpiresIn(time.Hour)
	revokedToken.SetRefresh("revoked-refresh-token")
	revokedToken.SetRefreshCreateAt(now)
	revokedToken.SetRefreshExpiresIn(2 * time.Hour)
	require.NoError(t, storeValue.Create(ctx, revokedToken))
	require.NoError(t, cpStore.ReplaceSubjectGrants(ctx, claims.Sub, nil))
	_, err = storeValue.GetByRefresh(ctx, "revoked-refresh-token")
	require.ErrorContains(t, err, "requested scope is not granted")

	_, err = storeValue.db.ExecContext(ctx, `
INSERT INTO oauth_sessions (session_id, subject_sub, client_id, service_id, redirect_uri, scope, access_token_hash, access_create_at, access_expires_in_seconds)
VALUES (?, ?, ?, 'example-a', 'https://example.com/callback', 'mcp:example-a', ?, ?, 3600);`, make([]byte, 16), claims.Sub, client.ID, hashOpaqueValue("corrupt-access-token"), formatSQLiteTime(now))
	require.NoError(t, err)
	_, err = storeValue.GetByAccess(ctx, "corrupt-access-token")
	require.ErrorContains(t, err, "parse oauth session id")

	require.NoError(t, storeValue.RecordAuditEvent(ctx, edgeAuditEvent{CorrelationID: "correlation-1", ActorSubjectSub: claims.Sub, ServiceID: "example-a", EventType: "test.audit", EventStatus: "ok", Payload: map[string]any{"source": "integration-test"}}))
	auditCount, err := storeValue.queries.CountAuditEventsByType(ctx, platformdb.CountAuditEventsByTypeParams{EventType: "test.audit"})
	require.NoError(t, err)
	require.Equal(t, int64(1), auditCount)
}

func TestDatabaseResolverAllowsStaticUpstreamExternalHost(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()
	databaseURL := "file:" + filepath.Join(t.TempDir(), "mcp-platform.db")

	cpStore, err := controlplane.NewStore(ctx, databaseURL, logger)
	require.NoError(t, err)
	defer cpStore.Close()
	require.NoError(t, cpStore.RunMigrations(ctx))
	require.NoError(t, cpStore.SeedServiceCatalog(ctx))
	registerSQLiteEdgeTestService(t, ctx, cpStore, databaseURL, "example-a", false)

	subject := domain.Subject{Sub: "static-user", SubjectKey: "static-user"}
	require.NoError(t, cpStore.UpsertManualServiceGrant(ctx, subject, "example-a"))
	require.NoError(t, cpStore.UpsertStaticTenantUpstream(ctx, subject, "example-a", "https://mcp.lan:9443", time.Now().UTC()))

	secretPath := filepath.Join(t.TempDir(), "session-key")
	require.NoError(t, os.WriteFile(secretPath, []byte("test-session-encryption-key"), 0o600))
	edgeStore, err := newSQLiteEdgeStateStore(ctx, Config{PlatformDatabaseURL: databaseURL, SessionEncryptionKeyPath: secretPath}, logger)
	require.NoError(t, err)
	defer edgeStore.Close()

	cache := NewCatalogCache(edgeStore, logger)
	require.NoError(t, cache.Refresh(ctx))
	resolver, err := NewDatabaseResolver(cache, edgeStore)
	require.NoError(t, err)
	previousLookup := lookupTenantUpstreamIP
	lookupTenantUpstreamIP = func(host string) ([]net.IP, error) {
		require.Equal(t, "mcp.lan", host)
		return []net.IP{net.ParseIP("192.168.1.10")}, nil
	}
	t.Cleanup(func() { lookupTenantUpstreamIP = previousLookup })

	target, err := resolver.Resolve(ctx, "example-a", subject.Sub)
	require.NoError(t, err)
	require.Equal(t, "mcp.lan:9443", target.BaseURL.Host)
}

func TestDatabaseResolverRejectsProvisionedUpstreamHostDrift(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()
	databaseURL := "file:" + filepath.Join(t.TempDir(), "mcp-platform.db")

	cpStore, err := controlplane.NewStore(ctx, databaseURL, logger)
	require.NoError(t, err)
	defer cpStore.Close()
	require.NoError(t, cpStore.RunMigrations(ctx))
	require.NoError(t, cpStore.SeedServiceCatalog(ctx))
	registerSQLiteEdgeTestService(t, ctx, cpStore, databaseURL, "example-a", true)
	subject := domain.Subject{Sub: "provisioned-user", SubjectKey: "provisioned-user"}
	require.NoError(t, cpStore.UpsertSubject(ctx, subject))
	require.NoError(t, cpStore.ReplaceSubjectGrants(ctx, subject.Sub, []controlplane.ServiceGrant{{SubjectSub: subject.Sub, ServiceID: "example-a", SourceGroup: "manual", GrantedAt: time.Now().UTC(), LastSyncedAt: time.Now().UTC()}}))
	require.NoError(t, cpStore.ReconcileDesiredTenants(ctx))
	tenants, err := cpStore.ListTenantInstances(ctx)
	require.NoError(t, err)
	require.Len(t, tenants, 1)
	now := time.Now().UTC()
	require.NoError(t, cpStore.UpdateTenantRuntimeStatus(ctx, controlplane.TenantRuntimeUpdate{TenantID: tenants[0].TenantID, RuntimeState: domain.TenantRuntimeStateReady, UpstreamURL: "http://unexpected:3031", LastHealthyAt: &now}))

	secretPath := filepath.Join(t.TempDir(), "session-key")
	require.NoError(t, os.WriteFile(secretPath, []byte("test-session-encryption-key"), 0o600))
	edgeStore, err := newSQLiteEdgeStateStore(ctx, Config{PlatformDatabaseURL: databaseURL, SessionEncryptionKeyPath: secretPath}, logger)
	require.NoError(t, err)
	defer edgeStore.Close()

	cache := NewCatalogCache(edgeStore, logger)
	require.NoError(t, cache.Refresh(ctx))
	resolver, err := NewDatabaseResolver(cache, edgeStore)
	require.NoError(t, err)

	_, err = resolver.Resolve(ctx, "example-a", "provisioned-user")
	require.ErrorContains(t, err, "tenant upstream host does not match internal DNS name")

	require.NoError(t, cpStore.UpdateTenantRuntimeStatus(ctx, controlplane.TenantRuntimeUpdate{TenantID: tenants[0].TenantID, RuntimeState: domain.TenantRuntimeStateReady, UpstreamURL: "http://" + tenants[0].InternalDNSName + ":9999/mcp", LastHealthyAt: &now}))
	_, err = resolver.Resolve(ctx, "example-a", "provisioned-user")
	require.ErrorContains(t, err, "tenant upstream port does not match service catalog")

	require.NoError(t, cpStore.UpdateTenantRuntimeStatus(ctx, controlplane.TenantRuntimeUpdate{TenantID: tenants[0].TenantID, RuntimeState: domain.TenantRuntimeStateReady, UpstreamURL: "http://" + tenants[0].InternalDNSName + ":3031/wrong", LastHealthyAt: &now}))
	_, err = resolver.Resolve(ctx, "example-a", "provisioned-user")
	require.ErrorContains(t, err, "tenant upstream path does not match service catalog")
}

func registerSQLiteEdgeTestService(t *testing.T, ctx context.Context, store *controlplane.Store, databaseURL string, serviceID string, builtin bool) {
	t.Helper()
	for _, entry := range testServiceCatalogEntries() {
		if entry.ServiceID != serviceID {
			continue
		}
		require.NoError(t, store.UpsertAdminServiceCatalogEntry(ctx, entry))
		if builtin {
			db, err := platformsqlite.Open(ctx, databaseURL)
			require.NoError(t, err)
			defer db.Close()
			_, err = db.ExecContext(ctx, `UPDATE service_catalog SET source = 'builtin' WHERE service_id = ?`, serviceID)
			require.NoError(t, err)
		}
		return
	}
	require.Failf(t, "missing test service", "service %q is not defined", serviceID)
}
