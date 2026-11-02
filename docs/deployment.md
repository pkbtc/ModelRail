# Deployment runbook

## Container

Build the multi-stage image from the repository root:

```sh
docker build -t modelrail:<release-tag> .
```

Run it with a PostgreSQL URL and a bootstrap token provided by your secret manager. Set `OPENROUTER_API_KEY` only when the OpenRouter provider is enabled. Set `TASK_TIMEOUT` to the maximum runtime for one provider attempt (2 minutes by default, up to 24 hours). Migrations run transactionally before the listener starts. Run one service instance for the built-in rate limiter, or enforce a shared limit at the reverse proxy when using multiple instances.

For signed callback tasks, set `WEBHOOK_ENCRYPTION_KEY` to 32 random bytes encoded as 64 hexadecimal characters. Keep this key stable and back it up separately from the database; pending deliveries cannot be signed if it is lost or changed.

Run the container as a non-root user (the image already does), keep it on a private network, and terminate TLS at a trusted ingress or load balancer. Permit inbound traffic only from that ingress. Use encrypted database storage and encrypted backup storage. Do not mount the host `.env` file or bake credentials into the image.

## Supabase Postgres

In the Supabase Dashboard, open **Connect** and copy a Postgres connection string. ModelRail is a persistent Go service, so use the direct connection when its IPv6/IPv4 requirements fit the host network. If the host is IPv4-only, use Supabase's shared **session** pooler. The application pool defaults to 10 connections; set `DATABASE_MAX_CONNS` lower if needed to fit the project's available connection budget. Supabase recommends direct connections for migrations and backup/restore workflows. See [Supabase's connection guide](https://supabase.com/docs/guides/database/connecting-to-postgres) for the current options and limits.

Keep the connection string server-side. Percent-encode reserved characters in the database password. Set `sslmode=require` at minimum; for server identity verification, configure `sslmode=verify-full` with the root certificate from Supabase Database settings. The service connects to Postgres directly and applies workspace isolation in its API queries; do not put the database password or a Supabase service-role key in the browser.

## Database changes

The service applies ordered SQL files in `db/migrations` and records each successful version in `public.schema_migrations`. Migrations are forward-only and stop startup on failure. Before a release that changes schema, take a PostgreSQL backup and check the migration against a restored staging copy. Keep the migration lock and schema history intact.

## Health and monitoring

Configure the orchestrator liveness probe to call `/healthz` and readiness probe to call `/readyz`. Scrape `/metrics` with a key that has `metrics:read`; the endpoint is workspace-scoped. Alert on readiness failures, growing queued-task counts, sustained processing duration, and rising task failures/retries. Logs use JSON and never include request bodies or bearer tokens.

## Backups and recovery

Configure encrypted automated backups and point-in-time recovery on the PostgreSQL service or schedule `pg_dump` to encrypted object storage with retention. A backup is only useful after a restore drill. Restore into a separate database first, verify `schema_migrations`, API-key authentication, task history, and worker claims, then direct traffic to the restored database. Do not point the live service at a backup while restore is running.

## Key rotation

Create a replacement API key with the same or narrower scopes, install it in clients, confirm requests succeed, then revoke the old key. Bootstrap admin credentials are seeded by digest only once; revoking the bootstrap key does not recreate it on restart. If it must be replaced, set a new high-entropy `MODELRAIL_BOOTSTRAP_API_KEY` during a controlled startup; the new token is added as an admin key. Keep bootstrap credentials in an operator-only secret store.

## Limits and delivery semantics

Task execution is at-least-once. Lease fencing prevents an expired worker from writing a result after another worker claims the run, but the upstream model may already have processed a request when a connection or worker is lost. Clients should send `Idempotency-Key` for submit retries and account for provider billing on ambiguous failures. Rate limits are in-memory per process; multi-instance deployments must enforce a shared edge limit.
