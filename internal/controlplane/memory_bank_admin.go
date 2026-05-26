package controlplane

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"dragonserver/mcp-platform/internal/domain"
	"dragonserver/mcp-platform/internal/ids"
)

type memoryBankProjectRequest struct {
	DisplayName string          `json:"display_name"`
	RootPath    string          `json:"root_path"`
	Metadata    json.RawMessage `json:"metadata"`
	ArchivedAt  *time.Time      `json:"archived_at"`
}

type memoryBankShareRequest struct {
	CollaboratorSubjectSub string          `json:"collaborator_subject_sub"`
	Permission             string          `json:"permission"`
	ExpiresAt              *time.Time      `json:"expires_at"`
	Metadata               json.RawMessage `json:"metadata"`
}

func (a *App) handleMemoryBankProjects(w http.ResponseWriter, r *http.Request, ownerSubjectSub string, serviceID string, tail string) {
	if tail == "" {
		a.handleMemoryBankProjectList(w, r, ownerSubjectSub, serviceID)
		return
	}
	projectKey, subpath, _ := strings.Cut(tail, "/")
	if err := validateMemoryBankProjectKey(projectKey); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if subpath == "" {
		a.handleMemoryBankProjectPut(w, r, ownerSubjectSub, serviceID, projectKey)
		return
	}
	if subpath == "shares" {
		a.handleMemoryBankProjectShareCreate(w, r, ownerSubjectSub, serviceID, projectKey)
		return
	}
	http.NotFound(w, r)
}

func (a *App) handleMemoryBankShares(w http.ResponseWriter, r *http.Request, subjectSub string, serviceID string, tail string) {
	if tail == "" {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method_not_allowed"})
			return
		}
		includeInactive := parseAdminBoolQuery(r, "include_inactive")
		shares, err := a.store.ListMemoryBankProjectSharesForSubjectService(r.Context(), subjectSub, serviceID, includeInactive)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "list_memory_bank_shares_failed"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"subject_sub": subjectSub, "service_id": serviceID, "shares": shares})
		return
	}
	shareIDRaw, action, ok := strings.Cut(tail, "/")
	if !ok || action != "accept" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPut {
		w.Header().Set("Allow", http.MethodPut)
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method_not_allowed"})
		return
	}
	shareID, err := ids.Parse(shareIDRaw)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_share_id"})
		return
	}
	activated, err := a.store.ActivateMemoryBankProjectShare(r.Context(), shareID, subjectSub, time.Now().UTC())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "activate_memory_bank_share_failed"})
		return
	}
	if !activated {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "share_not_found"})
		return
	}
	share, err := a.store.GetMemoryBankProjectShare(r.Context(), shareID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "load_memory_bank_share_failed"})
		return
	}
	writeJSON(w, http.StatusOK, share)
}

func (a *App) handleMemoryBankProjectList(w http.ResponseWriter, r *http.Request, ownerSubjectSub string, serviceID string) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method_not_allowed"})
		return
	}
	projects, err := a.store.ListMemoryBankProjectsByOwner(r.Context(), ownerSubjectSub, serviceID, parseAdminBoolQuery(r, "include_archived"))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "list_memory_bank_projects_failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"subject_sub": ownerSubjectSub, "service_id": serviceID, "projects": projects})
}

func (a *App) handleMemoryBankProjectPut(w http.ResponseWriter, r *http.Request, ownerSubjectSub string, serviceID string, projectKey string) {
	if r.Method != http.MethodPut {
		w.Header().Set("Allow", http.MethodPut)
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method_not_allowed"})
		return
	}
	var request memoryBankProjectRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_json"})
		return
	}
	displayName := strings.TrimSpace(request.DisplayName)
	rootPath := strings.TrimSpace(request.RootPath)
	if displayName == "" || rootPath == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "display_name_and_root_path_required"})
		return
	}
	if !strings.HasPrefix(rootPath, "/") || strings.Contains(rootPath, "..") {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_root_path"})
		return
	}
	tenant, err := a.store.GetTenantInstanceBySubjectService(r.Context(), ownerSubjectSub, serviceID)
	if err != nil {
		if errors.Is(err, ErrTenantInstanceNotFound) {
			writeJSON(w, http.StatusConflict, map[string]any{"error": "tenant_not_found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "load_memory_bank_tenant_failed"})
		return
	}
	metadata, err := jsonRawMessageOrDefault(request.Metadata, "{}")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_metadata"})
		return
	}
	projectID, err := a.store.UpsertMemoryBankProject(r.Context(), MemoryBankProject{OwnerSubjectSub: ownerSubjectSub, OwnerTenantID: tenant.TenantID, ServiceID: serviceID, ProjectKey: projectKey, DisplayName: displayName, RootPath: rootPath, Metadata: metadata, ArchivedAt: request.ArchivedAt})
	if err != nil {
		if errors.Is(err, ErrMemoryBankInvalidMetadata) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_metadata"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "upsert_memory_bank_project_failed"})
		return
	}
	project, err := a.store.GetMemoryBankProject(r.Context(), projectID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "load_memory_bank_project_failed"})
		return
	}
	writeJSON(w, http.StatusOK, project)
}

func (a *App) handleMemoryBankProjectShareCreate(w http.ResponseWriter, r *http.Request, ownerSubjectSub string, serviceID string, projectKey string) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method_not_allowed"})
		return
	}
	var request memoryBankShareRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_json"})
		return
	}
	collaborator := strings.TrimSpace(request.CollaboratorSubjectSub)
	permission := strings.TrimSpace(request.Permission)
	if collaborator == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "collaborator_subject_sub_required"})
		return
	}
	if permission != "view" && permission != "edit" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_permission"})
		return
	}
	project, err := a.store.GetMemoryBankProjectByOwnerKey(r.Context(), ownerSubjectSub, serviceID, projectKey)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "project_not_found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "load_memory_bank_project_failed"})
		return
	}
	if project.ArchivedAt != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "project_archived"})
		return
	}
	if err := a.store.UpsertSubject(r.Context(), domain.Subject{Sub: collaborator, SubjectKey: domain.DeriveSubjectKey(collaborator)}); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "upsert_collaborator_subject_failed"})
		return
	}
	metadata, err := jsonRawMessageOrDefault(request.Metadata, "{}")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_metadata"})
		return
	}
	shareID, err := a.store.CreateMemoryBankProjectShare(r.Context(), MemoryBankProjectShare{ProjectID: project.ProjectID, OwnerSubjectSub: ownerSubjectSub, CollaboratorSubjectSub: collaborator, Permission: permission, State: "pending", Source: "admin", CreatedBySubjectSub: ownerSubjectSub, ExpiresAt: request.ExpiresAt, Metadata: metadata})
	if err != nil {
		if errors.Is(err, ErrMemoryBankProjectNotShareable) {
			writeJSON(w, http.StatusConflict, map[string]any{"error": "project_archived"})
			return
		}
		if errors.Is(err, ErrMemoryBankInvalidMetadata) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_metadata"})
			return
		}
		if errors.Is(err, ErrMemoryBankShareExpired) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "share_expires_at_must_be_future"})
			return
		}
		writeJSON(w, http.StatusConflict, map[string]any{"error": "create_memory_bank_share_failed"})
		return
	}
	share, err := a.store.GetMemoryBankProjectShare(r.Context(), shareID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "load_memory_bank_share_failed"})
		return
	}
	writeJSON(w, http.StatusCreated, share)
}

func (a *App) handleMemoryBankShareDelete(w http.ResponseWriter, r *http.Request, ownerSubjectSub string, serviceID string, shareIDRaw string) {
	if r.Method != http.MethodDelete {
		w.Header().Set("Allow", http.MethodDelete)
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method_not_allowed"})
		return
	}
	shareID, err := ids.Parse(shareIDRaw)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_share_id"})
		return
	}
	share, err := a.store.GetMemoryBankProjectShare(r.Context(), shareID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "share_not_found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "load_memory_bank_share_failed"})
		return
	}
	if share.OwnerSubjectSub != ownerSubjectSub || share.ProjectID.IsZero() {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "share_not_found"})
		return
	}
	project, err := a.store.GetMemoryBankProject(r.Context(), share.ProjectID)
	if err != nil || project.ServiceID != serviceID {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "share_not_found"})
		return
	}
	revoked, err := a.store.RevokeMemoryBankProjectShare(r.Context(), shareID, time.Now().UTC())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "revoke_memory_bank_share_failed"})
		return
	}
	if !revoked {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "share_not_found"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func parseMemoryBankAdminTail(tail string) (serviceID string, resource string, resourceTail string, ok bool) {
	if !strings.HasPrefix(tail, "services/") {
		return "", "", "", false
	}
	remainder := strings.TrimPrefix(tail, "services/")
	serviceID, remainder, ok = strings.Cut(remainder, "/")
	if !ok || serviceID == "" {
		return "", "", "", false
	}
	resource, resourceTail, _ = strings.Cut(remainder, "/")
	if resource != "projects" && resource != "shares" {
		return "", "", "", false
	}
	return serviceID, resource, resourceTail, true
}

func validateMemoryBankProjectKey(projectKey string) error {
	if !controlPlaneServiceIDPattern.MatchString(projectKey) {
		return errors.New("invalid_project_key")
	}
	return nil
}

func parseAdminBoolQuery(r *http.Request, name string) bool {
	value := strings.TrimSpace(strings.ToLower(r.URL.Query().Get(name)))
	return value == "1" || value == "true" || value == "yes"
}

func jsonRawMessageOrDefault(value json.RawMessage, defaultValue string) (json.RawMessage, error) {
	if strings.TrimSpace(string(value)) == "" {
		return json.RawMessage(defaultValue), nil
	}
	if !json.Valid(value) {
		return nil, ErrMemoryBankInvalidMetadata
	}
	return value, nil
}
