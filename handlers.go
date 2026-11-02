package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"log/slog"
)

type TaskRequest struct {
	Task           string          `json:"task"`
	Input          string          `json:"input"`
	Provider       string          `json:"provider,omitempty"`
	Model          string          `json:"model,omitempty"`
	MaxAttempts    int             `json:"max_attempts,omitempty"`
	Temperature    *float64        `json:"temperature,omitempty"`
	MaxTokens      *int            `json:"max_tokens,omitempty"`
	TimeoutSeconds int             `json:"timeout_seconds,omitempty"`
	ResponseFormat json.RawMessage `json:"response_format,omitempty"`
	Webhook        *WebhookRequest `json:"webhook,omitempty"`
}

type WebhookRequest struct {
	URL    string `json:"url"`
	Secret string `json:"secret"`
}

type TaskAcceptedResponse struct {
	ID        string `json:"id"`
	Provider  string `json:"provider"`
	Model     string `json:"model,omitempty"`
	Status    string `json:"status"`
	StatusURL string `json:"status_url"`
}

func createTaskHandler(store TaskStore, provider Provider, config Config, metrics *Metrics) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal := principalFromContext(r.Context())
		var request TaskRequest
		if err := decodeJSONBody(w, r, config.MaxInputBytes+(64<<10), &request); err != nil {
			status := http.StatusBadRequest
			if errors.Is(err, errBodyTooLarge) {
				status = http.StatusRequestEntityTooLarge
			}
			writeError(w, status, err.Error())
			return
		}
		request.Task = strings.TrimSpace(request.Task)
		if request.Task == "" {
			writeError(w, http.StatusBadRequest, "task is required")
			return
		}
		if len(request.Task) > 120 {
			writeError(w, http.StatusBadRequest, "task must be 120 bytes or fewer")
			return
		}
		if strings.TrimSpace(request.Input) == "" {
			writeError(w, http.StatusBadRequest, "input is required")
			return
		}
		if int64(len(request.Input)) > config.MaxInputBytes {
			writeError(w, http.StatusRequestEntityTooLarge, "input exceeds the configured size limit")
			return
		}
		providerName, err := resolveTaskProviderName(request.Provider, provider, config)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		request.Provider = providerName
		request.Model = strings.TrimSpace(request.Model)
		if providerName == "openrouter" {
			if request.Model == "" {
				request.Model = strings.TrimSpace(config.OpenRouterModel)
			}
			if request.Model == "" {
				request.Model = "openrouter/free"
			}
			if len(request.Model) > 200 || strings.ContainsAny(request.Model, " \t\r\n") {
				writeError(w, http.StatusBadRequest, "model must be a model ID of 200 characters or fewer")
				return
			}
		} else if request.Model != "" {
			writeError(w, http.StatusBadRequest, "model selection is only supported by the OpenRouter provider")
			return
		}
		if request.MaxAttempts == 0 {
			request.MaxAttempts = 3
		}
		if request.MaxAttempts < 1 || request.MaxAttempts > 10 {
			writeError(w, http.StatusBadRequest, "max_attempts must be between 1 and 10")
			return
		}
		if request.Temperature != nil && (*request.Temperature < 0 || *request.Temperature > 2) {
			writeError(w, http.StatusBadRequest, "temperature must be between 0 and 2")
			return
		}
		if request.MaxTokens != nil && (*request.MaxTokens < 1 || *request.MaxTokens > 32768) {
			writeError(w, http.StatusBadRequest, "max_tokens must be between 1 and 32768")
			return
		}
		maxTimeoutSeconds := int64(config.TaskTimeout / time.Second)
		if request.TimeoutSeconds < 0 || (request.TimeoutSeconds > 0 && int64(request.TimeoutSeconds) > maxTimeoutSeconds) {
			writeError(w, http.StatusBadRequest, "timeout_seconds must be positive and no greater than the configured TASK_TIMEOUT")
			return
		}
		if err := validateTaskResponseFormat(request.ResponseFormat); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if providerName != "openrouter" && (request.Temperature != nil || request.MaxTokens != nil || len(request.ResponseFormat) > 0) {
			writeError(w, http.StatusBadRequest, "temperature, max_tokens, and response_format require the OpenRouter provider")
			return
		}
		model := request.Model
		var encryptedWebhookSecret []byte
		webhookURL := ""
		if request.Webhook != nil {
			if strings.TrimSpace(request.Webhook.URL) == "" || strings.TrimSpace(request.Webhook.Secret) == "" {
				writeError(w, http.StatusBadRequest, "webhook requires both url and secret")
				return
			}
			if len(request.Webhook.Secret) < 16 || len(request.Webhook.Secret) > 512 {
				writeError(w, http.StatusBadRequest, "webhook secret must be between 16 and 512 bytes")
				return
			}
			webhookURL, err = validateWebhookURL(request.Webhook.URL)
			if err != nil {
				writeError(w, http.StatusBadRequest, err.Error())
				return
			}
			encryptedWebhookSecret, err = encryptWebhookSecret(config.WebhookEncryptionKey, request.Webhook.Secret)
			if err != nil {
				writeError(w, http.StatusServiceUnavailable, "webhooks are not configured on this server")
				return
			}
		}
		idempotencyKey, err := parseIdempotencyKey(r.Header.Get("Idempotency-Key"))
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		requestHash := hashTaskRequest(request)
		newTask := TaskRecord{
			Task: request.Task, Input: request.Input, Provider: providerName, Model: model,
			MaxAttempts: request.MaxAttempts, Temperature: request.Temperature,
			MaxOutputTokens: request.MaxTokens, TimeoutSeconds: request.TimeoutSeconds,
			ResponseFormat: request.ResponseFormat, WebhookURL: webhookURL,
			WebhookSecretEncrypted: encryptedWebhookSecret,
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		task, created, err := store.Create(ctx, principal.WorkspaceID, newTask, idempotencyKey, requestHash)
		cancel()
		if errors.Is(err, ErrIdempotencyConflict) {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		if err != nil {
			slog.Error("could not queue task", "workspace_id", principal.WorkspaceID, "error", err)
			writeError(w, http.StatusInternalServerError, "could not queue task")
			return
		}
		if created {
			metrics.TaskSubmitted(principal.WorkspaceID)
		}
		response := TaskAcceptedResponse{ID: task.ID, Provider: task.Provider, Model: task.Model, Status: task.Status, StatusURL: "/v1/tasks/" + task.ID}
		w.Header().Set("Location", response.StatusURL)
		status := http.StatusAccepted
		if !created {
			status = http.StatusOK
		}
		writeJSON(w, status, response)
	})
}

var errBodyTooLarge = errors.New("request body is too large")

func decodeJSONBody(w http.ResponseWriter, r *http.Request, maxBytes int64, destination any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			return errBodyTooLarge
		}
		return errors.New("invalid JSON body or unknown field")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("request must contain exactly one JSON value")
	}
	return nil
}

func parseIdempotencyKey(raw string) (*string, error) {
	if raw == "" {
		return nil, nil
	}
	if len(raw) > 128 || strings.TrimSpace(raw) != raw {
		return nil, errors.New("Idempotency-Key must be 1–128 characters without surrounding whitespace")
	}
	for _, char := range raw {
		if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || strings.ContainsRune("._:-", char)) {
			return nil, errors.New("Idempotency-Key contains unsupported characters")
		}
	}
	return &raw, nil
}

func hashTaskRequest(request TaskRequest) string {
	encoded, _ := json.Marshal(request)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func resolveTaskProviderName(requested string, defaultProvider Provider, config Config) (string, error) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return defaultProvider.Name(), nil
	}
	if router, ok := defaultProvider.(*providerRouter); ok {
		if _, exists := router.resolve(requested); exists {
			return requested, nil
		}
		return "", errors.New("provider is not configured on this server")
	}
	if requested == defaultProvider.Name() {
		return requested, nil
	}
	if requested == "mock" {
		return requested, nil
	}
	if requested == "openrouter" && config.OpenRouterAPIKey != "" {
		return requested, nil
	}
	return "", errors.New("provider is not configured on this server")
}

func validateTaskResponseFormat(raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	if len(raw) > 16<<10 {
		return errors.New("response_format must be 16 KB or smaller")
	}
	var value map[string]json.RawMessage
	if err := json.Unmarshal(raw, &value); err != nil || value == nil {
		return errors.New("response_format must be a JSON object")
	}
	var formatType string
	if err := json.Unmarshal(value["type"], &formatType); err != nil {
		return errors.New("response_format.type must be json_object or json_schema")
	}
	switch formatType {
	case "json_object":
		return nil
	case "json_schema":
		var schemaSpec struct {
			Name   string          `json:"name"`
			Schema json.RawMessage `json:"schema"`
		}
		if err := json.Unmarshal(value["json_schema"], &schemaSpec); err != nil || strings.TrimSpace(schemaSpec.Name) == "" || len(schemaSpec.Schema) == 0 || !json.Valid(schemaSpec.Schema) {
			return errors.New("response_format.json_schema requires a name and JSON schema")
		}
		return nil
	default:
		return errors.New("response_format.type must be json_object or json_schema")
	}
}

func listTasksHandler(store TaskStore) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal := principalFromContext(r.Context())
		status := r.URL.Query().Get("status")
		if status != "" && !isTaskStatus(status) {
			writeError(w, http.StatusBadRequest, "status must be queued, processing, completed, failed, or cancelled")
			return
		}
		limit := 20
		if raw := r.URL.Query().Get("limit"); raw != "" {
			parsed, err := strconv.Atoi(raw)
			if err != nil || parsed < 1 || parsed > 100 {
				writeError(w, http.StatusBadRequest, "limit must be between 1 and 100")
				return
			}
			limit = parsed
		}
		cursor, err := parseTaskCursor(r.URL.Query().Get("cursor"))
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		page, err := store.List(ctx, principal.WorkspaceID, status, limit, cursor)
		if err != nil {
			slog.Error("could not list task history", "workspace_id", principal.WorkspaceID, "error", err)
			writeError(w, http.StatusInternalServerError, "could not list task history")
			return
		}
		writeJSON(w, http.StatusOK, page)
	})
}

func isTaskStatus(status string) bool {
	switch status {
	case "queued", "processing", "completed", "failed", "cancelled":
		return true
	default:
		return false
	}
}

func getTaskHandler(store TaskStore) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal := principalFromContext(r.Context())
		if !isUUID(r.PathValue("id")) {
			writeError(w, http.StatusBadRequest, "invalid task ID")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		task, err := store.Get(ctx, principal.WorkspaceID, r.PathValue("id"))
		respondTaskError(w, err, "could not retrieve task")
		if err == nil {
			writeJSON(w, http.StatusOK, task)
		}
	})
}

func retryTaskHandler(store TaskStore, metrics *Metrics) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal := principalFromContext(r.Context())
		if !isUUID(r.PathValue("id")) {
			writeError(w, http.StatusBadRequest, "invalid task ID")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		task, err := store.Retry(ctx, principal.WorkspaceID, r.PathValue("id"))
		if errors.Is(err, ErrTaskNotRetryable) {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		if respondTaskError(w, err, "could not retry task") {
			return
		}
		metrics.TaskSubmitted(principal.WorkspaceID)
		metrics.TaskRetried(principal.WorkspaceID)
		w.Header().Set("Location", "/v1/tasks/"+task.ID)
		writeJSON(w, http.StatusAccepted, task)
	})
}

func cancelTaskHandler(store TaskStore, metrics *Metrics) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal := principalFromContext(r.Context())
		if !isUUID(r.PathValue("id")) {
			writeError(w, http.StatusBadRequest, "invalid task ID")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		task, err := store.RequestCancel(ctx, principal.WorkspaceID, r.PathValue("id"))
		if errors.Is(err, ErrTaskNotCancelable) {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		if respondTaskError(w, err, "could not cancel task") {
			return
		}
		if task.Status == "cancelled" {
			metrics.TaskCancelled(principal.WorkspaceID)
		}
		writeJSON(w, http.StatusOK, task)
	})
}

func deleteTaskHandler(store TaskStore) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal := principalFromContext(r.Context())
		if !isUUID(r.PathValue("id")) {
			writeError(w, http.StatusBadRequest, "invalid task ID")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		err := store.Delete(ctx, principal.WorkspaceID, r.PathValue("id"))
		if errors.Is(err, ErrTaskActive) {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		if respondTaskError(w, err, "could not delete task") {
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

func respondTaskError(w http.ResponseWriter, err error, fallback string) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrTaskNotFound) {
		writeError(w, http.StatusNotFound, ErrTaskNotFound.Error())
		return true
	}
	slog.Error(fallback, "error", err)
	writeError(w, http.StatusInternalServerError, fallback)
	return true
}

func listAPIKeysHandler(store CredentialStore) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal := principalFromContext(r.Context())
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		keys, err := store.ListAPIKeys(ctx, principal.WorkspaceID)
		if err != nil {
			slog.Error("could not list API keys", "workspace_id", principal.WorkspaceID, "error", err)
			writeError(w, http.StatusInternalServerError, "could not list API keys")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": keys})
	})
}

type createAPIKeyRequest struct {
	Name   string   `json:"name"`
	Scopes []string `json:"scopes"`
}

func createAPIKeyHandler(store CredentialStore) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal := principalFromContext(r.Context())
		var request createAPIKeyRequest
		if err := decodeJSONBody(w, r, 16<<10, &request); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		request.Name = strings.TrimSpace(request.Name)
		if request.Name == "" || len(request.Name) > 100 {
			writeError(w, http.StatusBadRequest, "name must be between 1 and 100 bytes")
			return
		}
		if len(request.Scopes) == 0 || len(request.Scopes) > len(allScopes)+1 {
			writeError(w, http.StatusBadRequest, "at least one valid scope is required")
			return
		}
		seen := make(map[string]struct{}, len(request.Scopes))
		for _, scope := range request.Scopes {
			if !isKnownScope(scope) || !principal.CanGrant(scope) {
				writeError(w, http.StatusForbidden, "API key cannot grant one or more requested scopes")
				return
			}
			if _, duplicate := seen[scope]; duplicate {
				writeError(w, http.StatusBadRequest, "scopes must be unique")
				return
			}
			seen[scope] = struct{}{}
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		record, token, err := store.CreateAPIKey(ctx, principal.WorkspaceID, request.Name, request.Scopes)
		if err != nil {
			slog.Error("could not create API key", "workspace_id", principal.WorkspaceID, "error", err)
			writeError(w, http.StatusInternalServerError, "could not create API key")
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"key": record, "token": token})
	})
}

func revokeAPIKeyHandler(store CredentialStore) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal := principalFromContext(r.Context())
		if !isUUID(r.PathValue("id")) {
			writeError(w, http.StatusBadRequest, "invalid API key ID")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if err := store.RevokeAPIKey(ctx, principal.WorkspaceID, r.PathValue("id"), principal); err != nil {
			switch {
			case errors.Is(err, ErrAPIKeyNotFound):
				writeError(w, http.StatusNotFound, err.Error())
			case errors.Is(err, ErrLastAdminKey):
				writeError(w, http.StatusConflict, err.Error())
			case errors.Is(err, ErrScopeEscalation):
				writeError(w, http.StatusForbidden, err.Error())
			default:
				slog.Error("could not revoke API key", "workspace_id", principal.WorkspaceID, "error", err)
				writeError(w, http.StatusInternalServerError, "could not revoke API key")
			}
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

type createWorkspaceRequest struct {
	Name    string `json:"name"`
	KeyName string `json:"key_name"`
}

func createWorkspaceHandler(store CredentialStore) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request createWorkspaceRequest
		if err := decodeJSONBody(w, r, 16<<10, &request); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		request.Name = strings.TrimSpace(request.Name)
		request.KeyName = strings.TrimSpace(request.KeyName)
		if request.Name == "" || len(request.Name) > 100 {
			writeError(w, http.StatusBadRequest, "workspace name must be between 1 and 100 bytes")
			return
		}
		if request.KeyName == "" {
			request.KeyName = request.Name + " admin"
		}
		if len(request.KeyName) > 100 {
			writeError(w, http.StatusBadRequest, "API key name must be 100 bytes or fewer")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		workspace, key, token, err := store.CreateWorkspaceWithAdminKey(ctx, request.Name, request.KeyName)
		if err != nil {
			slog.Error("could not create workspace", "error", err)
			writeError(w, http.StatusInternalServerError, "could not create workspace")
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"workspace": workspace, "key": key, "token": token})
	})
}

func isKnownScope(scope string) bool {
	if scope == ScopeAdmin {
		return true
	}
	for _, known := range allScopes {
		if scope == known {
			return true
		}
	}
	return false
}

func validateID(value string) error {
	if !isUUID(value) {
		return fmt.Errorf("invalid ID")
	}
	return nil
}
