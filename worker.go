package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

type Provider interface {
	Name() string
	Process(ctx context.Context, task TaskRecord) (string, error)
}

type RetryableProviderError interface {
	error
	Retryable() bool
}

type MockProvider struct{}

func (MockProvider) Name() string { return "mock" }

func (MockProvider) Process(ctx context.Context, task TaskRecord) (string, error) {
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	default:
	}
	return fmt.Sprintf("Mock provider received task %q. Input length: %d bytes. No AI model was called.", task.Task, len(task.Input)), nil
}

type providerRouter struct {
	defaultName string
	providers   map[string]Provider
}

func (r *providerRouter) Name() string { return r.defaultName }

func (r *providerRouter) resolve(name string) (Provider, bool) {
	provider, ok := r.providers[name]
	return provider, ok
}

func (r *providerRouter) Process(ctx context.Context, task TaskRecord) (string, error) {
	name := task.Provider
	if name == "" {
		name = r.defaultName
	}
	provider, ok := r.resolve(name)
	if !ok {
		return "", &openRouterError{message: "task provider is not configured"}
	}
	return provider.Process(ctx, task)
}

func buildProvider(config Config) Provider {
	providers := map[string]Provider{"mock": MockProvider{}}
	defaultName := "mock"
	if config.OpenRouterAPIKey == "" {
		slog.Warn("using mock provider; set OPENROUTER_API_KEY to enable OpenRouter")
	} else {
		model := strings.TrimSpace(config.OpenRouterModel)
		if model == "" {
			model = "openrouter/free"
		}
		providers["openrouter"] = NewOpenRouterProvider(config.OpenRouterAPIKey, model)
		defaultName = "openrouter"
		slog.Info("provider configured", "provider", "openrouter", "model", model)
	}
	return &providerRouter{defaultName: defaultName, providers: providers}
}

const staleRecoveryInterval = 30 * time.Second

type workerGroup struct {
	wg sync.WaitGroup
}

func (g *workerGroup) Start(ctx context.Context, store TaskStore, provider Provider, config Config, metrics *Metrics) {
	for workerID := 1; workerID <= config.WorkerCount; workerID++ {
		g.wg.Add(1)
		go func(id int) {
			defer g.wg.Done()
			runWorker(ctx, id, store, provider, config, metrics)
		}(workerID)
	}
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		runRetentionCleanup(ctx, store, config.Retention)
	}()
	if webhookStore, ok := store.(WebhookStore); ok && len(config.WebhookEncryptionKey) == 32 {
		g.wg.Add(1)
		go func() {
			defer g.wg.Done()
			runWebhookDeliveryWorker(ctx, webhookStore, config.WebhookEncryptionKey)
		}()
	}
}

func (g *workerGroup) Wait(ctx context.Context) {
	done := make(chan struct{})
	go func() {
		g.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		slog.Warn("workers did not stop before shutdown deadline")
		<-done
	}
}

func runRetentionCleanup(ctx context.Context, store TaskStore, retention time.Duration) {
	ticker := time.NewTicker(6 * time.Hour)
	defer ticker.Stop()
	cleanup := func() {
		cleanupCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		count, err := store.DeleteExpired(cleanupCtx, retention)
		if err != nil {
			slog.Error("retention cleanup failed", "error", err)
			return
		}
		if count > 0 {
			slog.Info("expired tasks deleted", "count", count)
		}
	}
	cleanup()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cleanup()
		}
	}
}

func runWorker(ctx context.Context, workerID int, store TaskStore, provider Provider, config Config, metrics *Metrics) {
	pollTicker := time.NewTicker(time.Second)
	recoveryTicker := time.NewTicker(staleRecoveryInterval)
	defer pollTicker.Stop()
	defer recoveryTicker.Stop()

	if err := store.RecoverStale(ctx); err != nil && ctx.Err() == nil {
		slog.Error("worker lease recovery failed", "worker_id", workerID, "error", err)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-recoveryTicker.C:
			recoveryCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			if err := store.RecoverStale(recoveryCtx); err != nil && ctx.Err() == nil {
				slog.Error("worker lease recovery failed", "worker_id", workerID, "error", err)
			}
			cancel()
		default:
		}

		claimCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		task, err := store.ClaimNext(claimCtx, config.LeaseDuration)
		cancel()
		if errors.Is(err, ErrNoQueuedTask) {
			select {
			case <-ctx.Done():
				return
			case <-pollTicker.C:
			}
			continue
		}
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Error("worker could not claim task", "worker_id", workerID, "error", err)
			select {
			case <-ctx.Done():
				return
			case <-pollTicker.C:
			}
			continue
		}

		slog.Info("task claimed", "worker_id", workerID, "task_id", task.ID, "attempt", task.AttemptCount, "max_attempts", task.MaxAttempts)
		if err := processTask(ctx, workerID, store, provider, task, config, metrics); err != nil && !errors.Is(err, ErrTaskLeaseLost) {
			slog.Error("task processing failed", "worker_id", workerID, "task_id", task.ID, "error", err.Error(), "failure_type", fmt.Sprintf("%T", err))
		}
	}
}

type leaseOutcome struct {
	cancelRequested bool
	lost            bool
	heartbeatError  error
}

func processTask(ctx context.Context, workerID int, store TaskStore, provider Provider, task TaskRecord, config Config, metrics *Metrics) error {
	started := time.Now()
	taskTimeout := config.TaskTimeout
	if task.TimeoutSeconds > 0 {
		taskTimeout = time.Duration(task.TimeoutSeconds) * time.Second
	}
	taskCtx, cancelTask := context.WithTimeout(ctx, taskTimeout)
	stopHeartbeat := make(chan struct{})
	heartbeatDone := make(chan leaseOutcome, 1)
	go func() {
		ticker := time.NewTicker(config.HeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stopHeartbeat:
				heartbeatDone <- leaseOutcome{}
				return
			case <-taskCtx.Done():
				heartbeatDone <- leaseOutcome{}
				return
			case <-ticker.C:
				heartbeatCtx, cancel := context.WithTimeout(taskCtx, 4*time.Second)
				owned, cancelRequested, err := store.Heartbeat(heartbeatCtx, task.ID, task.LeaseToken, config.LeaseDuration)
				cancel()
				if err != nil {
					if taskCtx.Err() != nil {
						heartbeatDone <- leaseOutcome{}
						return
					}
					cancelTask()
					heartbeatDone <- leaseOutcome{heartbeatError: err}
					return
				}
				if !owned {
					cancelTask()
					heartbeatDone <- leaseOutcome{lost: true}
					return
				}
				if cancelRequested {
					cancelTask()
					heartbeatDone <- leaseOutcome{cancelRequested: true}
					return
				}
			}
		}
	}()

	result, processErr := provider.Process(taskCtx, task)
	close(stopHeartbeat)
	outcome := <-heartbeatDone
	executionErr := taskCtx.Err()
	cancelTask()
	finalizeCtx, finalizeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer finalizeCancel()

	if outcome.lost {
		metrics.RecordTask(task.WorkspaceID, "lease_lost", time.Since(started), false)
		return ErrTaskLeaseLost
	}
	if outcome.cancelRequested {
		if err := store.FinalizeCancelled(finalizeCtx, task.ID, task.LeaseToken); err != nil {
			return err
		}
		metrics.RecordTask(task.WorkspaceID, "cancelled", time.Since(started), false)
		return nil
	}
	if ctx.Err() != nil || outcome.heartbeatError != nil {
		message := "worker stopping; task returned to queue"
		if outcome.heartbeatError != nil {
			message = "worker lease heartbeat failed; task returned to queue"
		}
		status, err := store.Fail(finalizeCtx, task.ID, task.LeaseToken, message, true)
		if err != nil {
			return err
		}
		metrics.RecordTask(task.WorkspaceID, status, time.Since(started), status == "queued")
		return nil
	}
	if errors.Is(executionErr, context.DeadlineExceeded) {
		status, err := store.Fail(finalizeCtx, task.ID, task.LeaseToken, "task exceeded the configured runtime limit", true)
		if err != nil {
			return err
		}
		metrics.RecordTask(task.WorkspaceID, status, time.Since(started), status == "queued")
		return nil
	}
	if processErr != nil {
		retryable := true
		var classified RetryableProviderError
		if errors.As(processErr, &classified) {
			retryable = classified.Retryable()
		}
		status, err := store.Fail(finalizeCtx, task.ID, task.LeaseToken, processErr.Error(), retryable)
		if err != nil {
			return fmt.Errorf("provider failed: %v; could not persist failure: %w", processErr, err)
		}
		metrics.RecordTask(task.WorkspaceID, status, time.Since(started), status == "queued")
		return processErr
	}

	status, err := store.Complete(finalizeCtx, task.ID, task.LeaseToken, result)
	if err != nil {
		return err
	}
	metrics.RecordTask(task.WorkspaceID, status, time.Since(started), false)
	slog.Info("task finalized", "worker_id", workerID, "task_id", task.ID, "status", status, "duration_ms", time.Since(started).Milliseconds())
	return nil
}
