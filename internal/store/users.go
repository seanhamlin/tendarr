package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// User is an account that can sign in.
type User struct {
	ID                int64
	Username          string
	PasswordHash      string
	CreatedAt         time.Time
	PasswordChangedAt time.Time
}

// CountUsers returns how many accounts exist; zero means first-run setup.
func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

// CreateFirstUser creates the initial account, atomically refusing if any
// account already exists. Without the check inside one statement, two
// browsers racing through setup could both create an admin.
func (s *Store) CreateFirstUser(ctx context.Context, username, hash string) (User, error) {
	now := s.ts()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO users (username, password_hash, created_at, password_changed_at)
		 SELECT ?, ?, ?, ? WHERE NOT EXISTS (SELECT 1 FROM users)`,
		username, hash, now, now)
	if err != nil {
		return User{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return User{}, ErrSetupComplete
	}
	return s.UserByUsername(ctx, username)
}

// ErrSetupComplete is returned when setup is attempted after an account exists.
var ErrSetupComplete = errors.New("an account already exists")

// UserByUsername looks up an account, case-insensitively.
func (s *Store) UserByUsername(ctx context.Context, username string) (User, error) {
	return s.scanUser(s.db.QueryRowContext(ctx,
		`SELECT id, username, password_hash, created_at, password_changed_at FROM users WHERE username = ?`, username))
}

// UserByID looks up an account by id.
func (s *Store) UserByID(ctx context.Context, id int64) (User, error) {
	return s.scanUser(s.db.QueryRowContext(ctx,
		`SELECT id, username, password_hash, created_at, password_changed_at FROM users WHERE id = ?`, id))
}

func (s *Store) scanUser(row *sql.Row) (User, error) {
	var u User
	var created, changed string
	err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &created, &changed)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, err
	}
	u.CreatedAt, u.PasswordChangedAt = parseTS(created), parseTS(changed)
	return u, nil
}

// UpdatePassword replaces a password hash and revokes every other session,
// so a password change actually locks out whoever had the old one.
func (s *Store) UpdatePassword(ctx context.Context, userID int64, hash, keepSessionHash string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `UPDATE users SET password_hash = ?, password_changed_at = ? WHERE id = ?`, hash, s.ts(), userID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ? AND token_hash != ?`, userID, keepSessionHash); err != nil {
		return err
	}
	return tx.Commit()
}
