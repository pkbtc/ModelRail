package main

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrTaskNotFound is returned when a task ID does not exist in the database.
var ErrTaskNotFound = errors.New("task not found")

// ErrNoQueuedTask is returned when the queue contains no pending tasks.
var ErrNoQueuedTask = errors.New("no queued task")

// TaskRecord represents a complete task row returned from the database.
type TaskRecord struct {
	ID           string    `json:"id"`
	Task         string    `json:"task"`
	Input        string    `json:"input"`
	Provider     string    `json:"provider"`
	Status       string    `json:"status"`
	AttemptCount int       `json:"attempt_count"`
	MaxAttempts  int       `json:"max_attempts"`
	Result       *string   `json:"result"`
	ErrorMessage *string   `json:"error_message"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// TaskStore defines the operations needed to save and retrieve tasks.
type TaskStore interface {
	Create(
		ctx context.Context,
		task string,
		input string,
		provider string,
	) (string, error)

	Complete(
		ctx context.Context,
		id string,
		result string,
	) error

	Fail(
		ctx context.Context,
		id string,
		message string,
	) error

	Get(ctx context.Context, id string) (TaskRecord, error)

	// ClaimNext atomically claims the oldest queued task for processing.
	ClaimNext(ctx context.Context) (TaskRecord, error)

	// RecoverStale recovers tasks that have been in processing status past their lease duration.
	RecoverStale(ctx context.Context) error
}

// PostgresTaskStore implements TaskStore using PostgreSQL.
type PostgresTaskStore struct {
	pool *pgxpool.Pool
}

// Create inserts a new task in queued status and returns its generated UUID.
func (s *PostgresTaskStore) Create(
	ctx context.Context,
	task string,
	input string,
	provider string,
) (string, error) {
	var id string

	err := s.pool.QueryRow(
		ctx,
		`INSERT INTO public.tasks (task, input, provider, status)
		 VALUES ($1, $2, $3, 'queued')
		 RETURNING id::text;`,
		task,
		input,
		provider,
	).Scan(&id)
	if err != nil {
		return "", err
	}

	return id, nil
}

// Complete saves the result and marks the task as completed.
func (s *PostgresTaskStore) Complete(
	ctx context.Context,
	id string,
	result string,
) error {
	commandTag, err := s.pool.Exec(
		ctx,
		`UPDATE public.tasks
         SET status = 'completed',
             result = $2,
             error_message = NULL,
             claimed_at = NULL,
             updated_at = NOW()
         WHERE id = $1::uuid
           AND status = 'processing'`,
		id,
		result,
	)
	if err != nil {
		return err
	}

	if commandTag.RowsAffected() != 1 {
		return errors.New("task not found or not processing")
	}

	return nil
}

// Fail saves the error and marks the task as failed or retries if attempts remain.
func (s *PostgresTaskStore) Fail(
	ctx context.Context,
	id string,
	message string,
) error {
	commandTag, err := s.pool.Exec(
		ctx,
		`UPDATE public.tasks
         SET status = CASE
                 WHEN attempt_count < max_attempts THEN 'queued'
                 ELSE 'failed'
             END,
             result = NULL,
             error_message = $2,
             available_at = CASE
                 WHEN attempt_count < max_attempts THEN
                     NOW() + LEAST(
                         INTERVAL '60 seconds',
                         INTERVAL '2 seconds'
                             * POWER(2, GREATEST(attempt_count - 1, 0))
                     )
                 ELSE available_at
             END,
             claimed_at = NULL,
             updated_at = NOW()
         WHERE id = $1::uuid
           AND status = 'processing'`,
		id,
		message,
	)
	if err != nil {
		return err
	}

	if commandTag.RowsAffected() != 1 {
		return errors.New("task not found or not processing")
	}

	return nil
}

// Get retrieves a single task by its UUID.
func (s *PostgresTaskStore) Get(
	ctx context.Context,
	id string,
) (TaskRecord, error) {
	var task TaskRecord

	var result sql.NullString
	var errorMessage sql.NullString

	err := s.pool.QueryRow(
		ctx,
		`SELECT
			id::text,
			task,
			input,
			provider,
			status,
			attempt_count,
			max_attempts,
			result,
			error_message,
			created_at,
			updated_at
		FROM public.tasks
		WHERE id = $1::uuid`,
		id,
	).Scan(
		&task.ID,
		&task.Task,
		&task.Input,
		&task.Provider,
		&task.Status,
		&task.AttemptCount,
		&task.MaxAttempts,
		&result,
		&errorMessage,
		&task.CreatedAt,
		&task.UpdatedAt,
	)

	if errors.Is(err, pgx.ErrNoRows) {
		return TaskRecord{}, ErrTaskNotFound
	}

	if err != nil {
		return TaskRecord{}, err
	}

	if result.Valid {
		task.Result = &result.String
	}

	if errorMessage.Valid {
		task.ErrorMessage = &errorMessage.String
	}

	return task, nil
}

// ClaimNext atomically claims the oldest queued task for processing.
// It uses FOR UPDATE SKIP LOCKED so concurrent workers claim different tasks.
func (s *PostgresTaskStore) ClaimNext(
	ctx context.Context,
) (TaskRecord, error) {
	var task TaskRecord
	var result sql.NullString
	var errorMessage sql.NullString

	err := s.pool.QueryRow(
		ctx,
		`WITH next_task AS (
			SELECT id
			FROM public.tasks
			WHERE status = 'queued'
			  AND available_at <= NOW()
			ORDER BY created_at ASC
			LIMIT 1
			FOR UPDATE SKIP LOCKED
		)
		UPDATE public.tasks AS t
		SET status = 'processing',
		    attempt_count = t.attempt_count + 1,
		    claimed_at = NOW(),
		    updated_at = NOW()
		FROM next_task
		WHERE t.id = next_task.id
		RETURNING
			t.id::text,
			t.task,
			t.input,
			t.provider,
			t.status,
			t.attempt_count,
			t.max_attempts,
			t.result,
			t.error_message,
			t.created_at,
			t.updated_at`,
	).Scan(
		&task.ID,
		&task.Task,
		&task.Input,
		&task.Provider,
		&task.Status,
		&task.AttemptCount,
		&task.MaxAttempts,
		&result,
		&errorMessage,
		&task.CreatedAt,
		&task.UpdatedAt,
	)

	if errors.Is(err, pgx.ErrNoRows) {
		return TaskRecord{}, ErrNoQueuedTask
	}

	if err != nil {
		return TaskRecord{}, err
	}

	if result.Valid {
		task.Result = &result.String
	}

	if errorMessage.Valid {
		task.ErrorMessage = &errorMessage.String
	}

	return task, nil
}

// RecoverStale recovers tasks that have been in processing status past their lease duration.
func (s *PostgresTaskStore) RecoverStale(
	ctx context.Context,
) error {
	_, err := s.pool.Exec(
		ctx,
		`UPDATE public.tasks
         SET status = CASE
                 WHEN attempt_count >= max_attempts THEN 'failed'
                 ELSE 'queued'
             END,
             error_message = CASE
                 WHEN attempt_count >= max_attempts
                     THEN 'worker lease expired; maximum attempts reached'
                 ELSE 'worker lease expired; task was requeued'
             END,
             available_at = CASE
                 WHEN attempt_count < max_attempts THEN NOW()
                 ELSE available_at
             END,
             claimed_at = NULL,
             updated_at = NOW()
         WHERE status = 'processing'
           AND claimed_at IS NOT NULL
           AND claimed_at < NOW() - INTERVAL '2 minutes'`,
	)

	return err
}
