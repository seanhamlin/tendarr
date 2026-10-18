package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Session is a signed-in browser.
type Session struct {
	TokenHash string
	UserID    int64
	Username  string
	CSRFToken string
	ExpiresAt time.Time
}

// CreateSession stores a session. Only the hash of the token is kept, so a
// copy of the database does not hand out live sessions.
func (s *Store) CreateSession(ctx context.Context, tokenHash string, userID int64, csrf string, ttl time.Duration) error {
	now := s.now()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions (token_hash, user_id, csrf_token, created_at, expires_at, last_seen_at) VALUES (?, ?, ?, ?, ?, ?)`,
		tokenHash, userID, csrf, now.Format(time.RFC3339Nano), now.Add(ttl).Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
	return err
}

// SessionByHash returns an unexpired session.
func (s *Store) SessionByHash(ctx context.Context, tokenHash string) (Session, error) {
	var sess Session
	var expires string
	err := s.db.QueryRowContext(ctx,
		`SELECT s.token_hash, s.user_id, u.username, s.csrf_token, s.expires_at
		   FROM sessions s JOIN users u ON u.id = s.user_id
		  WHERE s.token_hash = ? AND s.expires_at > ?`,
		tokenHash, s.ts()).Scan(&sess.TokenHash, &sess.UserID, &sess.Username, &sess.CSRFToken, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	if err != nil {
		return Session{}, err
	}
	sess.ExpiresAt = parseTS(expires)
	return sess, nil
}

// TouchSession slides the expiry forward.
func (s *Store) TouchSession(ctx context.Context, tokenHash string, ttl time.Duration) error {
	now := s.now()
	_, err := s.db.ExecContext(ctx, `UPDATE sessions SET last_seen_at = ?, expires_at = ? WHERE token_hash = ?`,
		now.Format(time.RFC3339Nano), now.Add(ttl).Format(time.RFC3339Nano), tokenHash)
	return err
}

// DeleteSession signs a browser out.
func (s *Store) DeleteSession(ctx context.Context, tokenHash string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, tokenHash)
	return err
}

// PurgeExpiredSessions removes sessions past their expiry.
func (s *Store) PurgeExpiredSessions(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= ?`, s.ts())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
