CREATE TABLE users (
    id                  INTEGER PRIMARY KEY,
    username            TEXT    NOT NULL UNIQUE COLLATE NOCASE,
    password_hash       TEXT    NOT NULL,
    created_at          TEXT    NOT NULL,
    password_changed_at TEXT    NOT NULL
);

CREATE TABLE sessions (
    token_hash   TEXT    PRIMARY KEY,
    user_id      INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    csrf_token   TEXT    NOT NULL,
    created_at   TEXT    NOT NULL,
    expires_at   TEXT    NOT NULL,
    last_seen_at TEXT    NOT NULL
);

CREATE INDEX sessions_expires_at ON sessions (expires_at);

CREATE TABLE settings (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

-- Cached interactive-search results. Checking releases hits every indexer,
-- so results are kept and shown with their age rather than re-fetched.
CREATE TABLE release_checks (
    kind          TEXT    NOT NULL,
    item_key      INTEGER NOT NULL,
    checked_at    TEXT    NOT NULL,
    file_id       INTEGER NOT NULL,
    available     INTEGER NOT NULL,
    best_title    TEXT    NOT NULL DEFAULT '',
    best_score    INTEGER NOT NULL DEFAULT 0,
    best_quality  TEXT    NOT NULL DEFAULT '',
    best_size     INTEGER NOT NULL DEFAULT 0,
    total         INTEGER NOT NULL DEFAULT 0,
    rejected      INTEGER NOT NULL DEFAULT 0,
    top_rejection TEXT    NOT NULL DEFAULT '',
    error         TEXT    NOT NULL DEFAULT '',
    PRIMARY KEY (kind, item_key)
);

-- Everything this tool does to the library, in one chronological ledger.
CREATE TABLE audit_log (
    id       INTEGER PRIMARY KEY AUTOINCREMENT,
    ts       TEXT    NOT NULL,
    username TEXT    NOT NULL,
    action   TEXT    NOT NULL,
    service  TEXT    NOT NULL,
    item_id  INTEGER NOT NULL,
    title    TEXT    NOT NULL,
    outcome  TEXT    NOT NULL,
    message  TEXT    NOT NULL DEFAULT '',
    details  TEXT    NOT NULL DEFAULT '{}'
);

CREATE INDEX audit_log_ts ON audit_log (ts);
