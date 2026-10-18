package store

import (
	"context"
	"encoding/json"
	"time"
)

// Settings returns every stored setting.
func (s *Store) Settings(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// SaveSettings upserts several settings in one transaction.
func (s *Store) SaveSettings(ctx context.Context, values map[string]string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	now := s.ts()
	for k, v := range values {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
			 ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
			k, v, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ReleaseCheck is a cached interactive-search summary.
type ReleaseCheck struct {
	Kind         string
	ItemKey      int
	FileID       int
	CheckedAt    time.Time
	Available    bool
	BestTitle    string
	BestScore    int
	BestQuality  string
	BestSize     int64
	Total        int
	Rejected     int
	TopRejection string
	Error        string
}

// SaveReleaseCheck stores or replaces a result.
func (s *Store) SaveReleaseCheck(ctx context.Context, rc ReleaseCheck) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO release_checks (kind, item_key, file_id, checked_at, available, best_title, best_score, best_quality, best_size, total, rejected, top_rejection, error)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (kind, item_key) DO UPDATE SET
		   file_id = excluded.file_id, checked_at = excluded.checked_at, available = excluded.available,
		   best_title = excluded.best_title, best_score = excluded.best_score, best_quality = excluded.best_quality,
		   best_size = excluded.best_size, total = excluded.total, rejected = excluded.rejected,
		   top_rejection = excluded.top_rejection, error = excluded.error`,
		rc.Kind, rc.ItemKey, rc.FileID, rc.CheckedAt.UTC().Format(time.RFC3339Nano), rc.Available, rc.BestTitle, rc.BestScore,
		rc.BestQuality, rc.BestSize, rc.Total, rc.Rejected, rc.TopRejection, rc.Error)
	return err
}

// ReleaseChecks returns every cached result for a kind, keyed by item key.
func (s *Store) ReleaseChecks(ctx context.Context, kind string) (map[int]ReleaseCheck, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT kind, item_key, file_id, checked_at, available, best_title, best_score, best_quality, best_size, total, rejected, top_rejection, error
		   FROM release_checks WHERE kind = ?`, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[int]ReleaseCheck{}
	for rows.Next() {
		var rc ReleaseCheck
		var checked string
		if err := rows.Scan(&rc.Kind, &rc.ItemKey, &rc.FileID, &checked, &rc.Available, &rc.BestTitle, &rc.BestScore,
			&rc.BestQuality, &rc.BestSize, &rc.Total, &rc.Rejected, &rc.TopRejection, &rc.Error); err != nil {
			return nil, err
		}
		rc.CheckedAt = parseTS(checked)
		out[rc.ItemKey] = rc
	}
	return out, rows.Err()
}

// DeleteReleaseChecks forgets results, e.g. after a search was queued and the
// file is about to change.
func (s *Store) DeleteReleaseChecks(ctx context.Context, kind string, keys []int) error {
	for _, k := range keys {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM release_checks WHERE kind = ? AND item_key = ?`, kind, k); err != nil {
			return err
		}
	}
	return nil
}

// AuditEntry is one action against the library.
type AuditEntry struct {
	ID       int64
	TS       time.Time
	Username string
	Action   string
	Service  string
	ItemID   int
	Title    string
	Outcome  string
	Message  string
	Details  map[string]any
}

// AddAudit appends an entry.
func (s *Store) AddAudit(ctx context.Context, e AuditEntry) error {
	details := []byte("{}")
	if e.Details != nil {
		var err error
		if details, err = json.Marshal(e.Details); err != nil {
			return err
		}
	}
	ts := e.TS
	if ts.IsZero() {
		ts = s.now()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO audit_log (ts, username, action, service, item_id, title, outcome, message, details) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ts.UTC().Format(time.RFC3339Nano), e.Username, e.Action, e.Service, e.ItemID, e.Title, e.Outcome, e.Message, string(details))
	return err
}

// RecentAudit returns the newest entries first.
func (s *Store) RecentAudit(ctx context.Context, limit int) ([]AuditEntry, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, ts, username, action, service, item_id, title, outcome, message, details
		   FROM audit_log ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		var ts, details string
		if err := rows.Scan(&e.ID, &ts, &e.Username, &e.Action, &e.Service, &e.ItemID, &e.Title, &e.Outcome, &e.Message, &details); err != nil {
			return nil, err
		}
		e.TS = parseTS(ts)
		if err := json.Unmarshal([]byte(details), &e.Details); err != nil {
			e.Details = nil
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
