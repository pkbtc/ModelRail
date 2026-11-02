package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPostgresMigrationUpgradeAndTaskLifecycle(t *testing.T) {
	databaseURL := strings.TrimSpace(os.Getenv("MODELRAIL_TEST_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("MODELRAIL_TEST_DATABASE_URL is not set; PostgreSQL integration checks run in CI")
	}

	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse test database URL: %v", err)
	}
	if !strings.HasSuffix(strings.ToLower(config.ConnConfig.Database), "_test") && !strings.HasSuffix(strings.ToLower(config.ConnConfig.Database), "_ci") {
		t.Fatalf("refusing to run destructive integration checks against database %q; database name must end in _test or _ci", config.ConnConfig.Database)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("connect test database: %v", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("ping test database: %v", err)
	}

	if _, err := pool.Exec(ctx, `DROP TABLE IF EXISTS public.schema_migrations, public.webhook_deliveries, public.tasks, public.api_keys, public.workspaces CASCADE`); err != nil {
		t.Fatalf("reset isolated integration database: %v", err)
	}
	_, err = pool.Exec(ctx, `CREATE TABLE public.tasks (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(), task TEXT NOT NULL, input TEXT NOT NULL,
		provider TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'queued'
		CHECK (status IN ('queued', 'processing', 'completed', 'failed')),
		attempt_count INTEGER NOT NULL DEFAULT 0, max_attempts INTEGER NOT NULL DEFAULT 3,
		available_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), claimed_at TIMESTAMPTZ,
		result TEXT, error_message TEXT, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`)
	if err != nil {
		t.Fatalf("create legacy schema: %v", err)
	}
	if err := runMigrations(ctx, pool); err != nil {
		t.Fatalf("upgrade legacy schema: %v", err)
	}
	if err := runMigrations(ctx, pool); err != nil {
		t.Fatalf("rerun migrations: %v", err)
	}

	store := &PostgresTaskStore{pool: pool}
	bootstrapKey := "integration-test-bootstrap-key-0123456789"
	if err := store.EnsureBootstrapKey(ctx, bootstrapKey); err != nil {
		t.Fatalf("seed bootstrap key: %v", err)
	}
	principal, err := store.Authenticate(ctx, bootstrapKey)
	if err != nil || !principal.HasScope(ScopeTaskRead) || principal.WorkspaceID != defaultWorkspaceID {
		t.Fatalf("authenticate bootstrap key: principal=%+v err=%v", principal, err)
	}
	if err := store.RevokeAPIKey(ctx, principal.WorkspaceID, principal.KeyID, principal); !errors.Is(err, ErrLastAdminKey) {
		t.Fatalf("expected last-admin protection, got %v", err)
	}

	workspace, adminKey, adminToken, err := store.CreateWorkspaceWithAdminKey(ctx, "integration test", "integration owner")
	if err != nil {
		t.Fatalf("create test workspace with initial admin: %v", err)
	}
	workspaceID := workspace.ID
	workspacePrincipal, err := store.Authenticate(ctx, adminToken)
	if err != nil || workspacePrincipal.WorkspaceID != workspaceID || adminKey.Scopes[0] != ScopeAdmin {
		t.Fatalf("authenticate workspace admin: principal=%+v key=%+v err=%v", workspacePrincipal, adminKey, err)
	}
	readKey, readToken, err := store.CreateAPIKey(ctx, workspaceID, "read-only", []string{ScopeTaskRead})
	if err != nil {
		t.Fatalf("create scoped key: %v", err)
	}
	readPrincipal, err := store.Authenticate(ctx, readToken)
	if err != nil || !readPrincipal.HasScope(ScopeTaskRead) || readPrincipal.HasScope(ScopeTaskWrite) {
		t.Fatalf("scoped key permissions: principal=%+v err=%v", readPrincipal, err)
	}
	keys, err := store.ListAPIKeys(ctx, workspaceID)
	if err != nil || len(keys) != 2 {
		t.Fatalf("list API key metadata: keys=%+v err=%v", keys, err)
	}
	for _, key := range keys {
		if key.Prefix == readToken {
			t.Fatal("API key list must not return a full secret token")
		}
	}
	if err := store.RevokeAPIKey(ctx, workspaceID, readKey.ID, workspacePrincipal); err != nil {
		t.Fatalf("revoke scoped key: %v", err)
	}
	if _, err := store.Authenticate(ctx, readToken); !errors.Is(err, ErrInvalidAPIKey) {
		t.Fatalf("revoked key should no longer authenticate, got %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM public.tasks WHERE workspace_id = $1::uuid`, workspaceID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM public.api_keys WHERE workspace_id = $1::uuid`, workspaceID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM public.workspaces WHERE id = $1::uuid`, workspaceID)
	})

	idem := "request-123"
	first, created, err := store.Create(ctx, workspaceID, TaskRecord{Task: "summarize", Input: "hello", Provider: "mock", MaxAttempts: 3}, &idem, "request-hash")
	if err != nil || !created {
		t.Fatalf("create task: created=%v err=%v", created, err)
	}
	replayed, created, err := store.Create(ctx, workspaceID, TaskRecord{Task: "summarize", Input: "hello", Provider: "mock", MaxAttempts: 3}, &idem, "request-hash")
	if err != nil || created || replayed.ID != first.ID {
		t.Fatalf("idempotent replay: task=%+v created=%v err=%v", replayed, created, err)
	}
	if _, _, err := store.Create(ctx, workspaceID, TaskRecord{Task: "summarize", Input: "different", Provider: "mock", MaxAttempts: 3}, &idem, "different-hash"); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("expected idempotency conflict, got %v", err)
	}
	if _, err := store.Get(ctx, defaultWorkspaceID, first.ID); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("task should be isolated from the default workspace, got %v", err)
	}

	secondHistoryTask, _, err := store.Create(ctx, workspaceID, TaskRecord{Task: "classify", Input: "newest", Provider: "mock", MaxAttempts: 3}, nil, "")
	if err != nil {
		t.Fatalf("create second history task: %v", err)
	}
	page, err := store.List(ctx, workspaceID, "queued", 1, nil)
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != secondHistoryTask.ID || page.NextCursor == "" {
		t.Fatalf("list first history page: page=%+v err=%v", page, err)
	}
	pageCursor, err := parseTaskCursor(page.NextCursor)
	if err != nil {
		t.Fatalf("decode next-page cursor: %v", err)
	}
	olderPage, err := store.List(ctx, workspaceID, "queued", 1, pageCursor)
	if err != nil || len(olderPage.Items) != 1 || olderPage.Items[0].ID != first.ID {
		t.Fatalf("list next history page: page=%+v err=%v", olderPage, err)
	}
	if _, err := store.RequestCancel(ctx, workspaceID, secondHistoryTask.ID); err != nil {
		t.Fatalf("cancel second history task: %v", err)
	}
	if err := store.Delete(ctx, workspaceID, secondHistoryTask.ID); err != nil {
		t.Fatalf("delete second history task: %v", err)
	}

	claimed, err := store.ClaimNext(ctx, 45*time.Second)
	if err != nil || claimed.ID != first.ID || claimed.LeaseToken == "" {
		t.Fatalf("claim task: task=%+v err=%v", claimed, err)
	}
	owned, cancelRequested, err := store.Heartbeat(ctx, claimed.ID, claimed.LeaseToken, 45*time.Second)
	if err != nil || !owned || cancelRequested {
		t.Fatalf("renew lease: owned=%v cancel=%v err=%v", owned, cancelRequested, err)
	}
	if _, err := store.RequestCancel(ctx, workspaceID, claimed.ID); err != nil {
		t.Fatalf("request cancellation: %v", err)
	}
	owned, cancelRequested, err = store.Heartbeat(ctx, claimed.ID, claimed.LeaseToken, 45*time.Second)
	if err != nil || !owned || !cancelRequested {
		t.Fatalf("observe cancellation: owned=%v cancel=%v err=%v", owned, cancelRequested, err)
	}
	if err := store.FinalizeCancelled(ctx, claimed.ID, claimed.LeaseToken); err != nil {
		t.Fatalf("finalize cancellation: %v", err)
	}
	cancelled, err := store.Get(ctx, workspaceID, claimed.ID)
	if err != nil || cancelled.Status != "cancelled" {
		t.Fatalf("cancelled task status: task=%+v err=%v", cancelled, err)
	}

	completeTask, _, err := store.Create(ctx, workspaceID, TaskRecord{Task: "classify", Input: "ticket", Provider: "mock", MaxAttempts: 3}, nil, "")
	if err != nil {
		t.Fatalf("create completion task: %v", err)
	}
	claimed, err = store.ClaimNext(ctx, 45*time.Second)
	if err != nil || claimed.ID != completeTask.ID {
		t.Fatalf("claim completion task: task=%+v err=%v", claimed, err)
	}
	status, err := store.Complete(ctx, claimed.ID, claimed.LeaseToken, "done")
	if err != nil || status != "completed" {
		t.Fatalf("complete task: status=%s err=%v", status, err)
	}
	if err := store.Delete(ctx, workspaceID, completeTask.ID); err != nil {
		t.Fatalf("delete completed task: %v", err)
	}

	retrySource, _, err := store.Create(ctx, workspaceID, TaskRecord{Task: "extract", Input: "invoice", Provider: "mock", MaxAttempts: 3}, nil, "")
	if err != nil {
		t.Fatalf("create retry source: %v", err)
	}
	claimed, err = store.ClaimNext(ctx, 45*time.Second)
	if err != nil || claimed.ID != retrySource.ID {
		t.Fatalf("claim retry source: task=%+v err=%v", claimed, err)
	}
	status, err = store.Fail(ctx, claimed.ID, claimed.LeaseToken, "transient error", true)
	if err != nil || status != "queued" {
		t.Fatalf("retryable failure should requeue: status=%s err=%v", status, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE public.tasks SET available_at = NOW() WHERE id = $1::uuid`, retrySource.ID); err != nil {
		t.Fatalf("make retry available: %v", err)
	}
	claimed, err = store.ClaimNext(ctx, 45*time.Second)
	if err != nil || claimed.ID != retrySource.ID {
		t.Fatalf("claim retried source: task=%+v err=%v", claimed, err)
	}
	status, err = store.Fail(ctx, claimed.ID, claimed.LeaseToken, "permanent error", false)
	if err != nil || status != "failed" {
		t.Fatalf("permanent failure should not requeue: status=%s err=%v", status, err)
	}
	retryTask, err := store.Retry(ctx, workspaceID, retrySource.ID)
	if err != nil || retryTask.Status != "queued" || retryTask.ParentTaskID == nil || *retryTask.ParentTaskID != retrySource.ID {
		t.Fatalf("manual retry should create linked task: task=%+v err=%v", retryTask, err)
	}
	if _, err := store.RequestCancel(ctx, workspaceID, retryTask.ID); err != nil {
		t.Fatalf("cancel manual retry task: %v", err)
	}
	if err := store.Delete(ctx, workspaceID, retryTask.ID); err != nil {
		t.Fatalf("delete cancelled retry task: %v", err)
	}
	if err := store.Delete(ctx, workspaceID, retrySource.ID); err != nil {
		t.Fatalf("delete failed source task: %v", err)
	}

	if _, err := pool.Exec(ctx, `UPDATE public.tasks SET updated_at = NOW() - INTERVAL '2 days' WHERE id = $1::uuid`, first.ID); err != nil {
		t.Fatalf("age cancelled task for retention: %v", err)
	}
	deleted, err := store.DeleteExpired(ctx, 24*time.Hour)
	if err != nil || deleted != 1 {
		t.Fatalf("retention should delete the expired terminal task: deleted=%d err=%v", deleted, err)
	}
	if _, err := store.Get(ctx, workspaceID, first.ID); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("retained task should be removed: %v", err)
	}
}
