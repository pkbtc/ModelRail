-- Supabase SQL Editor migration for the existing ModelRail tasks table.
-- This mirrors db/migrations/001_production_baseline.sql and
-- records its version so ModelRail's startup migration runner will not apply
-- it a second time. This migration does not enable row-level security.
BEGIN;

CREATE TABLE IF NOT EXISTS public.schema_migrations (
    version TEXT PRIMARY KEY,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

SELECT pg_advisory_xact_lock(84201971);

CREATE TABLE IF NOT EXISTS public.workspaces (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

INSERT INTO public.workspaces (id, name)
VALUES ('00000000-0000-4000-8000-000000000001', 'Default workspace')
ON CONFLICT (id) DO NOTHING;

CREATE TABLE IF NOT EXISTS public.api_keys (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES public.workspaces(id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    key_prefix TEXT NOT NULL,
    key_hash BYTEA NOT NULL UNIQUE,
    scopes TEXT[] NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    revoked_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS api_keys_workspace_idx
    ON public.api_keys (workspace_id, created_at DESC);

CREATE TABLE IF NOT EXISTS public.tasks (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL DEFAULT '00000000-0000-4000-8000-000000000001'
        REFERENCES public.workspaces(id),
    task TEXT NOT NULL,
    input TEXT NOT NULL,
    provider TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'queued',
    attempt_count INTEGER NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    max_attempts INTEGER NOT NULL DEFAULT 3 CHECK (max_attempts > 0),
    available_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    claimed_at TIMESTAMPTZ,
    lease_expires_at TIMESTAMPTZ,
    lease_token UUID,
    cancel_requested_at TIMESTAMPTZ,
    idempotency_key TEXT,
    request_hash TEXT,
    parent_task_id UUID REFERENCES public.tasks(id) ON DELETE SET NULL,
    result TEXT,
    error_message TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

ALTER TABLE public.tasks
    ADD COLUMN IF NOT EXISTS workspace_id UUID
        DEFAULT '00000000-0000-4000-8000-000000000001',
    ADD COLUMN IF NOT EXISTS lease_expires_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS lease_token UUID,
    ADD COLUMN IF NOT EXISTS cancel_requested_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS idempotency_key TEXT,
    ADD COLUMN IF NOT EXISTS request_hash TEXT,
    ADD COLUMN IF NOT EXISTS parent_task_id UUID;

UPDATE public.tasks
SET workspace_id = '00000000-0000-4000-8000-000000000001'
WHERE workspace_id IS NULL;

UPDATE public.tasks
SET lease_expires_at = claimed_at + INTERVAL '2 minutes'
WHERE status = 'processing' AND lease_expires_at IS NULL AND claimed_at IS NOT NULL;

ALTER TABLE public.tasks
    ALTER COLUMN workspace_id SET DEFAULT '00000000-0000-4000-8000-000000000001',
    ALTER COLUMN workspace_id SET NOT NULL;

ALTER TABLE public.tasks DROP CONSTRAINT IF EXISTS tasks_status_check;
ALTER TABLE public.tasks
    ADD CONSTRAINT tasks_status_check
    CHECK (status IN ('queued', 'processing', 'completed', 'failed', 'cancelled'));

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'tasks_workspace_id_fkey'
          AND conrelid = 'public.tasks'::regclass
    ) THEN
        ALTER TABLE public.tasks
            ADD CONSTRAINT tasks_workspace_id_fkey
            FOREIGN KEY (workspace_id) REFERENCES public.workspaces(id);
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'tasks_parent_task_id_fkey'
          AND conrelid = 'public.tasks'::regclass
    ) THEN
        ALTER TABLE public.tasks
            ADD CONSTRAINT tasks_parent_task_id_fkey
            FOREIGN KEY (parent_task_id) REFERENCES public.tasks(id) ON DELETE SET NULL;
    END IF;
END $$;

CREATE INDEX IF NOT EXISTS tasks_queue_idx
    ON public.tasks (available_at, created_at)
    WHERE status = 'queued';
CREATE INDEX IF NOT EXISTS tasks_workspace_history_idx
    ON public.tasks (workspace_id, created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS tasks_workspace_status_history_idx
    ON public.tasks (workspace_id, status, created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS tasks_lease_expiry_idx
    ON public.tasks (lease_expires_at)
    WHERE status = 'processing';
CREATE UNIQUE INDEX IF NOT EXISTS tasks_idempotency_idx
    ON public.tasks (workspace_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;

INSERT INTO public.schema_migrations (version)
VALUES ('001_production_baseline.sql')
ON CONFLICT (version) DO NOTHING;

COMMIT;
