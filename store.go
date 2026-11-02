package main

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrTaskNotFound        = errors.New("task not found")
	ErrNoQueuedTask        = errors.New("no queued task")
	ErrIdempotencyConflict = errors.New("idempotency key was already used with a different request")
	ErrTaskNotRetryable    = errors.New("task is not in a retryable state")
	ErrTaskNotCancelable   = errors.New("task is not active")
	ErrTaskActive          = errors.New("active tasks must be cancelled before deletion")
	ErrTaskLeaseLost       = errors.New("task lease is no longer owned by this worker")
	ErrNoWebhookDelivery   = errors.New("no webhook delivery is ready")
)

const defaultWorkspaceID = "00000000-0000-4000-8000-000000000001"

type TaskRecord struct {
	ID                     string          `json:"id"`
	WorkspaceID            string          `json:"-"`
	Task                   string          `json:"task"`
	Input                  string          `json:"-"`
	Provider               string          `json:"provider"`
	Model                  string          `json:"model,omitempty"`
	Status                 string          `json:"status"`
	AttemptCount           int             `json:"attempt_count"`
	MaxAttempts            int             `json:"max_attempts"`
	Temperature            *float64        `json:"temperature,omitempty"`
	MaxOutputTokens        *int            `json:"max_tokens,omitempty"`
	TimeoutSeconds         int             `json:"timeout_seconds,omitempty"`
	ResponseFormat         json.RawMessage `json:"response_format,omitempty"`
	WebhookURL             string          `json:"webhook_url,omitempty"`
	WebhookSecretEncrypted []byte          `json:"-"`
	Result                 *string         `json:"result"`
	ErrorMessage           *string         `json:"error_message"`
	CancelRequested        bool            `json:"cancel_requested"`
	ParentTaskID           *string         `json:"parent_task_id"`
	CreatedAt              time.Time       `json:"created_at"`
	UpdatedAt              time.Time       `json:"updated_at"`
	LeaseToken             string          `json:"-"`
}

type TaskCursor struct {
	CreatedAt time.Time `json:"created_at"`
	ID        string    `json:"id"`
}

type TaskPage struct {
	Items      []TaskRecord `json:"items"`
	NextCursor string       `json:"next_cursor,omitempty"`
}

type WebhookDelivery struct {
	ID               string
	TaskID           string
	LeaseToken       string
	Event            string
	TargetURL        string
	SecretCiphertext []byte
	Payload          []byte
	AttemptCount     int
	MaxAttempts      int
}

type WebhookStore interface {
	ClaimWebhookDelivery(ctx context.Context) (WebhookDelivery, error)
	CompleteWebhookDelivery(ctx context.Context, id, leaseToken string, statusCode int) error
	FailWebhookDelivery(ctx context.Context, id, leaseToken, message string, statusCode int, retryable bool) (string, error)
}

type TaskStore interface {
	Create(ctx context.Context, workspaceID string, task TaskRecord, idempotencyKey *string, requestHash string) (TaskRecord, bool, error)
	Complete(ctx context.Context, id, leaseToken, result string) (string, error)
	Fail(ctx context.Context, id, leaseToken, message string, retryable bool) (string, error)
	Get(ctx context.Context, workspaceID, id string) (TaskRecord, error)
	List(ctx context.Context, workspaceID, status string, limit int, cursor *TaskCursor) (TaskPage, error)
	Retry(ctx context.Context, workspaceID, id string) (TaskRecord, error)
	RequestCancel(ctx context.Context, workspaceID, id string) (TaskRecord, error)
	FinalizeCancelled(ctx context.Context, id, leaseToken string) error
	Delete(ctx context.Context, workspaceID, id string) error
	ClaimNext(ctx context.Context, leaseDuration time.Duration) (TaskRecord, error)
	Heartbeat(ctx context.Context, id, leaseToken string, leaseDuration time.Duration) (bool, bool, error)
	RecoverStale(ctx context.Context) error
	DeleteExpired(ctx context.Context, retention time.Duration) (int64, error)
	QueueStats(ctx context.Context, workspaceID string) (map[string]int64, error)
}

type rowScanner interface {
	Scan(dest ...any) error
}

type PostgresTaskStore struct {
	pool *pgxpool.Pool
}

const taskColumns = `id::text, workspace_id::text, task, input, provider, status,
	attempt_count, max_attempts, result, error_message,
	(cancel_requested_at IS NOT NULL), parent_task_id::text, created_at, updated_at, lease_token::text,
	COALESCE(model, ''), temperature, max_output_tokens, timeout_seconds, response_format,
	COALESCE(webhook_url, ''), webhook_secret_encrypted`

func scanTask(row rowScanner) (TaskRecord, error) {
	var task TaskRecord
	var result, errorMessage, parentTaskID, leaseToken, model, webhookURL sql.NullString
	var temperature sql.NullFloat64
	var maxOutputTokens, timeoutSeconds sql.NullInt32
	var responseFormat, webhookSecret []byte
	err := row.Scan(
		&task.ID,
		&task.WorkspaceID,
		&task.Task,
		&task.Input,
		&task.Provider,
		&task.Status,
		&task.AttemptCount,
		&task.MaxAttempts,
		&result,
		&errorMessage,
		&task.CancelRequested,
		&parentTaskID,
		&task.CreatedAt,
		&task.UpdatedAt,
		&leaseToken,
		&model,
		&temperature,
		&maxOutputTokens,
		&timeoutSeconds,
		&responseFormat,
		&webhookURL,
		&webhookSecret,
	)
	if err != nil {
		return TaskRecord{}, err
	}
	if result.Valid {
		task.Result = &result.String
	}
	if errorMessage.Valid {
		task.ErrorMessage = &errorMessage.String
	}
	if parentTaskID.Valid {
		task.ParentTaskID = &parentTaskID.String
	}
	if leaseToken.Valid {
		task.LeaseToken = leaseToken.String
	}
	if model.Valid {
		task.Model = model.String
	}
	if temperature.Valid {
		value := temperature.Float64
		task.Temperature = &value
	}
	if maxOutputTokens.Valid {
		value := int(maxOutputTokens.Int32)
		task.MaxOutputTokens = &value
	}
	if timeoutSeconds.Valid {
		task.TimeoutSeconds = int(timeoutSeconds.Int32)
	}
	if len(responseFormat) > 0 {
		task.ResponseFormat = append(json.RawMessage(nil), responseFormat...)
	}
	if webhookURL.Valid {
		task.WebhookURL = webhookURL.String
	}
	task.WebhookSecretEncrypted = append([]byte(nil), webhookSecret...)
	return task, nil
}

func (s *PostgresTaskStore) Create(ctx context.Context, workspaceID string, task TaskRecord, idempotencyKey *string, requestHash string) (TaskRecord, bool, error) {
	var idem any
	if idempotencyKey != nil {
		idem = *idempotencyKey
	}
	var temperature, maxOutputTokens, timeoutSeconds any
	if task.Temperature != nil {
		temperature = *task.Temperature
	}
	if task.MaxOutputTokens != nil {
		maxOutputTokens = *task.MaxOutputTokens
	}
	if task.TimeoutSeconds > 0 {
		timeoutSeconds = task.TimeoutSeconds
	}
	var responseFormat, webhookURL any
	if len(task.ResponseFormat) > 0 {
		responseFormat = string(task.ResponseFormat)
	}
	if task.WebhookURL != "" {
		webhookURL = task.WebhookURL
	}
	row := s.pool.QueryRow(ctx,
		`INSERT INTO public.tasks (
			workspace_id, task, input, provider, model, status, max_attempts,
			temperature, max_output_tokens, timeout_seconds, response_format,
			webhook_url, webhook_secret_encrypted, idempotency_key, request_hash
		)
		 VALUES ($1::uuid, $2, $3, $4, $5, 'queued', $6, $7, $8, $9, $10, $11, $12, $13, $14)
		 ON CONFLICT (workspace_id, idempotency_key) WHERE idempotency_key IS NOT NULL DO NOTHING
		 RETURNING id::text`,
		workspaceID, task.Task, task.Input, task.Provider, task.Model, task.MaxAttempts,
		temperature, maxOutputTokens, timeoutSeconds, responseFormat,
		webhookURL, task.WebhookSecretEncrypted, idem, requestHash,
	)
	var id string
	err := row.Scan(&id)
	if err == nil {
		task, getErr := s.Get(ctx, workspaceID, id)
		return task, true, getErr
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return TaskRecord{}, false, err
	}
	if idempotencyKey == nil {
		return TaskRecord{}, false, errors.New("task insert returned no row")
	}

	var existingID, existingHash string
	err = s.pool.QueryRow(ctx,
		`SELECT id::text, request_hash FROM public.tasks
		 WHERE workspace_id = $1::uuid AND idempotency_key = $2`,
		workspaceID, *idempotencyKey,
	).Scan(&existingID, &existingHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return TaskRecord{}, false, errors.New("idempotency conflict row disappeared")
	}
	if err != nil {
		return TaskRecord{}, false, err
	}
	if existingHash != requestHash {
		return TaskRecord{}, false, ErrIdempotencyConflict
	}
	existingTask, getErr := s.Get(ctx, workspaceID, existingID)
	return existingTask, false, getErr
}

func (s *PostgresTaskStore) Get(ctx context.Context, workspaceID, id string) (TaskRecord, error) {
	task, err := scanTask(s.pool.QueryRow(ctx,
		`SELECT `+taskColumns+` FROM public.tasks WHERE workspace_id = $1::uuid AND id = $2::uuid`,
		workspaceID, id,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return TaskRecord{}, ErrTaskNotFound
	}
	return task, err
}

func (s *PostgresTaskStore) List(
	ctx context.Context,
	workspaceID, status string,
	limit int,
	cursor *TaskCursor,
) (TaskPage, error) {
	var statusFilter any
	if status != "" {
		statusFilter = status
	}
	var cursorTime any
	var cursorID any
	if cursor != nil {
		cursorTime = cursor.CreatedAt
		cursorID = cursor.ID
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+taskColumns+` FROM public.tasks
		 WHERE workspace_id = $1::uuid
		   AND ($2::text IS NULL OR status = $2)
		   AND ($3::timestamptz IS NULL OR (created_at, id) < ($3, $4::uuid))
		 ORDER BY created_at DESC, id DESC
		 LIMIT $5`,
		workspaceID, statusFilter, cursorTime, cursorID, limit+1,
	)
	if err != nil {
		return TaskPage{}, err
	}
	defer rows.Close()

	items := make([]TaskRecord, 0, limit+1)
	for rows.Next() {
		task, scanErr := scanTask(rows)
		if scanErr != nil {
			return TaskPage{}, scanErr
		}
		items = append(items, task)
	}
	if err := rows.Err(); err != nil {
		return TaskPage{}, err
	}

	page := TaskPage{Items: items}
	if len(items) > limit {
		page.Items = items[:limit]
		page.NextCursor = encodeTaskCursor(page.Items[len(page.Items)-1])
	}
	return page, nil
}

func encodeTaskCursor(task TaskRecord) string {
	raw, _ := json.Marshal(TaskCursor{CreatedAt: task.CreatedAt, ID: task.ID})
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeTaskCursor(value string) (*TaskCursor, error) {
	if value == "" {
		return nil, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return nil, errors.New("invalid pagination cursor")
	}
	var cursor TaskCursor
	if err := json.Unmarshal(raw, &cursor); err != nil || !isUUID(cursor.ID) || cursor.CreatedAt.IsZero() {
		return nil, errors.New("invalid pagination cursor")
	}
	return &cursor, nil
}

func (s *PostgresTaskStore) Retry(ctx context.Context, workspaceID, id string) (TaskRecord, error) {
	task, err := scanTask(s.pool.QueryRow(ctx,
		`INSERT INTO public.tasks (
			workspace_id, task, input, provider, status, max_attempts, parent_task_id,
			model, temperature, max_output_tokens, timeout_seconds, response_format,
			webhook_url, webhook_secret_encrypted
		)
		 SELECT source.workspace_id, source.task, source.input, source.provider, 'queued', source.max_attempts, source.id,
			source.model, source.temperature, source.max_output_tokens, source.timeout_seconds, source.response_format,
			source.webhook_url, source.webhook_secret_encrypted
		 FROM public.tasks AS source
		 WHERE source.workspace_id = $1::uuid AND source.id = $2::uuid AND source.status = 'failed'
		 RETURNING `+taskColumns,
		workspaceID, id,
	))
	if err == nil {
		return task, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return TaskRecord{}, err
	}
	_, lookupErr := s.Get(ctx, workspaceID, id)
	if errors.Is(lookupErr, ErrTaskNotFound) {
		return TaskRecord{}, ErrTaskNotFound
	}
	if lookupErr != nil {
		return TaskRecord{}, lookupErr
	}
	return TaskRecord{}, ErrTaskNotRetryable
}

func (s *PostgresTaskStore) RequestCancel(ctx context.Context, workspaceID, id string) (TaskRecord, error) {
	task, err := scanTask(s.pool.QueryRow(ctx,
		`UPDATE public.tasks
		 SET status = CASE WHEN status = 'queued' THEN 'cancelled' ELSE status END,
		     cancel_requested_at = CASE WHEN status = 'processing' THEN COALESCE(cancel_requested_at, NOW()) ELSE NULL END,
		     updated_at = NOW()
		 WHERE workspace_id = $1::uuid AND id = $2::uuid AND status IN ('queued', 'processing')
		 RETURNING `+taskColumns,
		workspaceID, id,
	))
	if err == nil {
		return task, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return TaskRecord{}, err
	}
	_, lookupErr := s.Get(ctx, workspaceID, id)
	if errors.Is(lookupErr, ErrTaskNotFound) {
		return TaskRecord{}, ErrTaskNotFound
	}
	if lookupErr != nil {
		return TaskRecord{}, lookupErr
	}
	return TaskRecord{}, ErrTaskNotCancelable
}

func (s *PostgresTaskStore) FinalizeCancelled(ctx context.Context, id, leaseToken string) error {
	commandTag, err := s.pool.Exec(ctx,
		`UPDATE public.tasks
		 SET status = 'cancelled', result = NULL, claimed_at = NULL,
		     lease_expires_at = NULL, lease_token = NULL,
		     cancel_requested_at = COALESCE(cancel_requested_at, NOW()), updated_at = NOW()
		 WHERE id = $1::uuid AND lease_token = $2::uuid AND status = 'processing'`,
		id, leaseToken,
	)
	if err != nil {
		return err
	}
	if commandTag.RowsAffected() != 1 {
		return ErrTaskLeaseLost
	}
	return nil
}

func (s *PostgresTaskStore) Delete(ctx context.Context, workspaceID, id string) error {
	commandTag, err := s.pool.Exec(ctx,
		`DELETE FROM public.tasks WHERE workspace_id = $1::uuid AND id = $2::uuid
		 AND status IN ('completed', 'failed', 'cancelled')`,
		workspaceID, id,
	)
	if err != nil {
		return err
	}
	if commandTag.RowsAffected() == 1 {
		return nil
	}
	var status string
	err = s.pool.QueryRow(ctx,
		`SELECT status FROM public.tasks WHERE workspace_id = $1::uuid AND id = $2::uuid`,
		workspaceID, id,
	).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrTaskNotFound
	}
	if err != nil {
		return err
	}
	return ErrTaskActive
}

func (s *PostgresTaskStore) ClaimNext(ctx context.Context, leaseDuration time.Duration) (TaskRecord, error) {
	task, err := scanTask(s.pool.QueryRow(ctx,
		`WITH next_task AS (
			SELECT t.id AS task_id FROM public.tasks AS t
			WHERE t.status = 'queued' AND t.available_at <= NOW()
			ORDER BY t.available_at ASC, t.created_at ASC
			LIMIT 1 FOR UPDATE SKIP LOCKED
		)
		UPDATE public.tasks AS t
		SET status = 'processing', attempt_count = t.attempt_count + 1,
		    claimed_at = NOW(), lease_expires_at = NOW() + ($1 * INTERVAL '1 second'),
		    lease_token = gen_random_uuid(), cancel_requested_at = NULL, updated_at = NOW()
		FROM next_task WHERE t.id = next_task.task_id
		RETURNING `+taskColumns,
		int64(leaseDuration.Seconds()),
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return TaskRecord{}, ErrNoQueuedTask
	}
	return task, err
}

func (s *PostgresTaskStore) ClaimWebhookDelivery(ctx context.Context) (WebhookDelivery, error) {
	var delivery WebhookDelivery
	if _, err := s.pool.Exec(ctx,
		`UPDATE public.webhook_deliveries
		 SET status = 'failed', last_error = 'delivery lease expired at maximum attempts',
		     lease_expires_at = NULL, lease_token = NULL, updated_at = NOW()
		 WHERE status = 'processing' AND lease_expires_at < NOW() AND attempt_count >= max_attempts`,
	); err != nil {
		return WebhookDelivery{}, err
	}
	err := s.pool.QueryRow(ctx,
		`WITH next_delivery AS (
			SELECT id FROM public.webhook_deliveries
			WHERE (status = 'queued' AND available_at <= NOW())
			   OR (status = 'processing' AND lease_expires_at < NOW() AND attempt_count < max_attempts)
			ORDER BY available_at ASC, created_at ASC
			LIMIT 1 FOR UPDATE SKIP LOCKED
		)
		UPDATE public.webhook_deliveries AS d
		SET status = 'processing', attempt_count = d.attempt_count + 1,
		    lease_expires_at = NOW() + INTERVAL '2 minutes', lease_token = gen_random_uuid(), updated_at = NOW()
		FROM next_delivery WHERE d.id = next_delivery.id
		RETURNING d.id::text, d.task_id::text, d.lease_token::text, d.event, d.target_url, d.secret_ciphertext,
		          d.payload::text, d.attempt_count, d.max_attempts`,
	).Scan(
		&delivery.ID,
		&delivery.TaskID,
		&delivery.LeaseToken,
		&delivery.Event,
		&delivery.TargetURL,
		&delivery.SecretCiphertext,
		&delivery.Payload,
		&delivery.AttemptCount,
		&delivery.MaxAttempts,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return WebhookDelivery{}, ErrNoWebhookDelivery
	}
	return delivery, err
}

func (s *PostgresTaskStore) CompleteWebhookDelivery(ctx context.Context, id, leaseToken string, statusCode int) error {
	commandTag, err := s.pool.Exec(ctx,
		`UPDATE public.webhook_deliveries
		 SET status = 'delivered', last_status = $3, last_error = NULL,
		     lease_expires_at = NULL, lease_token = NULL, delivered_at = NOW(), updated_at = NOW()
		 WHERE id = $1::uuid AND lease_token = $2::uuid AND status = 'processing'`,
		id, leaseToken, statusCode,
	)
	if err != nil {
		return err
	}
	if commandTag.RowsAffected() != 1 {
		return errors.New("webhook delivery lease was lost")
	}
	return nil
}

func (s *PostgresTaskStore) FailWebhookDelivery(ctx context.Context, id, leaseToken, message string, statusCode int, retryable bool) (string, error) {
	var status string
	err := s.pool.QueryRow(ctx,
		`UPDATE public.webhook_deliveries
		 SET status = CASE WHEN $5 AND attempt_count < max_attempts THEN 'queued' ELSE 'failed' END,
		     available_at = CASE
		       WHEN $5 AND attempt_count < max_attempts
		       THEN NOW() + LEAST(INTERVAL '1 hour', INTERVAL '5 seconds' * POWER(2, GREATEST(attempt_count - 1, 0)))
		       ELSE available_at
		     END,
		     last_status = NULLIF($4, 0), last_error = $3,
		     lease_expires_at = NULL, lease_token = NULL, updated_at = NOW()
		 WHERE id = $1::uuid AND lease_token = $2::uuid AND status = 'processing'
		 RETURNING status`,
		id, leaseToken, message, statusCode, retryable,
	).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", errors.New("webhook delivery lease was lost")
	}
	return status, err
}

func (s *PostgresTaskStore) Heartbeat(ctx context.Context, id, leaseToken string, leaseDuration time.Duration) (bool, bool, error) {
	var renewed bool
	err := s.pool.QueryRow(ctx,
		`UPDATE public.tasks SET lease_expires_at = NOW() + ($3::double precision * INTERVAL '1 second')
		 WHERE id = $1::uuid AND lease_token = $2::uuid
		   AND status = 'processing' AND cancel_requested_at IS NULL
		 RETURNING TRUE`,
		id, leaseToken, leaseDuration.Seconds(),
	).Scan(&renewed)
	if err == nil {
		return renewed, false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, false, err
	}
	var status string
	var cancelRequested bool
	err = s.pool.QueryRow(ctx,
		`SELECT status, cancel_requested_at IS NOT NULL FROM public.tasks
		 WHERE id = $1::uuid AND lease_token = $2::uuid`,
		id, leaseToken,
	).Scan(&status, &cancelRequested)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	return status == "processing", cancelRequested, nil
}

func (s *PostgresTaskStore) Complete(ctx context.Context, id, leaseToken, result string) (string, error) {
	var status string
	err := s.pool.QueryRow(ctx,
		`UPDATE public.tasks
		 SET status = CASE WHEN cancel_requested_at IS NOT NULL THEN 'cancelled' ELSE 'completed' END,
		     result = CASE WHEN cancel_requested_at IS NOT NULL THEN NULL ELSE $3 END,
		     error_message = NULL, claimed_at = NULL, lease_expires_at = NULL,
		     lease_token = NULL, updated_at = NOW()
		 WHERE id = $1::uuid AND lease_token = $2::uuid AND status = 'processing'
		 RETURNING status`,
		id, leaseToken, result,
	).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrTaskLeaseLost
	}
	return status, err
}

func (s *PostgresTaskStore) Fail(ctx context.Context, id, leaseToken, message string, retryable bool) (string, error) {
	var status string
	err := s.pool.QueryRow(ctx,
		`UPDATE public.tasks
		 SET status = CASE
		       WHEN cancel_requested_at IS NOT NULL THEN 'cancelled'
		       WHEN $4 AND attempt_count < max_attempts THEN 'queued'
		       ELSE 'failed'
		     END,
		     result = NULL, error_message = $3,
		     available_at = CASE
		       WHEN cancel_requested_at IS NULL AND $4 AND attempt_count < max_attempts
		       THEN NOW() + LEAST(INTERVAL '60 seconds', INTERVAL '2 seconds' * POWER(2, GREATEST(attempt_count - 1, 0)))
		       ELSE available_at
		     END,
		     claimed_at = NULL, lease_expires_at = NULL, lease_token = NULL, updated_at = NOW()
		 WHERE id = $1::uuid AND lease_token = $2::uuid AND status = 'processing'
		 RETURNING status`,
		id, leaseToken, message, retryable,
	).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrTaskLeaseLost
	}
	return status, err
}

func (s *PostgresTaskStore) RecoverStale(ctx context.Context) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE public.tasks
		 SET status = CASE
		       WHEN cancel_requested_at IS NOT NULL THEN 'cancelled'
		       WHEN attempt_count >= max_attempts THEN 'failed'
		       ELSE 'queued'
		     END,
		     error_message = CASE
		       WHEN cancel_requested_at IS NOT NULL THEN 'task cancellation completed after worker lease expired'
		       WHEN attempt_count >= max_attempts THEN 'worker lease expired; maximum attempts reached'
		       ELSE 'worker lease expired; task was requeued'
		     END,
		     available_at = CASE WHEN cancel_requested_at IS NULL AND attempt_count < max_attempts THEN NOW() ELSE available_at END,
		     claimed_at = NULL, lease_expires_at = NULL, lease_token = NULL, updated_at = NOW()
		 WHERE status = 'processing' AND lease_expires_at < NOW()`,
	)
	return err
}

func (s *PostgresTaskStore) DeleteExpired(ctx context.Context, retention time.Duration) (int64, error) {
	commandTag, err := s.pool.Exec(ctx,
		`DELETE FROM public.tasks
		 WHERE status IN ('completed', 'failed', 'cancelled')
		   AND updated_at < NOW() - ($1::double precision * INTERVAL '1 second')`,
		int64(retention.Seconds()),
	)
	if err != nil {
		return 0, err
	}
	deletedTasks := commandTag.RowsAffected()
	_, err = s.pool.Exec(ctx,
		`DELETE FROM public.webhook_deliveries
		 WHERE status IN ('delivered', 'failed')
		   AND updated_at < NOW() - ($1::double precision * INTERVAL '1 second')`,
		int64(retention.Seconds()),
	)
	return deletedTasks, err
}

func (s *PostgresTaskStore) QueueStats(ctx context.Context, workspaceID string) (map[string]int64, error) {
	rows, err := s.pool.Query(ctx, `SELECT status, COUNT(*) FROM public.tasks WHERE workspace_id = $1::uuid GROUP BY status`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := map[string]int64{"queued": 0, "processing": 0, "completed": 0, "failed": 0, "cancelled": 0}
	for rows.Next() {
		var status string
		var count int64
		if err := rows.Scan(&status, &count); err != nil {
			return nil, err
		}
		counts[status] = count
	}
	return counts, rows.Err()
}

func parseTaskCursor(value string) (*TaskCursor, error) { return decodeTaskCursor(value) }
