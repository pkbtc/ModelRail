package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeTaskStore implements TaskStore without touching a real database.
type fakeTaskStore struct {
	created   bool
	completed bool
	failed    bool
}

func (s *fakeTaskStore) Create(
	_ context.Context,
	_ string,
	_ string,
	_ string,
) (string, error) {
	s.created = true
	return "test-task-id", nil
}

func (s *fakeTaskStore) Complete(
	_ context.Context,
	_ string,
	_ string,
) error {
	s.completed = true
	return nil
}

func (s *fakeTaskStore) Fail(
	_ context.Context,
	_ string,
	_ string,
) error {
	s.failed = true
	return nil
}

func (s *fakeTaskStore) Get(
	_ context.Context,
	id string,
) (TaskRecord, error) {
	result := "Fake test result"

	return TaskRecord{
		ID:           id,
		Task:         "summarize",
		Input:        "hello",
		Provider:     "mock",
		Status:       "completed",
		AttemptCount: 1,
		MaxAttempts:  3,
		Result:       &result,
	}, nil
}

func (s *fakeTaskStore) ClaimNext(
	_ context.Context,
) (TaskRecord, error) {
	return TaskRecord{}, ErrNoQueuedTask
}

func (s *fakeTaskStore) RecoverStale(
	_ context.Context,
) error {
	return nil
}

// Test a valid task request.
func TestTaskHandlerSuccess(t *testing.T) {
	store := &fakeTaskStore{}
	handler := taskHandler(MockProvider{}, store)

	body := `{"task":"summarize","input":"hello"}`

	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/tasks",
		strings.NewReader(body),
	)

	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected status 202, got %d", rec.Code)
	}

	var response TaskAcceptedResponse

	if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if response.ID != "test-task-id" {
		t.Errorf("expected test-task-id, got %q", response.ID)
	}

	if response.Provider != "mock" {
		t.Errorf("expected provider mock, got %q", response.Provider)
	}

	if response.Status != "queued" {
		t.Errorf("expected status queued, got %q", response.Status)
	}

	if response.StatusURL != "/v1/tasks/test-task-id" {
		t.Errorf("unexpected status URL: %q", response.StatusURL)
	}

	if !store.created {
		t.Error("expected the task to be saved")
	}

	if store.completed {
		t.Error("HTTP handler should not complete the task itself")
	}
}

// Test invalid requests.
func TestTaskHandlerBadRequests(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		wantError string
	}{
		{
			name:      "invalid JSON",
			body:      `{"task":`,
			wantError: "invalid JSON body or unknown field",
		},
		{
			name:      "missing task",
			body:      `{"task":"","input":"hello"}`,
			wantError: "task is required",
		},
		{
			name:      "missing input",
			body:      `{"task":"summarize","input":" "}`,
			wantError: "input is required",
		},
		{
			name:      "unknown field",
			body:      `{"task":"summarize","input":"hello","extra":true}`,
			wantError: "invalid JSON body or unknown field",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeTaskStore{}
			handler := taskHandler(MockProvider{}, store)

			req := httptest.NewRequest(
				http.MethodPost,
				"/v1/tasks",
				strings.NewReader(tc.body),
			)

			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf(
					"expected status 400, got %d",
					rec.Code,
				)
			}

			var response map[string]string

			if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
				t.Fatalf("failed to decode error response: %v", err)
			}

			if response["error"] != tc.wantError {
				t.Errorf(
					"expected error %q, got %q",
					tc.wantError,
					response["error"],
				)
			}

			if store.created {
				t.Error("invalid request should not create a task")
			}
		})
	}
}

// A provider that simulates a failure.
type failingProvider struct{}

func (failingProvider) Name() string {
	return "failing"
}

func (failingProvider) Process(task string, input string) (string, error) {
	return "", errors.New("simulated provider failure")
}

// TestProcessTaskProviderFailure tests that a provider failure is correctly
// recorded by the worker's processTask function.
func TestProcessTaskProviderFailure(t *testing.T) {
	store := &fakeTaskStore{}

	task := TaskRecord{
		ID:    "test-task-id",
		Task:  "summarize",
		Input: "hello",
	}

	err := processTask(
		context.Background(),
		store,
		failingProvider{},
		task,
	)

	if err == nil {
		t.Fatal("expected provider error")
	}

	if !store.failed {
		t.Error("expected task failure to be recorded")
	}

	if store.completed {
		t.Error("failed task must not be marked completed")
	}
}
