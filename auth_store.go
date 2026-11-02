package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

var (
	ErrInvalidAPIKey   = errors.New("invalid API key")
	ErrAPIKeyNotFound  = errors.New("API key not found")
	ErrLastAdminKey    = errors.New("cannot revoke the last active admin key")
	ErrScopeEscalation = errors.New("API key cannot revoke another key with higher scopes")
)

const (
	ScopeAdmin       = "admin"
	ScopeTaskRead    = "tasks:read"
	ScopeTaskWrite   = "tasks:write"
	ScopeTaskManage  = "tasks:manage"
	ScopeTaskDelete  = "tasks:delete"
	ScopeKeysManage  = "keys:manage"
	ScopeMetricsRead = "metrics:read"
)

var allScopes = []string{
	ScopeTaskRead,
	ScopeTaskWrite,
	ScopeTaskManage,
	ScopeTaskDelete,
	ScopeKeysManage,
	ScopeMetricsRead,
}

type Principal struct {
	KeyID       string
	WorkspaceID string
	KeyName     string
	Scopes      map[string]struct{}
}

func (p Principal) HasScope(scope string) bool {
	if _, ok := p.Scopes[ScopeAdmin]; ok {
		return true
	}
	_, ok := p.Scopes[scope]
	return ok
}

func (p Principal) CanGrant(scope string) bool {
	if _, ok := p.Scopes[ScopeAdmin]; ok {
		return true
	}
	return p.HasScope(scope) && scope != ScopeAdmin
}

type APIKeyRecord struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Prefix    string     `json:"prefix"`
	Scopes    []string   `json:"scopes"`
	CreatedAt time.Time  `json:"created_at"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
}

type WorkspaceRecord struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

type CredentialStore interface {
	Authenticate(ctx context.Context, token string) (Principal, error)
	CreateAPIKey(ctx context.Context, workspaceID, name string, scopes []string) (APIKeyRecord, string, error)
	ListAPIKeys(ctx context.Context, workspaceID string) ([]APIKeyRecord, error)
	RevokeAPIKey(ctx context.Context, workspaceID, id string, principal Principal) error
	CreateWorkspaceWithAdminKey(ctx context.Context, name, keyName string) (WorkspaceRecord, APIKeyRecord, string, error)
}

func digestAPIKey(token string) []byte {
	digest := sha256.Sum256([]byte(token))
	return digest[:]
}

func (s *PostgresTaskStore) EnsureBootstrapKey(ctx context.Context, token string) error {
	if token == "" {
		return errors.New("bootstrap API key is required")
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO public.api_keys (workspace_id, name, key_prefix, key_hash, scopes)
		 VALUES ($1::uuid, 'Bootstrap admin', $2, $3, ARRAY['admin']::text[])
		 ON CONFLICT (key_hash) DO NOTHING`,
		defaultWorkspaceID, apiKeyPrefix(token), digestAPIKey(token),
	)
	return err
}

func (s *PostgresTaskStore) Authenticate(ctx context.Context, token string) (Principal, error) {
	var principal Principal
	var scopes []string
	err := s.pool.QueryRow(ctx,
		`SELECT id::text, workspace_id::text, name, scopes
		 FROM public.api_keys WHERE key_hash = $1 AND revoked_at IS NULL`,
		digestAPIKey(token),
	).Scan(&principal.KeyID, &principal.WorkspaceID, &principal.KeyName, &scopes)
	if errors.Is(err, pgx.ErrNoRows) {
		return Principal{}, ErrInvalidAPIKey
	}
	if err != nil {
		return Principal{}, err
	}
	principal.Scopes = make(map[string]struct{}, len(scopes))
	for _, scope := range scopes {
		principal.Scopes[scope] = struct{}{}
	}
	return principal, nil
}

func (s *PostgresTaskStore) CreateAPIKey(ctx context.Context, workspaceID, name string, scopes []string) (APIKeyRecord, string, error) {
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return APIKeyRecord{}, "", fmt.Errorf("generate API key: %w", err)
	}
	token := "mr_" + hex.EncodeToString(secret[:])
	var record APIKeyRecord
	err := s.pool.QueryRow(ctx,
		`INSERT INTO public.api_keys (workspace_id, name, key_prefix, key_hash, scopes)
		 VALUES ($1::uuid, $2, $3, $4, $5)
		 RETURNING id::text, name, key_prefix, scopes, created_at`,
		workspaceID, name, apiKeyPrefix(token), digestAPIKey(token), scopes,
	).Scan(&record.ID, &record.Name, &record.Prefix, &record.Scopes, &record.CreatedAt)
	if err != nil {
		return APIKeyRecord{}, "", err
	}
	return record, token, nil
}

func (s *PostgresTaskStore) CreateWorkspaceWithAdminKey(ctx context.Context, name, keyName string) (WorkspaceRecord, APIKeyRecord, string, error) {
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return WorkspaceRecord{}, APIKeyRecord{}, "", fmt.Errorf("generate workspace key: %w", err)
	}
	token := "mr_" + hex.EncodeToString(secret[:])
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return WorkspaceRecord{}, APIKeyRecord{}, "", err
	}
	defer tx.Rollback(ctx)

	var workspace WorkspaceRecord
	if err := tx.QueryRow(ctx,
		`INSERT INTO public.workspaces (name) VALUES ($1) RETURNING id::text, name, created_at`, name,
	).Scan(&workspace.ID, &workspace.Name, &workspace.CreatedAt); err != nil {
		return WorkspaceRecord{}, APIKeyRecord{}, "", err
	}
	var key APIKeyRecord
	if err := tx.QueryRow(ctx,
		`INSERT INTO public.api_keys (workspace_id, name, key_prefix, key_hash, scopes)
		 VALUES ($1::uuid, $2, $3, $4, ARRAY['admin']::text[])
		 RETURNING id::text, name, key_prefix, scopes, created_at`,
		workspace.ID, keyName, apiKeyPrefix(token), digestAPIKey(token),
	).Scan(&key.ID, &key.Name, &key.Prefix, &key.Scopes, &key.CreatedAt); err != nil {
		return WorkspaceRecord{}, APIKeyRecord{}, "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return WorkspaceRecord{}, APIKeyRecord{}, "", err
	}
	return workspace, key, token, nil
}

func (s *PostgresTaskStore) ListAPIKeys(ctx context.Context, workspaceID string) ([]APIKeyRecord, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id::text, name, key_prefix, scopes, created_at, revoked_at
		 FROM public.api_keys WHERE workspace_id = $1::uuid
		 ORDER BY created_at DESC, id DESC`, workspaceID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	keys := make([]APIKeyRecord, 0)
	for rows.Next() {
		var record APIKeyRecord
		var revokedAt *time.Time
		if err := rows.Scan(&record.ID, &record.Name, &record.Prefix, &record.Scopes, &record.CreatedAt, &revokedAt); err != nil {
			return nil, err
		}
		record.RevokedAt = revokedAt
		keys = append(keys, record)
	}
	return keys, rows.Err()
}

func (s *PostgresTaskStore) RevokeAPIKey(ctx context.Context, workspaceID, id string, principal Principal) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var scopes []string
	var revokedAt *time.Time
	err = tx.QueryRow(ctx,
		`SELECT scopes, revoked_at FROM public.api_keys
		 WHERE workspace_id = $1::uuid AND id = $2::uuid FOR UPDATE`,
		workspaceID, id,
	).Scan(&scopes, &revokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrAPIKeyNotFound
	}
	if err != nil {
		return err
	}
	if revokedAt != nil {
		return tx.Commit(ctx)
	}
	for _, scope := range scopes {
		if !principal.CanGrant(scope) {
			return ErrScopeEscalation
		}
	}

	for _, scope := range scopes {
		if scope != ScopeAdmin {
			continue
		}
		rows, err := tx.Query(ctx,
			`SELECT id FROM public.api_keys
			 WHERE workspace_id = $1::uuid AND revoked_at IS NULL AND 'admin' = ANY(scopes)
			 ORDER BY id FOR UPDATE`,
			workspaceID,
		)
		if err != nil {
			return err
		}
		activeAdmins := 0
		for rows.Next() {
			var adminID string
			if err := rows.Scan(&adminID); err != nil {
				rows.Close()
				return err
			}
			activeAdmins++
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		if activeAdmins <= 1 {
			return ErrLastAdminKey
		}
	}

	if _, err := tx.Exec(ctx,
		`UPDATE public.api_keys SET revoked_at = NOW()
		 WHERE workspace_id = $1::uuid AND id = $2::uuid AND revoked_at IS NULL`,
		workspaceID, id,
	); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func apiKeyPrefix(token string) string {
	if len(token) <= 12 {
		return token
	}
	return token[:12]
}
