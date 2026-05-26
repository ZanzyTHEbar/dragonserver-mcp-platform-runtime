package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"dragonserver/mcp-platform/internal/catalog"
	"dragonserver/mcp-platform/internal/domain"

	"github.com/rs/zerolog"
)

const (
	deleteRequeueInterval     = 2 * time.Minute
	runtimeModeStaticUpstream = "static_upstream"
)

type CoolifyTenantRuntime struct {
	cfg           Config
	store         *Store
	coolify       CoolifyProvider
	secrets       SecretResolver
	logger        zerolog.Logger
	healthClient  *http.Client
	serviceByID   map[string]catalog.ServiceCatalogEntry
	templatesByID map[string]tenantTemplate
}

type tenantTemplate interface {
	Render(Config, TenantInstance, catalog.ServiceCatalogEntry, map[string]string) (renderedTenantService, error)
}

type renderedTenantService struct {
	CreateRequest CoolifyCreateServiceRequest
	UpdateRequest CoolifyUpdateServiceRequest
	EnvVars       []CoolifyEnvVar
	UpstreamURL   string
}

type staticTenantTemplate struct {
	render func(Config, TenantInstance, catalog.ServiceCatalogEntry, map[string]string) (renderedTenantService, error)
}

func (t staticTenantTemplate) Render(cfg Config, tenant TenantInstance, service catalog.ServiceCatalogEntry, secrets map[string]string) (renderedTenantService, error) {
	return t.render(cfg, tenant, service, secrets)
}

func NewCoolifyTenantRuntime(cfg Config, store *Store, clients *DependencyClients, logger zerolog.Logger) *CoolifyTenantRuntime {
	serviceByID := make(map[string]catalog.ServiceCatalogEntry)
	for _, entry := range catalog.DefaultCatalogV1() {
		serviceByID[entry.ServiceID] = entry
	}

	return &CoolifyTenantRuntime{
		cfg:     cfg,
		store:   store,
		coolify: clients.Coolify,
		secrets: clients.Infisical,
		logger:  logger,
		healthClient: &http.Client{
			Timeout: 5 * time.Second,
		},
		serviceByID:   serviceByID,
		templatesByID: map[string]tenantTemplate{},
	}
}

func (r *CoolifyTenantRuntime) Apply(ctx context.Context, tenant TenantInstance, plan TenantPlan) (RuntimeApplyResult, error) {
	if tenantRuntimeMode(tenant) == runtimeModeStaticUpstream && plan.Action == ReconcileActionDelete {
		return r.applyStaticUpstream(ctx, tenant, plan, catalog.ServiceCatalogEntry{})
	}

	service, ok, err := r.loadService(ctx, tenant.ServiceID)
	if err != nil {
		return RuntimeApplyResult{}, err
	}
	if !ok {
		return RuntimeApplyResult{}, fmt.Errorf("service %s is not present in the tenant runtime catalog", tenant.ServiceID)
	}
	if tenantRuntimeMode(tenant) == runtimeModeStaticUpstream {
		return r.applyStaticUpstream(ctx, tenant, plan, service)
	}

	switch plan.Action {
	case ReconcileActionNoop:
		if tenant.RuntimeState == domain.TenantRuntimeStateReady ||
			tenant.RuntimeState == domain.TenantRuntimeStateProvisioning ||
			tenant.RuntimeState == domain.TenantRuntimeStateDegraded {
			return r.observe(ctx, tenant, service)
		}
		return RuntimeApplyResult{
			Status: "noop",
			Details: map[string]any{
				"runtime_state": tenant.RuntimeState,
			},
		}, nil

	case ReconcileActionEnsure:
		return r.ensure(ctx, tenant, service)

	case ReconcileActionEnable:
		return r.enable(ctx, tenant, service)

	case ReconcileActionDisable:
		return r.disable(ctx, tenant)

	case ReconcileActionDelete:
		return r.delete(ctx, tenant)

	default:
		return RuntimeApplyResult{}, fmt.Errorf("unsupported reconcile action %s", plan.Action)
	}
}

func (r *CoolifyTenantRuntime) loadService(ctx context.Context, serviceID string) (catalog.ServiceCatalogEntry, bool, error) {
	if service, ok := r.serviceByID[serviceID]; ok {
		return service, true, nil
	}
	if r.store == nil {
		return catalog.ServiceCatalogEntry{}, false, nil
	}
	service, err := r.store.GetEnabledServiceCatalogEntry(ctx, serviceID)
	if err != nil {
		return catalog.ServiceCatalogEntry{}, false, err
	}
	return service, true, nil
}

func (r *CoolifyTenantRuntime) applyStaticUpstream(ctx context.Context, tenant TenantInstance, plan TenantPlan, service catalog.ServiceCatalogEntry) (RuntimeApplyResult, error) {
	switch plan.Action {
	case ReconcileActionNoop, ReconcileActionEnsure, ReconcileActionEnable:
		if strings.TrimSpace(tenant.UpstreamURL) == "" {
			return RuntimeApplyResult{}, fmt.Errorf("static upstream url is not configured for tenant %s", tenant.TenantID)
		}
		return r.observeWithURL(ctx, tenant, service, "", tenant.UpstreamURL)
	case ReconcileActionDisable:
		if err := r.store.UpdateTenantRuntimeStatus(ctx, TenantRuntimeUpdate{
			TenantID:     tenant.TenantID,
			RuntimeState: domain.TenantRuntimeStateDisabled,
			LastError:    "",
		}); err != nil {
			return RuntimeApplyResult{}, err
		}
		return RuntimeApplyResult{Status: "disabled", ObservedState: domain.TenantRuntimeStateDisabled, LastError: stringPointer("")}, nil
	case ReconcileActionDelete:
		return RuntimeApplyResult{Status: "deleted", ObservedState: domain.TenantRuntimeStateDeleting, LastError: stringPointer(""), DeleteCompleted: true}, nil
	default:
		return RuntimeApplyResult{}, fmt.Errorf("unsupported reconcile action %s", plan.Action)
	}
}

func (r *CoolifyTenantRuntime) ensure(ctx context.Context, tenant TenantInstance, service catalog.ServiceCatalogEntry) (RuntimeApplyResult, error) {
	rendered, err := r.renderTenant(ctx, tenant, service)
	if err != nil {
		return RuntimeApplyResult{}, err
	}

	serviceUUID := tenant.CoolifyResourceID
	if serviceUUID != "" {
		_, err := r.coolify.GetService(ctx, serviceUUID)
		if err != nil {
			if IsHTTPStatus(err, http.StatusNotFound) {
				serviceUUID = ""
			} else {
				return RuntimeApplyResult{}, err
			}
		}
	}

	created := false
	if serviceUUID == "" {
		createResponse, err := r.coolify.CreateService(ctx, rendered.CreateRequest)
		if err != nil {
			_ = r.store.UpdateTenantRuntimeStatus(ctx, TenantRuntimeUpdate{
				TenantID:               tenant.TenantID,
				RuntimeState:           domain.TenantRuntimeStateDegraded,
				LastError:              err.Error(),
				ClearRuntimeReferences: true,
			})
			return RuntimeApplyResult{}, err
		}
		serviceUUID = createResponse.UUID
		created = true
	}

	existingService, err := r.coolify.GetService(ctx, serviceUUID)
	if err != nil {
		return RuntimeApplyResult{}, err
	}
	metadataDrift := existingService.Name != rendered.UpdateRequest.Name || existingService.Description != rendered.UpdateRequest.Description
	composeDrift := existingService.DockerComposeRaw != rendered.UpdateRequest.DockerComposeRaw
	if metadataDrift || composeDrift {
		if err := r.coolify.UpdateService(ctx, serviceUUID, rendered.UpdateRequest); err != nil {
			return RuntimeApplyResult{}, err
		}
	}

	currentEnvs, err := r.coolify.ListServiceEnvs(ctx, serviceUUID)
	if err != nil {
		return RuntimeApplyResult{}, err
	}
	envDrift := envsDrifted(currentEnvs, rendered.EnvVars)
	if envDrift {
		if _, err := r.coolify.UpdateServiceEnvsBulk(ctx, serviceUUID, rendered.EnvVars); err != nil {
			return RuntimeApplyResult{}, err
		}
	}

	if created || envDrift || composeDrift {
		if _, err := r.coolify.RestartService(ctx, serviceUUID, false); err != nil {
			return RuntimeApplyResult{}, err
		}
	}

	if err := r.store.UpdateTenantRuntimeStatus(ctx, TenantRuntimeUpdate{
		TenantID:          tenant.TenantID,
		RuntimeState:      domain.TenantRuntimeStateProvisioning,
		CoolifyResourceID: serviceUUID,
		UpstreamURL:       rendered.UpstreamURL,
		LastError:         "",
	}); err != nil {
		return RuntimeApplyResult{}, err
	}

	result, err := r.observeWithURL(ctx, tenant, service, serviceUUID, rendered.UpstreamURL)
	if err != nil {
		return RuntimeApplyResult{}, err
	}
	if created {
		if result.Details == nil {
			result.Details = make(map[string]any)
		}
		result.Details["created"] = true
	}
	if envDrift {
		if result.Details == nil {
			result.Details = make(map[string]any)
		}
		result.Details["env_drift_corrected"] = true
	}
	if composeDrift {
		if result.Details == nil {
			result.Details = make(map[string]any)
		}
		result.Details["compose_drift_corrected"] = true
	}
	return result, nil
}

func (r *CoolifyTenantRuntime) enable(ctx context.Context, tenant TenantInstance, service catalog.ServiceCatalogEntry) (RuntimeApplyResult, error) {
	if tenant.CoolifyResourceID == "" {
		return r.ensure(ctx, tenant, service)
	}

	if _, err := r.coolify.StartService(ctx, tenant.CoolifyResourceID); err != nil {
		if IsHTTPStatus(err, http.StatusNotFound) {
			return r.ensure(ctx, tenant, service)
		}
		return RuntimeApplyResult{}, err
	}
	if err := r.store.UpdateTenantRuntimeStatus(ctx, TenantRuntimeUpdate{
		TenantID:     tenant.TenantID,
		RuntimeState: domain.TenantRuntimeStateProvisioning,
		LastError:    "",
	}); err != nil {
		return RuntimeApplyResult{}, err
	}

	return r.observe(ctx, tenant, service)
}

func (r *CoolifyTenantRuntime) disable(ctx context.Context, tenant TenantInstance) (RuntimeApplyResult, error) {
	if tenant.CoolifyResourceID != "" {
		if _, err := r.coolify.StopService(ctx, tenant.CoolifyResourceID, true); err != nil && !IsHTTPStatus(err, http.StatusNotFound) {
			return RuntimeApplyResult{}, err
		}
	}

	if err := r.store.UpdateTenantRuntimeStatus(ctx, TenantRuntimeUpdate{
		TenantID:     tenant.TenantID,
		RuntimeState: domain.TenantRuntimeStateDisabled,
		LastError:    "",
	}); err != nil {
		return RuntimeApplyResult{}, err
	}

	return RuntimeApplyResult{
		Status:        "disabled",
		ObservedState: domain.TenantRuntimeStateDisabled,
		LastError:     stringPointer(""),
		Details: map[string]any{
			"service_uuid": tenant.CoolifyResourceID,
		},
	}, nil
}

func (r *CoolifyTenantRuntime) delete(ctx context.Context, tenant TenantInstance) (RuntimeApplyResult, error) {
	if tenant.CoolifyResourceID == "" {
		return RuntimeApplyResult{
			Status:          "deleted",
			ObservedState:   domain.TenantRuntimeStateDeleting,
			LastError:       stringPointer(""),
			DeleteCompleted: true,
		}, nil
	}

	_, err := r.coolify.GetService(ctx, tenant.CoolifyResourceID)
	if err != nil {
		if IsHTTPStatus(err, http.StatusNotFound) {
			return RuntimeApplyResult{
				Status:          "deleted",
				ObservedState:   domain.TenantRuntimeStateDeleting,
				LastError:       stringPointer(""),
				DeleteCompleted: true,
				Details: map[string]any{
					"service_uuid": tenant.CoolifyResourceID,
				},
			}, nil
		}
		return RuntimeApplyResult{}, err
	}

	deleteQueued := false
	deleteRequeued := false
	shouldQueueDelete := tenant.RuntimeState != domain.TenantRuntimeStateDeleting
	if !shouldQueueDelete && shouldRequeueDelete(tenant.LastReconciledAt) {
		shouldQueueDelete = true
		deleteRequeued = true
	}
	if shouldQueueDelete {
		// Tenant deletion is intentionally destructive: desired_state=deleted is the
		// operator confirmation boundary, so Coolify volumes/configuration are removed.
		if _, err := r.coolify.DeleteService(ctx, tenant.CoolifyResourceID, CoolifyDeleteServiceOptions{
			DeleteConfigurations:    true,
			DeleteVolumes:           true,
			DockerCleanup:           true,
			DeleteConnectedNetworks: true,
		}); err != nil {
			if IsHTTPStatus(err, http.StatusNotFound) {
				return RuntimeApplyResult{
					Status:          "deleted",
					ObservedState:   domain.TenantRuntimeStateDeleting,
					LastError:       stringPointer(""),
					DeleteCompleted: true,
					Details: map[string]any{
						"service_uuid": tenant.CoolifyResourceID,
					},
				}, nil
			}
			return RuntimeApplyResult{}, err
		}
		deleteQueued = true
	}

	if err := r.store.UpdateTenantRuntimeStatus(ctx, TenantRuntimeUpdate{
		TenantID:     tenant.TenantID,
		RuntimeState: domain.TenantRuntimeStateDeleting,
		LastError:    "",
	}); err != nil {
		return RuntimeApplyResult{}, err
	}

	return RuntimeApplyResult{
		Status:            "deleting",
		ObservedState:     domain.TenantRuntimeStateDeleting,
		LastError:         stringPointer(""),
		SkipReconcileMark: !deleteQueued,
		Details: map[string]any{
			"service_uuid":    tenant.CoolifyResourceID,
			"delete_queued":   deleteQueued,
			"delete_requeued": deleteRequeued,
		},
	}, nil
}

func (r *CoolifyTenantRuntime) observe(ctx context.Context, tenant TenantInstance, service catalog.ServiceCatalogEntry) (RuntimeApplyResult, error) {
	upstreamURL := tenant.UpstreamURL
	if upstreamURL == "" {
		upstreamURL = buildUpstreamURL(tenant, service)
	}
	return r.observeWithURL(ctx, tenant, service, tenant.CoolifyResourceID, upstreamURL)
}

func (r *CoolifyTenantRuntime) observeWithURL(ctx context.Context, tenant TenantInstance, service catalog.ServiceCatalogEntry, serviceUUID string, upstreamURL string) (RuntimeApplyResult, error) {
	healthy, statusDetail, err := r.probeTenantHealth(ctx, tenant, service, serviceUUID, upstreamURL)
	if err != nil {
		if attestErr := r.recordRuntimeObservation(ctx, tenant, service, serviceUUID, upstreamURL, false, err.Error(), time.Now().UTC()); attestErr != nil {
			r.logger.Warn().Err(attestErr).Str("tenant_id", tenant.TenantID.String()).Msg("runtime attestation observation failed")
		}
		if updateErr := r.store.UpdateTenantRuntimeStatus(ctx, TenantRuntimeUpdate{
			TenantID:          tenant.TenantID,
			RuntimeState:      domain.TenantRuntimeStateDegraded,
			CoolifyResourceID: serviceUUID,
			UpstreamURL:       upstreamURL,
			LastError:         err.Error(),
		}); updateErr != nil {
			return RuntimeApplyResult{}, updateErr
		}
		return RuntimeApplyResult{
			Status:        "degraded",
			ObservedState: domain.TenantRuntimeStateDegraded,
			LastError:     stringPointer(err.Error()),
			Details: map[string]any{
				"health": statusDetail,
			},
		}, nil
	}

	now := time.Now().UTC()
	if healthy {
		if err := r.recordRuntimeObservation(ctx, tenant, service, serviceUUID, upstreamURL, true, statusDetail, now); err != nil {
			r.logger.Warn().Err(err).Str("tenant_id", tenant.TenantID.String()).Msg("runtime attestation observation failed")
		}
		if err := r.store.UpdateTenantRuntimeStatus(ctx, TenantRuntimeUpdate{
			TenantID:          tenant.TenantID,
			RuntimeState:      domain.TenantRuntimeStateReady,
			CoolifyResourceID: serviceUUID,
			UpstreamURL:       upstreamURL,
			LastHealthyAt:     &now,
			LastError:         "",
		}); err != nil {
			return RuntimeApplyResult{}, err
		}
		return RuntimeApplyResult{
			Status:        "ready",
			ObservedState: domain.TenantRuntimeStateReady,
			LastError:     stringPointer(""),
			Details: map[string]any{
				"health": statusDetail,
			},
		}, nil
	}

	if err := r.recordRuntimeObservation(ctx, tenant, service, serviceUUID, upstreamURL, false, statusDetail, now); err != nil {
		r.logger.Warn().Err(err).Str("tenant_id", tenant.TenantID.String()).Msg("runtime attestation observation failed")
	}
	if err := r.store.UpdateTenantRuntimeStatus(ctx, TenantRuntimeUpdate{
		TenantID:          tenant.TenantID,
		RuntimeState:      domain.TenantRuntimeStateDegraded,
		CoolifyResourceID: serviceUUID,
		UpstreamURL:       upstreamURL,
		LastError:         statusDetail,
	}); err != nil {
		return RuntimeApplyResult{}, err
	}
	return RuntimeApplyResult{
		Status:        "degraded",
		ObservedState: domain.TenantRuntimeStateDegraded,
		LastError:     stringPointer(statusDetail),
		Details: map[string]any{
			"health": statusDetail,
		},
	}, nil
}

func (r *CoolifyTenantRuntime) renderTenant(ctx context.Context, tenant TenantInstance, service catalog.ServiceCatalogEntry) (renderedTenantService, error) {
	template, ok := r.templatesByID[tenant.ServiceID]
	if !ok {
		return renderedTenantService{}, fmt.Errorf("tenant template for service %s is not configured", tenant.ServiceID)
	}

	secretValues := make(map[string]string, len(service.SecretContract))
	for _, secretDefinition := range service.SecretContract {
		secretReference := domain.BuildTenantSecretPath(tenant.SubjectKey, tenant.ServiceID, secretDefinition.Key)
		secretValue, err := r.secrets.ResolveSecretReference(ctx, secretReference)
		if err != nil {
			if secretDefinition.Required {
				return renderedTenantService{}, err
			}
			continue
		}
		secretValues[secretDefinition.Key] = secretValue
	}

	return template.Render(r.cfg, tenant, service, secretValues)
}

func (r *CoolifyTenantRuntime) probeTenantHealth(ctx context.Context, tenant TenantInstance, service catalog.ServiceCatalogEntry, serviceUUID string, upstreamURL string) (bool, string, error) {
	if serviceUUID != "" {
		expectedInternalDNSName := domain.BuildTenantInstanceName(tenant.ServiceID, tenant.SubjectKey)
		if tenant.InternalDNSName != "" && tenant.InternalDNSName != expectedInternalDNSName {
			return false, "", fmt.Errorf("tenant internal dns drift detected for %s", tenant.TenantID)
		}
	}
	if upstreamURL == "" {
		upstreamURL = buildUpstreamURL(tenant, service)
	}
	requestURL, err := staticUpstreamHealthURL(upstreamURL, service.HealthPath)
	if err != nil {
		return false, "", fmt.Errorf("build tenant health url for %s: %w", tenant.TenantID, err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return false, "", err
	}

	response, err := r.healthClient.Do(request)
	if err != nil {
		return false, "health request failed", err
	}
	defer response.Body.Close()

	bodyBytes, _ := io.ReadAll(io.LimitReader(response.Body, 1024))
	bodyText := strings.TrimSpace(string(bodyBytes))
	contentType := response.Header.Get("Content-Type")

	if response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices {
		return true, fmt.Sprintf("%s returned healthy status %d", service.ServiceID, response.StatusCode), nil
	}

	return false, fmt.Sprintf("unexpected health response: status=%d content_type=%s body=%s", response.StatusCode, contentType, bodyText), nil
}

func tenantRuntimeMode(tenant TenantInstance) string {
	if len(tenant.Metadata) == 0 {
		return ""
	}
	var metadata struct {
		RuntimeMode string `json:"runtime_mode"`
	}
	if err := json.Unmarshal(tenant.Metadata, &metadata); err != nil {
		return ""
	}
	return metadata.RuntimeMode
}

func buildUpstreamURL(tenant TenantInstance, service catalog.ServiceCatalogEntry) string {
	internalDNSName := domain.BuildTenantInstanceName(tenant.ServiceID, tenant.SubjectKey)
	return "http://" + internalDNSName + ":" + itoa(service.InternalPort) + service.InternalUpstreamPath
}

func stringPointer(value string) *string {
	return &value
}

func shouldRequeueDelete(lastReconciledAt *time.Time) bool {
	if lastReconciledAt == nil {
		return true
	}
	return time.Since(lastReconciledAt.UTC()) >= deleteRequeueInterval
}

func envsDrifted(current []CoolifyEnvVar, expected []CoolifyEnvVar) bool {
	currentByKey := make(map[string]CoolifyEnvVar, len(current))
	for _, currentEnv := range current {
		currentByKey[currentEnv.Key] = currentEnv
	}

	for _, expectedEnv := range expected {
		currentEnv, ok := currentByKey[expectedEnv.Key]
		if !ok {
			return true
		}
		if currentEnv.Value != expectedEnv.Value ||
			currentEnv.IsLiteral != expectedEnv.IsLiteral ||
			currentEnv.IsMultiline != expectedEnv.IsMultiline ||
			currentEnv.IsPreview != expectedEnv.IsPreview ||
			currentEnv.IsShownOnce != expectedEnv.IsShownOnce {
			return true
		}
	}
	return false
}

func (r *CoolifyTenantRuntime) recordRuntimeObservation(ctx context.Context, tenant TenantInstance, service catalog.ServiceCatalogEntry, serviceUUID string, upstreamURL string, healthy bool, statusDetail string, observedAt time.Time) error {
	if r.store == nil || tenant.TenantID.IsZero() {
		return nil
	}
	if observedAt.IsZero() {
		observedAt = time.Now().UTC()
	}
	imageRef := tenantImageForService(service)
	imageRefsJSON, err := json.Marshal([]string{imageRef})
	if err != nil {
		return fmt.Errorf("marshal runtime image refs: %w", err)
	}
	networkPolicy := map[string]string{
		"network":      r.cfg.DockerNetwork,
		"upstream_url": upstreamURL,
	}
	networkPolicyJSON, err := json.Marshal(networkPolicy)
	if err != nil {
		return fmt.Errorf("marshal runtime network policy: %w", err)
	}
	secretContractJSON, err := json.Marshal(service.SecretContract)
	if err != nil {
		return fmt.Errorf("marshal runtime secret contract: %w", err)
	}
	identityContextJSON, err := json.Marshal(service.IdentityContext.Normalized())
	if err != nil {
		return fmt.Errorf("marshal runtime identity context: %w", err)
	}
	composeHash := sha256String(service.ServiceID, service.InternalUpstreamPath, service.HealthPath, upstreamURL)
	envContractHash := sha256String(string(secretContractJSON), itoa(service.InternalPort), service.HealthPath, service.InternalUpstreamPath)
	secretContractHash := sha256String(string(secretContractJSON))
	identityContextHash := sha256String(string(identityContextJSON))
	healthStatus := "unhealthy"
	verdict := "warning"
	failureReasons := []string{statusDetail}
	if healthy {
		healthStatus = "healthy"
		verdict = "trusted"
		failureReasons = []string{}
	}
	rawSummaryJSON, err := json.Marshal(map[string]any{
		"status_detail": statusDetail,
		"upstream_url":  upstreamURL,
	})
	if err != nil {
		return fmt.Errorf("marshal runtime measurement summary: %w", err)
	}
	source := "http_probe"
	if tenantRuntimeMode(tenant) == runtimeModeStaticUpstream || serviceUUID == "" {
		source = "static_upstream"
	}
	failureReasonsJSON, err := json.Marshal(failureReasons)
	if err != nil {
		return fmt.Errorf("marshal runtime attestation failures: %w", err)
	}
	expiresAt := observedAt.Add(15 * time.Minute)
	return r.store.RecordTenantRuntimeObservation(ctx, TenantRuntimeObservation{
		Spec: TenantRuntimeSpec{
			TenantID:            tenant.TenantID,
			SpecVersion:         "observe-v1",
			ComposeHash:         composeHash,
			EnvContractHash:     envContractHash,
			SecretContractHash:  secretContractHash,
			ImageRefsJSON:       imageRefsJSON,
			NetworkPolicyJSON:   networkPolicyJSON,
			IdentityContextHash: identityContextHash,
			CreatedAt:           observedAt,
		},
		Measurement: TenantRuntimeMeasurement{
			TenantID:          tenant.TenantID,
			CoolifyResourceID: serviceUUID,
			Source:            source,
			ImageRef:          imageRef,
			ComposeHash:       composeHash,
			EnvContractHash:   envContractHash,
			NetworkJSON:       networkPolicyJSON,
			HealthStatus:      healthStatus,
			RawSummaryJSON:    rawSummaryJSON,
			MeasuredAt:        observedAt,
		},
		Attestation: TenantRuntimeAttestation{
			TenantID:           tenant.TenantID,
			PolicyVersion:      "observe-v1",
			Verdict:            verdict,
			FailureReasonsJSON: failureReasonsJSON,
			ExpiresAt:          &expiresAt,
			CreatedAt:          observedAt,
		},
	})
}

func tenantImageForService(service catalog.ServiceCatalogEntry) string {
	return service.UpstreamServiceName
}

func sha256String(parts ...string) string {
	hash := sha256.New()
	for _, part := range parts {
		_, _ = hash.Write([]byte(part))
		_, _ = hash.Write([]byte{0})
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil))
}

func buildCreateServiceRequest(cfg Config, tenant TenantInstance, compose string) CoolifyCreateServiceRequest {
	return CoolifyCreateServiceRequest{
		Type:             "docker-compose",
		Name:             tenant.TenantInstanceName,
		Description:      "MCP tenant " + tenant.ServiceID + " for " + tenant.SubjectKey,
		ProjectUUID:      cfg.CoolifyProjectUUID,
		EnvironmentName:  cfg.CoolifyEnvironmentName,
		EnvironmentUUID:  cfg.CoolifyEnvironmentUUID,
		ServerUUID:       cfg.CoolifyServerUUID,
		DestinationUUID:  cfg.CoolifyDestinationUUID,
		InstantDeploy:    true,
		DockerComposeRaw: compose,
	}
}

func buildUpdateServiceRequest(cfg Config, tenant TenantInstance, compose string) CoolifyUpdateServiceRequest {
	return CoolifyUpdateServiceRequest{
		Name:             tenant.TenantInstanceName,
		Description:      "MCP tenant " + tenant.ServiceID + " for " + tenant.SubjectKey,
		ProjectUUID:      cfg.CoolifyProjectUUID,
		EnvironmentName:  cfg.CoolifyEnvironmentName,
		EnvironmentUUID:  cfg.CoolifyEnvironmentUUID,
		ServerUUID:       cfg.CoolifyServerUUID,
		DestinationUUID:  cfg.CoolifyDestinationUUID,
		InstantDeploy:    true,
		DockerComposeRaw: compose,
	}
}
