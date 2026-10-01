// Package postgres provides an optional PostgreSQL-backed storage.Store using
// standard library database/sql.
package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const schemaSQL = `
CREATE TABLE IF NOT EXISTS bot_kv_store (
    namespace  TEXT NOT NULL,
    key        TEXT NOT NULL,
    payload    BYTEA NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (namespace, key)
);
`

// Store persists documents in a PostgreSQL table via database/sql.
type Store struct {
	db *sql.DB
}

// New opens a PostgreSQL connection using the provided driverName (default
// "postgres") and dsn, and ensures the key-value table exists.
func New(driverName, dsn string) (*Store, error) {
	dsn = strings.TrimSpace(dsn)
	if dsn == "" {
		return nil, errors.New("postgres DSN is empty")
	}
	if strings.TrimSpace(driverName) == "" {
		driverName = "postgres"
	}

	db, err := sql.Open(driverName, dsn)
	if err != nil {
		return nil, fmt.Errorf("opening postgres connection: %w", err)
	}

	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(30 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("pinging postgres: %w", err)
	}

	if _, err := db.ExecContext(ctx, schemaSQL); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("initializing postgres schema: %w", err)
	}

	return &Store{db: db}, nil
}

func (s *Store) Type() string { return "postgres" }

func (s *Store) Save(ctx context.Context, namespace, key string, data []byte) error {
	if s == nil || s.db == nil {
		return errors.New("postgres store is not initialized")
	}
	const q = `
INSERT INTO bot_kv_store (namespace, key, payload, updated_at)
VALUES ($1, $2, $3, NOW())
ON CONFLICT (namespace, key)
DO UPDATE SET payload = EXCLUDED.payload, updated_at = NOW();
`
	_, err := s.db.ExecContext(ctx, q, namespace, key, data)
	return err
}

func (s *Store) Load(ctx context.Context, namespace, key string) ([]byte, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("postgres store is not initialized")
	}
	const q = `SELECT payload FROM bot_kv_store WHERE namespace = $1 AND key = $2;`
	var payload []byte
	err := s.db.QueryRowContext(ctx, q, namespace, key).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return payload, err
}

func (s *Store) Delete(ctx context.Context, namespace, key string) error {
	if s == nil || s.db == nil {
		return errors.New("postgres store is not initialized")
	}
	const q = `DELETE FROM bot_kv_store WHERE namespace = $1 AND key = $2;`
	_, err := s.db.ExecContext(ctx, q, namespace, key)
	return err
}

func (s *Store) ListKeys(ctx context.Context, namespace string) ([]string, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("postgres store is not initialized")
	}
	const q = `SELECT key FROM bot_kv_store WHERE namespace = $1 ORDER BY key;`
	rows, err := s.db.QueryContext(ctx, q, namespace)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var keys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}
