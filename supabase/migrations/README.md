# Run the ModelRail Supabase migrations

1. Open the Supabase project you use for ModelRail and go to **SQL Editor**.
2. For a new project, run `20261102000000_modelrail_production.sql` first. It creates the initial ModelRail tables and preserves an existing `tasks` table.
3. Run `20261102000002_task_options_webhooks.sql`. It adds per-task settings and the durable webhook delivery queue.
4. Check each result for success. The scripts record their app migration versions and leave RLS disabled.
5. Start ModelRail with `go run .`; its migration runner should see the versions recorded and leave the schema as-is.

Run these against the intended Supabase project in timestamp order. The SQL is safe to rerun, but it changes the database schema, so inspect the target project before executing it.
