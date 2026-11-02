# ModelRail

ModelRail is a PostgreSQL-backed queue for asynchronous AI tasks. It provides durable task history, status filtering and pagination, cancellation, retries, idempotent submission, scoped API keys, and a React operations console.

For application developers, ModelRail accepts asynchronous AI jobs over HTTP or through the TypeScript SDK. It can call the configured OpenRouter provider, save the job in Postgres, and return the result by API or signed webhook.

Workers provide **at-least-once execution**. A task may be submitted to a model more than once if a worker loses its lease after the upstream provider has accepted the request. Use idempotency keys for submissions and keep model/provider side effects safe to repeat.

## Run locally

Prerequisites: Go 1.25.3 or newer, Node.js 20 or newer, npm, and a PostgreSQL database. ModelRail works with Supabase Postgres or a local PostgreSQL instance; Docker Compose is optional for local database development.

1. Copy `.env.example` to `.env`. Set `DATABASE_URL` to the Postgres connection string from your provider and a private `MODELRAIL_BOOTSTRAP_API_KEY` with at least 32 characters. For Supabase, see the [database connection notes](docs/deployment.md#supabase-postgres). Set `OPENROUTER_API_KEY` to use OpenRouter; otherwise tasks use the mock provider.
2. Build the embedded frontend:

   ```powershell
   cd frontend
   npm ci
   npm run build
   cd ..
   ```

3. Start the service with `go run .`. The service applies versioned SQL migrations before accepting requests.
4. Open [http://localhost:8080](http://localhost:8080) and enter an API key. The bootstrap key works for initial setup; create a scoped dashboard key with the API below for regular use.

The bootstrap token can also be supplied as the legacy `MODELRAIL_API_KEY` environment variable. New deployments should use `MODELRAIL_BOOTSTRAP_API_KEY`.

## TypeScript SDK

The SDK source is in [`sdk/typescript`](sdk/typescript/README.md). It provides typed task submission, status reads, pagination, retry/cancel operations, and a bounded-backoff `waitForTask` helper. Publish the package to your npm organization before installing it from an application. The complete five-minute walkthrough is in [docs/typescript-quickstart.md](docs/typescript-quickstart.md).

Keep ModelRail API keys in your application's backend environment. The SDK is designed for server-side TypeScript and should not be bundled into public browser code with a long-lived key.

## Per-task model options and callbacks

Task requests can choose `provider` (`openrouter` or `mock`), an OpenRouter `model`, `max_tokens`, `temperature`, `timeout_seconds`, `max_attempts`, and a structured `response_format`. Settings are persisted with the task and reused by manual retries. See [API task options and webhooks](docs/api.md#task-options-and-completion-webhooks).

To request a signed completion callback, include `webhook: { "url": "https://...", "secret": "..." }`. The URL must use HTTPS on port 443 and resolve to public IP addresses. Set `WEBHOOK_ENCRYPTION_KEY` to 32 random bytes encoded as 64 hexadecimal characters; it encrypts callback secrets in Postgres. Generate one with `node -e "console.log(require('node:crypto').randomBytes(32).toString('hex'))"` and keep it stable while deliveries are pending. Delivery is durable and retried with exponential backoff. Callbacks are signed with HMAC-SHA256.

## API overview

All `/v1` and `/metrics` routes require a Bearer API key. Keys are stored as SHA-256 digests, shown only once when created, can be revoked, and carry scopes. Tasks and keys are isolated by workspace. The first install creates a default workspace; the bootstrap key belongs to it.

Create a dashboard key with read, submit, retry/cancel, and delete access:

```powershell
$headers = @{ Authorization = "Bearer $env:MODELRAIL_BOOTSTRAP_API_KEY" }
$body = @{ name = "operations dashboard"; scopes = @("tasks:read", "tasks:write", "tasks:manage", "tasks:delete") } | ConvertTo-Json
Invoke-RestMethod -Method Post -Uri http://localhost:8080/v1/api-keys -Headers $headers -ContentType "application/json" -Body $body
```

Copy the returned `token` when it is shown. ModelRail does not store or return the raw token again. See [API reference](docs/api.md) and [OpenAPI schema](docs/openapi.yaml) for routes and request details.

## Operations

- `/healthz` is a liveness check. `/readyz` returns `503` if PostgreSQL cannot be reached.
- `/metrics` exposes Prometheus counters, task duration histograms, and workspace queue depth. It requires the `metrics:read` scope.
- Structured JSON logs include request method, path, status, timing, task IDs, and worker IDs. Request bodies, API keys, model prompts, and results are not logged.
- The default rate limit is 120 requests per minute per API key and service process. The built-in limiter is process-local; use a shared gateway limiter when running multiple HTTP replicas.
- Terminal tasks are deleted after 30 days by default. Set `TASK_RETENTION_DAYS` to change the period. Active tasks are never removed by retention cleanup. Individual terminal tasks can be deleted with `DELETE /v1/tasks/{id}`.
- Deploy behind HTTPS termination and keep the service on a private network. Never expose the bootstrap key to browsers or application clients; create scoped keys instead.
- The worker queue is at-least-once. Lease tokens fence off stale workers; heartbeat renewals keep long model calls from being reclaimed. Cancellation stops the provider request when supported by its HTTP client.

## Configuration

| Variable | Default | Purpose |
| --- | --- | --- |
| `DATABASE_URL` | required | PostgreSQL URL |
| `MODELRAIL_BOOTSTRAP_API_KEY` | required | Initial workspace admin token; legacy `MODELRAIL_API_KEY` is accepted |
| `MODELRAIL_ADDR` | `:8080` | HTTP listen address |
| `DATABASE_CONNECT_TIMEOUT` | `30s` | Startup database deadline |
| `DATABASE_MAX_CONNS` | `10` | Maximum connections in the service's PostgreSQL pool (1–100) |
| `SHUTDOWN_TIMEOUT` | `20s` | Graceful HTTP and worker shutdown deadline |
| `TASK_TIMEOUT` | `2m` | Maximum runtime for one provider attempt (maximum `24h`) |
| `WORKER_COUNT` | `2` | Worker goroutines in this service process |
| `WORKER_LEASE_SECONDS` | `45` | Lease duration before work can be recovered |
| `WORKER_HEARTBEAT_SECONDS` | `10` | Lease renewal and cancellation polling interval |
| `RATE_LIMIT_PER_MINUTE` | `120` | Per-key process-local token bucket capacity/refill |
| `TASK_MAX_INPUT_BYTES` | `1048576` | Maximum input payload size; maximum supported value is 16 MiB |
| `TASK_RETENTION_DAYS` | `30` | Retention for completed, failed, and cancelled tasks |
| `OPENROUTER_API_KEY` | unset | Enables OpenRouter; unset uses the mock provider |
| `OPENROUTER_MODEL` | `openrouter/free` | Default OpenRouter model identifier; tasks may override it |
| `WEBHOOK_ENCRYPTION_KEY` | unset | 32 random bytes as 64 hex characters; required for webhook tasks |

## Backups and restore

For a self-managed PostgreSQL deployment, schedule encrypted backups outside the service and periodically test restores. On Windows, `scripts/backup.ps1` writes a timestamped custom-format dump to a protected directory using the PostgreSQL client tools. Schedule it with Task Scheduler and retain copies in encrypted off-host storage:

```powershell
./scripts/backup.ps1 -OutputDirectory D:\modelrail-backups
```

Stop ModelRail before restoring, then restore into the intended database:

```powershell
pg_restore --clean --if-exists --no-owner --dbname=$env:DATABASE_URL modelrail-backup.dump
```

Managed PostgreSQL should use the provider's automated backups and point-in-time recovery. Keep the recovery point objective, retention, encryption, and restore drill schedule in the deployment runbook. Migrations are forward-only; back up before upgrading.

## Build and release checks

Run `go test ./...`, `go build .`, `cd frontend && npm ci && npm run build`, `npx tsc -p ../sdk/typescript/tsconfig.json`, and `npm run test:e2e`. PostgreSQL integration checks use only `MODELRAIL_TEST_DATABASE_URL`, and refuse database names not ending in `_test` or `_ci`. GitHub Actions runs the Go checks, PostgreSQL upgrade/lifecycle checks, frontend and SDK builds, Playwright browser smoke test, and container build.

## Container image

Build with `docker build -t modelrail:local .`. The image builds the frontend and Go service in separate stages, then runs as a non-root user. Provide the runtime configuration through a secret manager or orchestrator; do not bake `.env` into the image. See [deployment notes](docs/deployment.md).
