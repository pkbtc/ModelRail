BEGIN;

CREATE TABLE IF NOT EXISTS public.schema_migrations (
    version TEXT PRIMARY KEY,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

SELECT pg_advisory_xact_lock(84201971);

ALTER TABLE public.tasks
    ADD COLUMN IF NOT EXISTS model TEXT,
    ADD COLUMN IF NOT EXISTS temperature DOUBLE PRECISION,
    ADD COLUMN IF NOT EXISTS max_output_tokens INTEGER,
    ADD COLUMN IF NOT EXISTS timeout_seconds INTEGER,
    ADD COLUMN IF NOT EXISTS response_format JSONB,
    ADD COLUMN IF NOT EXISTS webhook_url TEXT,
    ADD COLUMN IF NOT EXISTS webhook_secret_encrypted BYTEA;

CREATE TABLE IF NOT EXISTS public.webhook_deliveries (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    task_id UUID NOT NULL,
    workspace_id UUID NOT NULL,
    event TEXT NOT NULL,
    target_url TEXT NOT NULL,
    secret_ciphertext BYTEA NOT NULL,
    payload JSONB NOT NULL,
    status TEXT NOT NULL DEFAULT 'queued'
        CHECK (status IN ('queued', 'processing', 'delivered', 'failed')),
    attempt_count INTEGER NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    max_attempts INTEGER NOT NULL DEFAULT 10 CHECK (max_attempts > 0),
    available_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    lease_expires_at TIMESTAMPTZ,
    lease_token UUID,
    last_status INTEGER,
    last_error TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    delivered_at TIMESTAMPTZ,
    UNIQUE (task_id, event)
);

CREATE INDEX IF NOT EXISTS webhook_deliveries_queue_idx
    ON public.webhook_deliveries (available_at, created_at)
    WHERE status IN ('queued', 'processing');
CREATE INDEX IF NOT EXISTS webhook_deliveries_retention_idx
    ON public.webhook_deliveries (updated_at)
    WHERE status IN ('delivered', 'failed');

CREATE OR REPLACE FUNCTION public.modelrail_enqueue_task_webhook()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
    event_name TEXT;
BEGIN
    IF NEW.status IS DISTINCT FROM OLD.status
       AND NEW.status IN ('completed', 'failed', 'cancelled')
       AND NEW.webhook_url IS NOT NULL
       AND NEW.webhook_secret_encrypted IS NOT NULL THEN
        event_name := 'task.' || NEW.status;

        INSERT INTO public.webhook_deliveries (
            task_id, workspace_id, event, target_url, secret_ciphertext, payload
        ) VALUES (
            NEW.id,
            NEW.workspace_id,
            event_name,
            NEW.webhook_url,
            NEW.webhook_secret_encrypted,
            jsonb_build_object(
                'id', NEW.id,
                'event', event_name,
                'created_at', NOW(),
                'data', jsonb_build_object(
                    'id', NEW.id,
                    'task', NEW.task,
                    'provider', NEW.provider,
                    'model', NEW.model,
                    'status', NEW.status,
                    'attempt_count', NEW.attempt_count,
                    'max_attempts', NEW.max_attempts,
                    'result', NEW.result,
                    'error_message', NEW.error_message,
                    'created_at', NEW.created_at,
                    'updated_at', NEW.updated_at
                )
            )
        )
        ON CONFLICT (task_id, event) DO NOTHING;
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS modelrail_task_webhook_outbox ON public.tasks;
CREATE TRIGGER modelrail_task_webhook_outbox
AFTER UPDATE OF status ON public.tasks
FOR EACH ROW
EXECUTE FUNCTION public.modelrail_enqueue_task_webhook();

INSERT INTO public.schema_migrations (version)
VALUES ('002_task_options_webhooks.sql')
ON CONFLICT (version) DO NOTHING;

COMMIT;
