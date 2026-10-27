package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
)

// TaskRequest represents the JSON data sent by the client.
type TaskRequest struct {
	Task  string `json:"task"`
	Input string `json:"input"`
}

// TaskAcceptedResponse is returned when a task has been queued for processing.
type TaskAcceptedResponse struct {
	ID        string `json:"id"`
	Provider  string `json:"provider"`
	Status    string `json:"status"`
	StatusURL string `json:"status_url"`
}

// Provider describes what any ModelRail provider must implement.
type Provider interface {
	Name() string
	Process(task string, input string) (string, error)
}

// MockProvider simulates processing without using an AI API.
type MockProvider struct{}

func (MockProvider) Name() string {
	return "mock"
}

func (MockProvider) Process(task string, input string) (string, error) {
	result := fmt.Sprintf(
		"Mock provider received task %q. Input length: %d bytes. No AI model was called.",
		task,
		len(input),
	)

	return result, nil
}

func main() {
	// Load configuration from the local .env file.
	if err := godotenv.Load(); err != nil {
		log.Println("No .env file found; checking system environment")
	}

	// Read the database connection string.
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		log.Fatal("DATABASE_URL is missing from the environment")
	}

	// Allow enough time for a serverless database to wake up on first use.
	connectTimeout := 30 * time.Second
	if rawTimeout := strings.TrimSpace(os.Getenv("DATABASE_CONNECT_TIMEOUT")); rawTimeout != "" {
		parsedTimeout, err := time.ParseDuration(rawTimeout)
		if err != nil || parsedTimeout <= 0 {
			log.Fatalf("DATABASE_CONNECT_TIMEOUT must be a positive duration (for example, 30s): %q", rawTimeout)
		}
		connectTimeout = parsedTimeout
	}

	ctx, cancel := context.WithTimeout(
		context.Background(),
		connectTimeout,
	)
	defer cancel()

	// Parse the URL separately so connection failures can report the target
	// without ever logging the username, password, or full connection string.
	poolConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		log.Fatalf("Could not configure database pool: %v", err)
	}

	// Disable prepared statement cache for compatibility with Supabase transaction pooler (PgBouncer).
	poolConfig.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeExec

	// Create a reusable PostgreSQL connection pool.
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		log.Fatalf("Could not configure database pool: %v", err)
	}
	defer pool.Close()

	// Actually verify that Neon is reachable and accepts the connection.
	if err := pool.Ping(ctx); err != nil {
		log.Fatalf(
			"Could not connect to PostgreSQL at %s:%d/%s within %s: %v. Check that the database endpoint is active and outbound TCP port %d is reachable",
			poolConfig.ConnConfig.Host,
			poolConfig.ConnConfig.Port,
			poolConfig.ConnConfig.Database,
			connectTimeout,
			err,
			poolConfig.ConnConfig.Port,
		)
	}

	log.Println("Connected to Supabase PostgreSQL")

	provider := MockProvider{}
	store := &PostgresTaskStore{pool: pool}

	startWorkers(context.Background(), store, provider, 2)

	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", healthzHandler)
	mux.HandleFunc("POST /v1/tasks", taskHandler(provider, store))
	mux.HandleFunc("GET /v1/tasks/{id}", getTaskHandler(store))

	server := &http.Server{
		Addr:              ":8080",
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.Println("ModelRail server starting on http://localhost:8080")

	if err := server.ListenAndServe(); err != nil &&
		!errors.Is(err, http.ErrServerClosed) {
		log.Printf("HTTP server failed: %v", err)
	}
}

// healthzHandler checks whether the HTTP server is responding.
func healthzHandler(w http.ResponseWriter, r *http.Request) {
	response := map[string]string{
		"service": "modelrail",
		"status":  "ok",
	}

	writeJSON(w, http.StatusOK, response)
}

// taskHandler validates the request, queues the task, and returns 202 Accepted.
func taskHandler(
	provider Provider,
	store TaskStore,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)

		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()

		var request TaskRequest

		if err := decoder.Decode(&request); err != nil {
			writeError(
				w,
				http.StatusBadRequest,
				"invalid JSON body or unknown field",
			)
			return
		}

		if strings.TrimSpace(request.Task) == "" {
			writeError(w, http.StatusBadRequest, "task is required")
			return
		}

		if strings.TrimSpace(request.Input) == "" {
			writeError(w, http.StatusBadRequest, "input is required")
			return
		}

		createCtx, cancel := context.WithTimeout(
			r.Context(),
			5*time.Second,
		)

		taskID, err := store.Create(
			createCtx,
			request.Task,
			request.Input,
			provider.Name(),
		)

		cancel()

		if err != nil {
			log.Printf("Failed to queue task: %v", err)
			writeError(
				w,
				http.StatusInternalServerError,
				"could not queue task",
			)
			return
		}

		response := TaskAcceptedResponse{
			ID:        taskID,
			Provider:  provider.Name(),
			Status:    "queued",
			StatusURL: "/v1/tasks/" + taskID,
		}

		w.Header().Set("Location", response.StatusURL)
		writeJSON(w, http.StatusAccepted, response)
	}
}

// writeJSON sends a JSON response with the specified HTTP status.
func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	if err := json.NewEncoder(w).Encode(data); err != nil {
		log.Printf("Failed to write JSON response: %v", err)
	}
}

// writeError returns an error message as a JSON object.
func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{
		"error": message,
	})
}

// getTaskHandler retrieves a saved task by its UUID.
func getTaskHandler(store TaskStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")

		if !isUUID(id) {
			writeError(w, http.StatusBadRequest, "invalid task ID")
			return
		}

		ctx, cancel := context.WithTimeout(
			r.Context(),
			5*time.Second,
		)
		defer cancel()

		task, err := store.Get(ctx, id)

		if errors.Is(err, ErrTaskNotFound) {
			writeError(w, http.StatusNotFound, "task not found")
			return
		}

		if err != nil {
			log.Printf("Failed to retrieve task %s: %v", id, err)
			writeError(
				w,
				http.StatusInternalServerError,
				"could not retrieve task",
			)
			return
		}

		writeJSON(w, http.StatusOK, task)
	}
}

// isUUID validates that s is a canonical 8-4-4-4-12 UUID.
func isUUID(value string) bool {
	if len(value) != 36 {
		return false
	}

	for _, position := range []int{8, 13, 18, 23} {
		if value[position] != '-' {
			return false
		}
	}

	compact := strings.ReplaceAll(value, "-", "")

	_, err := hex.DecodeString(compact)
	return err == nil
}

// startWorkers launches count worker goroutines that process queued tasks.
func startWorkers(
	ctx context.Context,
	store TaskStore,
	provider Provider,
	count int,
) {
	for workerID := 1; workerID <= count; workerID++ {
		go runWorker(ctx, workerID, store, provider)
	}
}

// runWorker continuously claims and processes queued tasks and periodically recovers stale tasks.
func runWorker(
	ctx context.Context,
	workerID int,
	store TaskStore,
	provider Provider,
) {
	pollTicker := time.NewTicker(1 * time.Second)
	recoveryTicker := time.NewTicker(30 * time.Second)

	defer pollTicker.Stop()
	defer recoveryTicker.Stop()

	recoverStale := func() {
		recoveryCtx, cancel := context.WithTimeout(
			ctx,
			5*time.Second,
		)
		defer cancel()

		if err := store.RecoverStale(recoveryCtx); err != nil {
			log.Printf(
				"Worker %d could not recover stale tasks: %v",
				workerID,
				err,
			)
		}
	}

	wait := func() bool {
		select {
		case <-ctx.Done():
			return false
		case <-pollTicker.C:
			return true
		case <-recoveryTicker.C:
			recoverStale()
			return true
		}
	}

	// Run recovery at startup.
	recoverStale()

	for {
		select {
		case <-ctx.Done():
			return
		case <-recoveryTicker.C:
			recoverStale()
		default:
		}

		claimCtx, cancel := context.WithTimeout(
			ctx,
			5*time.Second,
		)
		task, err := store.ClaimNext(claimCtx)
		cancel()

		if errors.Is(err, ErrNoQueuedTask) {
			if !wait() {
				return
			}
			continue
		}

		if err != nil {
			log.Printf(
				"Worker %d could not claim a task: %v",
				workerID,
				err,
			)

			if !wait() {
				return
			}
			continue
		}

		log.Printf(
			"Worker %d processing task %s (attempt %d/%d)",
			workerID,
			task.ID,
			task.AttemptCount,
			task.MaxAttempts,
		)

		if err := processTask(ctx, store, provider, task); err != nil {
			log.Printf(
				"Worker %d processing attempt for task %s failed: %v",
				workerID,
				task.ID,
				err,
			)
		}
	}
}

// processTask runs the provider and persists the result or failure.
func processTask(
	ctx context.Context,
	store TaskStore,
	provider Provider,
	task TaskRecord,
) error {
	result, err := provider.Process(task.Task, task.Input)

	if err != nil {
		// Try to record the failure even if the request context ended.
		failCtx, cancel := context.WithTimeout(
			context.Background(),
			5*time.Second,
		)

		saveErr := store.Fail(failCtx, task.ID, err.Error())
		cancel()

		if saveErr != nil {
			return fmt.Errorf(
				"provider failed: %v; could not save failure: %w",
				err,
				saveErr,
			)
		}

		return err
	}

	completeCtx, cancel := context.WithTimeout(
		ctx,
		5*time.Second,
	)
	defer cancel()

	if err := store.Complete(completeCtx, task.ID, result); err != nil {
		return fmt.Errorf(
			"could not save result for task %s: %w",
			task.ID,
			err,
		)
	}

	log.Printf("Task %s completed successfully", task.ID)
	return nil
}
