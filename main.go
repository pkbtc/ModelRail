package main

import (
	"context"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
)

//go:embed frontend/dist
var frontendAssets embed.FS

type Config struct {
	Addr                 string
	DatabaseURL          string
	BootstrapAPIKey      string
	DatabaseTimeout      time.Duration
	DatabaseMaxConns     int32
	ShutdownTimeout      time.Duration
	WorkerCount          int
	RateLimitPerMin      int
	MaxInputBytes        int64
	Retention            time.Duration
	TaskTimeout          time.Duration
	LeaseDuration        time.Duration
	HeartbeatInterval    time.Duration
	OpenRouterAPIKey     string
	OpenRouterModel      string
	WebhookEncryptionKey []byte
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))
	if err := godotenv.Load(); err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Warn("could not load .env; using process environment", "error", err)
	}
	config, err := loadConfig()
	if err != nil {
		slog.Error("invalid configuration", "error", err)
		os.Exit(1)
	}

	startupCtx, startupCancel := context.WithTimeout(context.Background(), config.DatabaseTimeout)
	defer startupCancel()
	poolConfig, err := pgxpool.ParseConfig(config.DatabaseURL)
	if err != nil {
		slog.Error("could not configure database pool")
		os.Exit(1)
	}
	poolConfig.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeExec
	poolConfig.MaxConns = config.DatabaseMaxConns
	pool, err := pgxpool.NewWithConfig(startupCtx, poolConfig)
	if err != nil {
		slog.Error("could not create database pool", "error", err)
		os.Exit(1)
	}
	defer pool.Close()
	if err := pool.Ping(startupCtx); err != nil {
		slog.Error("could not connect to database", "host", poolConfig.ConnConfig.Host, "port", poolConfig.ConnConfig.Port, "database", poolConfig.ConnConfig.Database, "error", err)
		os.Exit(1)
	}
	if err := runMigrations(startupCtx, pool); err != nil {
		slog.Error("database migration failed", "error", err)
		os.Exit(1)
	}

	store := &PostgresTaskStore{pool: pool}
	if err := store.EnsureBootstrapKey(startupCtx, config.BootstrapAPIKey); err != nil {
		slog.Error("could not provision bootstrap API key", "error", err)
		os.Exit(1)
	}
	provider := buildProvider(config)
	if len(config.WebhookEncryptionKey) != 32 {
		slog.Info("signed webhooks are disabled; set WEBHOOK_ENCRYPTION_KEY to accept callback tasks")
	}
	metrics := NewMetrics()
	limiter := NewRateLimiter(config.RateLimitPerMin)

	rootCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var workers workerGroup
	workers.Start(rootCtx, store, provider, config, metrics)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthzHandler(provider))
	mux.Handle("GET /readyz", readinessHandler(pool))
	mux.Handle("GET /metrics", authenticate(store, limiter, ScopeMetricsRead, metrics.Handler(store)))
	mux.Handle("POST /v1/tasks", authenticate(store, limiter, ScopeTaskWrite, createTaskHandler(store, provider, config, metrics)))
	mux.Handle("GET /v1/tasks", authenticate(store, limiter, ScopeTaskRead, listTasksHandler(store)))
	mux.Handle("GET /v1/tasks/{id}", authenticate(store, limiter, ScopeTaskRead, getTaskHandler(store)))
	mux.Handle("POST /v1/tasks/{id}/retry", authenticate(store, limiter, ScopeTaskManage, retryTaskHandler(store, metrics)))
	mux.Handle("POST /v1/tasks/{id}/cancel", authenticate(store, limiter, ScopeTaskManage, cancelTaskHandler(store, metrics)))
	mux.Handle("DELETE /v1/tasks/{id}", authenticate(store, limiter, ScopeTaskDelete, deleteTaskHandler(store)))
	mux.Handle("GET /v1/api-keys", authenticate(store, limiter, ScopeKeysManage, listAPIKeysHandler(store)))
	mux.Handle("POST /v1/api-keys", authenticate(store, limiter, ScopeKeysManage, createAPIKeyHandler(store)))
	mux.Handle("DELETE /v1/api-keys/{id}", authenticate(store, limiter, ScopeKeysManage, revokeAPIKeyHandler(store)))
	mux.Handle("POST /v1/workspaces", authenticate(store, limiter, ScopeAdmin, createWorkspaceHandler(store)))

	staticFiles, err := fs.Sub(frontendAssets, "frontend/dist")
	if err != nil {
		slog.Error("could not load embedded frontend", "error", err)
		os.Exit(1)
	}
	mux.Handle("/", http.FileServer(http.FS(staticFiles)))

	server := &http.Server{
		Addr:              config.Addr,
		Handler:           requestLogger(mux),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	serverErrors := make(chan error, 1)
	go func() {
		slog.Info("server started", "addr", config.Addr, "workers", config.WorkerCount)
		serverErrors <- server.ListenAndServe()
	}()

	select {
	case <-rootCtx.Done():
		slog.Info("shutdown requested")
	case err := <-serverErrors:
		if !errors.Is(err, http.ErrServerClosed) {
			slog.Error("HTTP server stopped unexpectedly", "error", err)
		}
		stop()
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), config.ShutdownTimeout)
	defer shutdownCancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		slog.Warn("graceful HTTP shutdown timed out", "error", err)
		_ = server.Close()
	}
	stop()
	workers.Wait(shutdownCtx)
	retentionCtx, retentionCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer retentionCancel()
	if count, err := store.DeleteExpired(retentionCtx, config.Retention); err != nil {
		slog.Warn("final retention cleanup failed", "error", err)
	} else if count > 0 {
		slog.Info("deleted expired tasks", "count", count)
	}
}

func loadConfig() (Config, error) {
	bootstrapKey := strings.TrimSpace(os.Getenv("MODELRAIL_BOOTSTRAP_API_KEY"))
	if bootstrapKey == "" {
		bootstrapKey = strings.TrimSpace(os.Getenv("MODELRAIL_API_KEY"))
	}
	placeholder := strings.ToLower(bootstrapKey)
	if len(bootstrapKey) < 32 || strings.Contains(placeholder, "replace-with") || strings.Contains(placeholder, "change-this") {
		return Config{}, errors.New("MODELRAIL_BOOTSTRAP_API_KEY must contain at least 32 characters")
	}
	databaseURL := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if databaseURL == "" {
		return Config{}, errors.New("DATABASE_URL is required")
	}
	webhookEncryptionKey := []byte(nil)
	if raw := strings.TrimSpace(os.Getenv("WEBHOOK_ENCRYPTION_KEY")); raw != "" {
		decoded, err := hex.DecodeString(raw)
		if err != nil || len(decoded) != 32 {
			return Config{}, errors.New("WEBHOOK_ENCRYPTION_KEY must be 64 hexadecimal characters (32 bytes)")
		}
		webhookEncryptionKey = decoded
	}
	connectTimeout, err := durationEnv("DATABASE_CONNECT_TIMEOUT", 30*time.Second)
	if err != nil {
		return Config{}, err
	}
	databaseMaxConns, err := intEnv("DATABASE_MAX_CONNS", 10, 1, 100)
	if err != nil {
		return Config{}, err
	}
	shutdownTimeout, err := durationEnv("SHUTDOWN_TIMEOUT", 20*time.Second)
	if err != nil {
		return Config{}, err
	}
	workers, err := intEnv("WORKER_COUNT", 2, 1, 64)
	if err != nil {
		return Config{}, err
	}
	rateLimit, err := intEnv("RATE_LIMIT_PER_MINUTE", 120, 1, 100000)
	if err != nil {
		return Config{}, err
	}
	maxInput, err := int64Env("TASK_MAX_INPUT_BYTES", 1<<20, 1, 16<<20)
	if err != nil {
		return Config{}, err
	}
	retentionDays, err := intEnv("TASK_RETENTION_DAYS", 30, 1, 3650)
	if err != nil {
		return Config{}, err
	}
	taskTimeout, err := durationEnv("TASK_TIMEOUT", 2*time.Minute)
	if err != nil || taskTimeout > 24*time.Hour {
		return Config{}, errors.New("TASK_TIMEOUT must be a positive duration no greater than 24h")
	}
	leaseSeconds, err := intEnv("WORKER_LEASE_SECONDS", 45, 15, 3600)
	if err != nil {
		return Config{}, err
	}
	heartbeatSeconds, err := intEnv("WORKER_HEARTBEAT_SECONDS", 10, 1, 900)
	if err != nil {
		return Config{}, err
	}
	leaseDuration := time.Duration(leaseSeconds) * time.Second
	heartbeatInterval := time.Duration(heartbeatSeconds) * time.Second
	if heartbeatInterval*2 >= leaseDuration {
		return Config{}, errors.New("WORKER_HEARTBEAT_SECONDS must be less than half WORKER_LEASE_SECONDS")
	}
	addr := strings.TrimSpace(os.Getenv("MODELRAIL_ADDR"))
	if addr == "" {
		addr = ":8080"
	}
	return Config{
		Addr: addr, DatabaseURL: databaseURL, BootstrapAPIKey: bootstrapKey,
		DatabaseTimeout: connectTimeout, DatabaseMaxConns: int32(databaseMaxConns), ShutdownTimeout: shutdownTimeout,
		WorkerCount: workers, RateLimitPerMin: rateLimit, MaxInputBytes: maxInput,
		Retention:     time.Duration(retentionDays) * 24 * time.Hour,
		TaskTimeout:   taskTimeout,
		LeaseDuration: leaseDuration, HeartbeatInterval: heartbeatInterval,
		OpenRouterAPIKey:     strings.TrimSpace(os.Getenv("OPENROUTER_API_KEY")),
		OpenRouterModel:      strings.TrimSpace(os.Getenv("OPENROUTER_MODEL")),
		WebhookEncryptionKey: webhookEncryptionKey,
	}, nil
}

func durationEnv(name string, fallback time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := time.ParseDuration(raw)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration", name)
	}
	return value, nil
}

func intEnv(name string, fallback, minimum, maximum int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < minimum || value > maximum {
		return 0, fmt.Errorf("%s must be between %d and %d", name, minimum, maximum)
	}
	return value, nil
}

func int64Env(name string, fallback, minimum, maximum int64) (int64, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < minimum || value > maximum {
		return 0, fmt.Errorf("%s must be between %d and %d", name, minimum, maximum)
	}
	return value, nil
}

func healthzHandler(provider Provider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"service": "modelrail", "status": "ok", "provider": provider.Name()})
	}
}

func readinessHandler(pool *pgxpool.Pool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := pool.Ping(ctx); err != nil {
			writeError(w, http.StatusServiceUnavailable, "database is unavailable")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"service": "modelrail", "status": "ready"})
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (w *statusRecorder) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; base-uri 'self'; form-action 'self'; frame-ancestors 'none'")
		started := time.Now()
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(recorder, r)
		slog.Info("http request", "method", r.Method, "path", r.URL.Path, "status", recorder.status, "duration_ms", time.Since(started).Milliseconds())
	})
}
