package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// IRTTKey is the shared HMAC secret for one irtt endpoint ("host:port" as the
// target is configured). Key is the text as the operator would pass it to
// `irtt server --hmac`; the probe package decodes it.
type IRTTKey struct {
	Endpoint  string
	Key       string
	UpdatedAt int64
	UpdatedBy string
}

// ListIRTTKeys returns every key, for the prober.
func (s *SQLite) ListIRTTKeys(ctx context.Context) ([]IRTTKey, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT endpoint, key, updated_at, updated_by FROM irtt_keys ORDER BY endpoint")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []IRTTKey
	for rows.Next() {
		var k IRTTKey
		if err := rows.Scan(&k.Endpoint, &k.Key, &k.UpdatedAt, &k.UpdatedBy); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// GetIRTTKey returns one key and whether it exists.
func (s *SQLite) GetIRTTKey(ctx context.Context, endpoint string) (IRTTKey, bool, error) {
	var k IRTTKey
	err := s.db.QueryRowContext(ctx,
		"SELECT endpoint, key, updated_at, updated_by FROM irtt_keys WHERE endpoint = ?", endpoint).
		Scan(&k.Endpoint, &k.Key, &k.UpdatedAt, &k.UpdatedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return IRTTKey{}, false, nil
	}
	return k, err == nil, err
}

// PutIRTTKey sets or replaces the key for an endpoint.
func (s *SQLite) PutIRTTKey(ctx context.Context, endpoint, key, by string, now time.Time) error {
	if endpoint == "" || key == "" {
		return fmt.Errorf("store: an irtt key needs an endpoint and a key")
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO irtt_keys (endpoint, key, updated_at, updated_by) VALUES (?, ?, ?, ?)
		ON CONFLICT (endpoint) DO UPDATE SET
		  key = excluded.key, updated_at = excluded.updated_at, updated_by = excluded.updated_by`,
		endpoint, key, now.Unix(), by)
	return err
}

// DeleteIRTTKey removes the key for an endpoint. Removing none is not an error.
func (s *SQLite) DeleteIRTTKey(ctx context.Context, endpoint string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM irtt_keys WHERE endpoint = ?", endpoint)
	return err
}
