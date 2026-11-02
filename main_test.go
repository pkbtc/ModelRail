package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type fakeTaskStore struct {
	TaskStore
	createdTask     TaskRecord
	created         bool
	failRetryable   bool
	failStatus      string
	failMessage     string
	completed       bool
	statusFilter    string
	listCursor      *TaskCursor
	listWorkspace   string
	requestHash     string
	idempotencyKey  *string
	lastLeaseToken  string
	cancelFinalized bool
}

func (s *fakeTaskStore) Create(_ context.Context, workspaceID string, task TaskRecord, key *string, requestHash string) (TaskRecord, bool, error) {
	s.created = true
	s.requestHash = requestHash
	s.idempotencyKey = key
	task.ID = "11111111-1111-4111-8111-111111111111"
	task.WorkspaceID = workspaceID
	task.Status = "queued"
	task.CreatedAt = time.Now()
	s.createdTask = task
	return s.createdTask, true, nil
}

func (s *fakeTaskStore) Complete(_ context.Context, _, leaseToken, _ string) (string, error) {
	s.completed = true
	s.lastLeaseToken = leaseToken
	return "completed", nil
}

func (s *fakeTaskStore) Fail(_ context.Context, _, leaseToken, message string, retryable bool) (string, error) {
	s.failRetryable = retryable
	s.failMessage = message
	s.lastLeaseToken = leaseToken
	s.failStatus = "failed"
	if retryable {
		s.failStatus = "queued"
	}
	return s.failStatus, nil
}

func (s *fakeTaskStore) Heartbeat(context.Context, string, string, time.Duration) (bool, bool, error) {
	return true, false, nil
}

func (s *fakeTaskStore) Get(_ context.Context, workspaceID, id string) (TaskRecord, error) {
	if id != s.createdTask.ID || workspaceID != s.createdTask.WorkspaceID {
		return TaskRecord{}, ErrTaskNotFound
	}
	return s.createdTask, nil
}

func (s *fakeTaskStore) List(_ context.Context, workspaceID, status string, _ int, cursor *TaskCursor) (TaskPage, error) {
	s.listWorkspace, s.statusFilter, s.listCursor = workspaceID, status, cursor
	return TaskPage{Items: []TaskRecord{s.createdTask}}, nil
}

func (s *fakeTaskStore) Retry(context.Context, string, string) (TaskRecord, error) {
	return TaskRecord{}, ErrTaskNotRetryable
}

func (s *fakeTaskStore) RequestCancel(context.Context, string, string) (TaskRecord, error) {
	return s.createdTask, nil
}

func (s *fakeTaskStore) FinalizeCancelled(context.Context, string, string) error {
	s.cancelFinalized = true
	return nil
}

type failingProvider struct{}

func (failingProvider) Name() string { return "failing" }
func (failingProvider) Process(context.Context, TaskRecord) (string, error) {
	return "", errors.New("simulated provider failure")
}

type waitingProvider struct{}

func (waitingProvider) Name() string { return "waiting" }
func (waitingProvider) Process(ctx context.Context, _ TaskRecord) (string, error) {
	<-ctx.Done()
	return "", ctx.Err()
}

type fakeCredentialStore struct {
	CredentialStore
	principal Principal
	token     string
}

func (s fakeCredentialStore) Authenticate(_ context.Context, token string) (Principal, error) {
	if token != s.token {
		return Principal{}, ErrInvalidAPIKey
	}
	return s.principal, nil
}

func TestCreateTaskHandler(t *testing.T) {
	store := &fakeTaskStore{}
	metrics := NewMetrics()
	handler := createTaskHandler(store, MockProvider{}, Config{MaxInputBytes: 1024}, metrics)
	request := httptest.NewRequest(http.MethodPost, "/v1/tasks", strings.NewReader(`{"task":"summarize","input":"hello"}`))
	request.Header.Set("Idempotency-Key", "request-1")
	request = request.WithContext(context.WithValue(request.Context(), principalContextKey{}, Principal{WorkspaceID: defaultWorkspaceID}))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("expected status 202, got %d: %s", recorder.Code, recorder.Body.String())
	}
	var response TaskAcceptedResponse
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.Status != "queued" || response.Provider != "mock" || !store.created {
		t.Fatalf("unexpected response or task not stored: %+v", response)
	}
	if store.idempotencyKey == nil || *store.idempotencyKey != "request-1" || store.requestHash == "" {
		t.Fatal("idempotency key and payload fingerprint were not stored")
	}
}

func TestCreateTaskRejectsMalformedAndOversizedRequests(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		max        int64
		wantStatus int
	}{
		{name: "unknown field", body: `{"task":"sum","input":"hello","admin":true}`, max: 1024, wantStatus: http.StatusBadRequest},
		{name: "oversized input", body: `{"task":"sum","input":"this payload is too long"}`, max: 4, wantStatus: http.StatusRequestEntityTooLarge},
		{name: "empty task", body: `{"task":" ","input":"hello"}`, max: 1024, wantStatus: http.StatusBadRequest},
		{name: "trailing JSON", body: `{"task":"sum","input":"hello"}{}`, max: 1024, wantStatus: http.StatusBadRequest},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &fakeTaskStore{}
			handler := createTaskHandler(store, MockProvider{}, Config{MaxInputBytes: test.max}, NewMetrics())
			request := httptest.NewRequest(http.MethodPost, "/v1/tasks", strings.NewReader(test.body))
			request = request.WithContext(context.WithValue(request.Context(), principalContextKey{}, Principal{WorkspaceID: defaultWorkspaceID}))
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			if recorder.Code != test.wantStatus {
				t.Fatalf("expected status %d, got %d: %s", test.wantStatus, recorder.Code, recorder.Body.String())
			}
			if store.created {
				t.Fatal("invalid request was persisted")
			}
		})
	}
}

func TestListTasksValidatesAndAppliesStatusFilter(t *testing.T) {
	store := &fakeTaskStore{createdTask: TaskRecord{ID: "11111111-1111-4111-8111-111111111111", WorkspaceID: defaultWorkspaceID}}
	handler := listTasksHandler(store)
	request := httptest.NewRequest(http.MethodGet, "/v1/tasks?status=failed&limit=10", nil)
	request = request.WithContext(context.WithValue(request.Context(), principalContextKey{}, Principal{WorkspaceID: defaultWorkspaceID}))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || store.statusFilter != "failed" || store.listWorkspace != defaultWorkspaceID {
		t.Fatalf("status filter or workspace was not applied: status=%d filter=%q workspace=%q", recorder.Code, store.statusFilter, store.listWorkspace)
	}
}

func TestAuthenticateScopesAndRateLimit(t *testing.T) {
	const token = "0123456789abcdef0123456789abcdef"
	store := fakeCredentialStore{token: token, principal: Principal{KeyID: "key-id", WorkspaceID: defaultWorkspaceID, Scopes: map[string]struct{}{ScopeTaskRead: {}}}}
	limiter := NewRateLimiter(1)
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	readHandler := authenticate(store, limiter, ScopeTaskRead, next)
	request := httptest.NewRequest(http.MethodGet, "/v1/tasks", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	recorder := httptest.NewRecorder()
	readHandler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("expected authenticated request, got %d", recorder.Code)
	}
	request = httptest.NewRequest(http.MethodGet, "/v1/tasks", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	recorder = httptest.NewRecorder()
	readHandler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusTooManyRequests || recorder.Header().Get("Retry-After") == "" {
		t.Fatalf("expected rate limited response with Retry-After, got %d", recorder.Code)
	}
	writeHandler := authenticate(store, NewRateLimiter(10), ScopeTaskWrite, next)
	request = httptest.NewRequest(http.MethodPost, "/v1/tasks", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	recorder = httptest.NewRecorder()
	writeHandler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("expected insufficient-scope 403, got %d", recorder.Code)
	}
}

func TestProcessTaskProviderFailureIsRetryableByDefault(t *testing.T) {
	store := &fakeTaskStore{}
	task := TaskRecord{ID: "11111111-1111-4111-8111-111111111111", LeaseToken: "22222222-2222-4222-8222-222222222222", Task: "summarize", Input: "hello"}
	config := Config{TaskTimeout: 2 * time.Minute, LeaseDuration: 45 * time.Second, HeartbeatInterval: 10 * time.Second}
	if err := processTask(context.Background(), 1, store, failingProvider{}, task, config, NewMetrics()); err == nil {
		t.Fatal("expected provider error")
	}
	if !store.failRetryable || store.completed || store.lastLeaseToken != task.LeaseToken {
		t.Fatal("retryable failure was not persisted against the worker lease")
	}
}

func TestProcessTaskTimeoutIsRetryableAndDoesNotStoreProviderError(t *testing.T) {
	store := &fakeTaskStore{}
	task := TaskRecord{ID: "11111111-1111-4111-8111-111111111111", LeaseToken: "22222222-2222-4222-8222-222222222222", Task: "summarize", Input: "private task input"}
	config := Config{TaskTimeout: 30 * time.Millisecond, LeaseDuration: time.Second, HeartbeatInterval: 5 * time.Millisecond}
	if err := processTask(context.Background(), 1, store, waitingProvider{}, task, config, NewMetrics()); err != nil {
		t.Fatalf("timeout should be persisted as a retryable task failure, got %v", err)
	}
	if !store.failRetryable || store.completed || store.failMessage != "task exceeded the configured runtime limit" {
		t.Fatalf("unexpected timeout handling: retryable=%v completed=%v message=%q", store.failRetryable, store.completed, store.failMessage)
	}
}

func TestTaskRetriedMetricCountsManualRetries(t *testing.T) {
	metrics := NewMetrics()
	metrics.TaskRetried(defaultWorkspaceID)
	metrics.mu.Lock()
	got := metrics.workspaces[defaultWorkspaceID].retried
	metrics.mu.Unlock()
	if got != 1 {
		t.Fatalf("manual retry counter = %d; want 1", got)
	}
}

func TestLoadConfigTaskTimeout(t *testing.T) {
	t.Setenv("MODELRAIL_BOOTSTRAP_API_KEY", "unit-test-bootstrap-key-0123456789abcdef")
	t.Setenv("MODELRAIL_API_KEY", "")
	t.Setenv("DATABASE_URL", "postgres://modelrail:local@localhost/modelrail?sslmode=disable")
	t.Setenv("MODELRAIL_ADDR", "")
	t.Setenv("DATABASE_CONNECT_TIMEOUT", "")
	t.Setenv("DATABASE_MAX_CONNS", "")
	t.Setenv("SHUTDOWN_TIMEOUT", "")
	t.Setenv("WORKER_COUNT", "")
	t.Setenv("RATE_LIMIT_PER_MINUTE", "")
	t.Setenv("TASK_MAX_INPUT_BYTES", "")
	t.Setenv("TASK_RETENTION_DAYS", "")
	t.Setenv("WORKER_LEASE_SECONDS", "")
	t.Setenv("WORKER_HEARTBEAT_SECONDS", "")
	t.Setenv("TASK_TIMEOUT", "")
	defaultConfig, err := loadConfig()
	if err != nil || defaultConfig.TaskTimeout != 2*time.Minute {
		t.Fatalf("load default timeout: timeout=%s err=%v", defaultConfig.TaskTimeout, err)
	}
	if defaultConfig.DatabaseMaxConns != 10 {
		t.Fatalf("default database pool size = %d; want 10", defaultConfig.DatabaseMaxConns)
	}
	t.Setenv("DATABASE_MAX_CONNS", "5")
	configuredPool, err := loadConfig()
	if err != nil || configuredPool.DatabaseMaxConns != 5 {
		t.Fatalf("load configured database pool size: max=%d err=%v", configuredPool.DatabaseMaxConns, err)
	}
	t.Setenv("DATABASE_MAX_CONNS", "")
	t.Setenv("TASK_TIMEOUT", "5m")
	config, err := loadConfig()
	if err != nil || config.TaskTimeout != 5*time.Minute {
		t.Fatalf("load configured timeout: timeout=%s err=%v", config.TaskTimeout, err)
	}
	t.Setenv("TASK_TIMEOUT", "25h")
	if _, err := loadConfig(); err == nil {
		t.Fatal("expected timeout greater than 24 hours to be rejected")
	}
}

func TestOpenRouterRetryableStatusClassification(t *testing.T) {
	for _, test := range []struct {
		status int
		want   bool
	}{{http.StatusUnauthorized, false}, {http.StatusBadRequest, false}, {http.StatusPaymentRequired, false}, {http.StatusRequestTimeout, true}, {http.StatusTooManyRequests, true}, {http.StatusBadGateway, true}} {
		if got := isRetryableOpenRouterStatus(test.status); got != test.want {
			t.Errorf("status %d retryable = %v; want %v", test.status, got, test.want)
		}
	}
}

func TestOpenRouterProviderRequestAndResponse(t *testing.T) {
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/chat/completions" {
			t.Errorf("unexpected OpenRouter request: %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-openrouter-secret" {
			t.Errorf("unexpected Authorization header")
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("unexpected Content-Type: %q", r.Header.Get("Content-Type"))
		}
		var request orRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode OpenRouter request: %v", err)
		}
		if request.Model != "test/model" || len(request.Messages) != 2 || request.Messages[0].Role != "system" || request.Messages[1].Role != "user" || request.Messages[1].Content != "private input" {
			t.Errorf("unexpected OpenRouter payload: %+v", request)
		}
		return mockJSONResponse(r, http.StatusOK, `{"choices":[{"message":{"content":"processed output"}}]}`), nil
	})

	provider := NewOpenRouterProvider("test-openrouter-secret", "test/model")
	provider.endpoint = "https://openrouter.test/api/v1/chat/completions"
	provider.client = &http.Client{Transport: transport}
	result, err := provider.Process(context.Background(), TaskRecord{Task: "summarize", Input: "private input"})
	if err != nil || result != "processed output" {
		t.Fatalf("OpenRouter response = %q, err = %v", result, err)
	}
}

func TestOpenRouterErrorsDoNotPersistResponseBody(t *testing.T) {
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return mockJSONResponse(r, http.StatusTooManyRequests, `{"error":"private prompt fragment"}`), nil
	})

	provider := NewOpenRouterProvider("test-openrouter-secret", "test/model")
	provider.endpoint = "https://openrouter.test/api/v1/chat/completions"
	provider.client = &http.Client{Transport: transport}
	_, err := provider.Process(context.Background(), TaskRecord{Task: "summarize", Input: "private input"})
	if err == nil || strings.Contains(err.Error(), "private prompt fragment") || strings.Contains(err.Error(), "private input") {
		t.Fatalf("provider error leaked response or task content: %v", err)
	}
	var retryable RetryableProviderError
	if !errors.As(err, &retryable) || !retryable.Retryable() {
		t.Fatalf("expected a retryable rate limit error, got %v", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func mockJSONResponse(request *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    request,
	}
}
