package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS users (
	id               INTEGER PRIMARY KEY,
	google_sub       TEXT NOT NULL UNIQUE,
	email            TEXT NOT NULL,
	name             TEXT NOT NULL DEFAULT '',
	picture          TEXT NOT NULL DEFAULT '',
	tz               TEXT NOT NULL DEFAULT '',
	default_filter   TEXT NOT NULL DEFAULT 'comments,reviews,commits,labels,state,other',
	default_delivery TEXT NOT NULL DEFAULT 'instant',
	quiet_start      INTEGER,              -- minutes after local midnight
	quiet_end        INTEGER,
	digest_hour      INTEGER NOT NULL DEFAULT 8,
	last_digest_day  TEXT NOT NULL DEFAULT '', -- YYYY-MM-DD in the user's tz
	api_token_hash   TEXT UNIQUE,
	created_at       INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS items (
	id              INTEGER PRIMARY KEY,
	owner           TEXT NOT NULL,
	repo            TEXT NOT NULL,
	number          INTEGER NOT NULL,
	kind            TEXT NOT NULL,         -- issue | pr
	title           TEXT NOT NULL DEFAULT '',
	state           TEXT NOT NULL DEFAULT 'open', -- open | closed | merged
	author          TEXT NOT NULL DEFAULT '',
	comments        INTEGER NOT NULL DEFAULT 0,
	html_url        TEXT NOT NULL,
	private         INTEGER NOT NULL DEFAULT 0,
	token_user_id   INTEGER,               -- whose GitHub token can read it (private repos)
	etag            TEXT NOT NULL DEFAULT '',
	timeline_page   INTEGER NOT NULL DEFAULT 1,
	gh_updated_at   INTEGER NOT NULL DEFAULT 0,
	last_activity   INTEGER NOT NULL DEFAULT 0,
	last_timeline   INTEGER NOT NULL DEFAULT 0,
	next_poll_at    INTEGER NOT NULL DEFAULT 0,
	last_error      TEXT NOT NULL DEFAULT '',
	merge_sha       TEXT NOT NULL DEFAULT '',
	release_etag    TEXT NOT NULL DEFAULT '',
	released_in     TEXT NOT NULL DEFAULT '', -- tag of the first release seen containing merge_sha
	UNIQUE (owner, repo, number)
);

CREATE TABLE IF NOT EXISTS events (
	id          INTEGER PRIMARY KEY,
	item_id     INTEGER NOT NULL REFERENCES items(id) ON DELETE CASCADE,
	gh_key      TEXT NOT NULL,
	kind        TEXT NOT NULL,             -- raw GitHub timeline event name
	category    TEXT NOT NULL,             -- comments | reviews | commits | labels | state | other
	actor       TEXT NOT NULL DEFAULT '',
	summary     TEXT NOT NULL,
	body        TEXT NOT NULL DEFAULT '',
	url         TEXT NOT NULL DEFAULT '',
	occurred_at INTEGER NOT NULL,
	ingested_at INTEGER NOT NULL,
	UNIQUE (item_id, gh_key)
);

CREATE TABLE IF NOT EXISTS watches (
	id                INTEGER PRIMARY KEY,
	user_id           INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	item_id           INTEGER NOT NULL REFERENCES items(id) ON DELETE CASCADE,
	note              TEXT NOT NULL,
	filter            TEXT NOT NULL,       -- comma-separated categories
	delivery          TEXT NOT NULL,       -- instant | digest
	status            TEXT NOT NULL DEFAULT 'active', -- active | muted | done
	notified_event_id INTEGER NOT NULL DEFAULT 0,
	viewed_event_id   INTEGER NOT NULL DEFAULT 0,
	created_at        INTEGER NOT NULL,
	UNIQUE (user_id, item_id)
);

-- A comment in some repo's code that references a watched item. Each sync
-- replaces one repo's rows; a watch with no note and no refs is deleted.
CREATE TABLE IF NOT EXISTS code_refs (
	watch_id INTEGER NOT NULL REFERENCES watches(id) ON DELETE CASCADE,
	repo     TEXT NOT NULL,                -- owner/name the comment is in, lowercase
	sha      TEXT NOT NULL,
	path     TEXT NOT NULL,
	line     INTEGER NOT NULL,
	note     TEXT NOT NULL
);

-- Repos scanned for code refs, per user.
CREATE TABLE IF NOT EXISTS code_repos (
	user_id   INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	repo      TEXT NOT NULL,               -- owner/name, lowercase
	pushed_at TEXT NOT NULL,               -- GitHub's pushed_at when last scanned
	sha       TEXT NOT NULL,               -- default branch commit last scanned
	token_id  INTEGER,                     -- the token that read it, if one could
	refusals  TEXT NOT NULL DEFAULT '{}', -- JSON token id -> Refusal, from GitHub refusing it the code
	PRIMARY KEY (user_id, repo)
);

-- A user's GitHub tokens. A fine-grained token covers one account or
-- organization, so a user may need several.
CREATE TABLE IF NOT EXISTS github_tokens (
	id         INTEGER PRIMARY KEY,
	user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	token      BLOB NOT NULL,                -- AES-GCM sealed
	login      TEXT NOT NULL,                -- the GitHub user it belongs to
	repos      TEXT NOT NULL DEFAULT '[]',   -- JSON []TokenRepo, as of listed_at
	listed_at  INTEGER NOT NULL DEFAULT 0,
	list_error TEXT NOT NULL DEFAULT '',     -- why the last listing failed, if it did
	created_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS events_item ON events(item_id, id);
CREATE INDEX IF NOT EXISTS github_tokens_user ON github_tokens(user_id);
CREATE INDEX IF NOT EXISTS items_poll ON items(next_poll_at);
CREATE INDEX IF NOT EXISTS watches_item ON watches(item_id);
CREATE INDEX IF NOT EXISTS code_refs_watch ON code_refs(watch_id);
`

// addedColumns brings databases created before a column existed up to schema.
var addedColumns = []string{
	`ALTER TABLE items ADD COLUMN merge_sha TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE items ADD COLUMN release_etag TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE items ADD COLUMN released_in TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE code_repos ADD COLUMN token_id INTEGER`,
	`ALTER TABLE code_repos ADD COLUMN refusals TEXT NOT NULL DEFAULT '{}'`,
}

// movedTokens moves each user's one GitHub token into github_tokens, from
// before a user could save several. It fails on its first statement once done.
var movedTokens = []string{
	`INSERT INTO github_tokens (user_id, token, login, created_at)
		SELECT id, github_token, github_login, created_at FROM users WHERE github_token IS NOT NULL`,
	`ALTER TABLE users DROP COLUMN github_token`,
	`ALTER TABLE users DROP COLUMN github_login`,
}

func moveTokens(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range movedTokens {
		if _, err := tx.Exec(q); err != nil {
			if strings.Contains(err.Error(), "no such column") {
				return nil
			}
			return err
		}
	}
	return tx.Commit()
}

type DB struct{ *sql.DB }

func openDB(path string) (*DB, error) {
	dsn := "file:" + path + "?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	if path == ":memory:" {
		dsn = "file::memory:?_pragma=foreign_keys(1)"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// SQLite allows one writer; a single connection avoids SQLITE_BUSY and
	// keeps :memory: databases shared across the app.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	for _, q := range addedColumns {
		if _, err := db.Exec(q); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			return nil, fmt.Errorf("migrate: %w", err)
		}
	}
	if err := moveTokens(db); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &DB{db}, nil
}

var errNotFound = errors.New("not found")

// ---- users ----

type User struct {
	ID              int64
	GoogleSub       string
	Email           string
	Name            string
	Picture         string
	TZ              string
	DefaultFilter   string
	DefaultDelivery string
	QuietStart      sql.NullInt64
	QuietEnd        sql.NullInt64
	DigestHour      int
	LastDigestDay   string
	HasGitHubToken  bool
	HasAPIToken     bool
}

const userCols = `id, google_sub, email, name, picture, tz, default_filter, default_delivery,
	quiet_start, quiet_end, digest_hour, last_digest_day,
	EXISTS (SELECT 1 FROM github_tokens t WHERE t.user_id = users.id), api_token_hash IS NOT NULL`

func scanUser(row interface{ Scan(...any) error }) (*User, error) {
	u := &User{}
	err := row.Scan(&u.ID, &u.GoogleSub, &u.Email, &u.Name, &u.Picture, &u.TZ, &u.DefaultFilter,
		&u.DefaultDelivery, &u.QuietStart, &u.QuietEnd, &u.DigestHour, &u.LastDigestDay,
		&u.HasGitHubToken, &u.HasAPIToken)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFound
	}
	return u, err
}

func (db *DB) UserByID(ctx context.Context, id int64) (*User, error) {
	return scanUser(db.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE id = ?`, id))
}

func (db *DB) UserByAPITokenHash(ctx context.Context, h string) (*User, error) {
	return scanUser(db.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE api_token_hash = ?`, h))
}

func (db *DB) UpsertGoogleUser(ctx context.Context, sub, email, name, picture string) (*User, error) {
	_, err := db.ExecContext(ctx, `
		INSERT INTO users (google_sub, email, name, picture, created_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (google_sub) DO UPDATE SET email = excluded.email, name = excluded.name, picture = excluded.picture`,
		sub, email, name, picture, time.Now().Unix())
	if err != nil {
		return nil, err
	}
	return scanUser(db.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE google_sub = ?`, sub))
}

type UserSettings struct {
	TZ              string
	DefaultFilter   string
	DefaultDelivery string
	QuietStart      sql.NullInt64
	QuietEnd        sql.NullInt64
	DigestHour      int
}

func (db *DB) UpdateUserSettings(ctx context.Context, uid int64, s UserSettings) error {
	_, err := db.ExecContext(ctx, `UPDATE users SET tz = ?, default_filter = ?, default_delivery = ?,
		quiet_start = ?, quiet_end = ?, digest_hour = ? WHERE id = ?`,
		s.TZ, s.DefaultFilter, s.DefaultDelivery, s.QuietStart, s.QuietEnd, s.DigestHour, uid)
	return err
}

func (db *DB) SetUserTZ(ctx context.Context, uid int64, tz string) error {
	_, err := db.ExecContext(ctx, `UPDATE users SET tz = ? WHERE id = ?`, tz, uid)
	return err
}

func (db *DB) SetAPITokenHash(ctx context.Context, uid int64, h string) error {
	_, err := db.ExecContext(ctx, `UPDATE users SET api_token_hash = ? WHERE id = ?`, h, uid)
	return err
}

func (db *DB) SetLastDigestDay(ctx context.Context, uid int64, day string) error {
	_, err := db.ExecContext(ctx, `UPDATE users SET last_digest_day = ? WHERE id = ?`, day, uid)
	return err
}

func (db *DB) AllUsers(ctx context.Context) ([]*User, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+userCols+` FROM users`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// ---- items ----

type Item struct {
	ID           int64
	Owner        string
	Repo         string
	Number       int
	Kind         string
	Title        string
	State        string
	Author       string
	Comments     int
	HTMLURL      string
	Private      bool
	TokenUserID  sql.NullInt64
	ETag         string
	TimelinePage int
	GHUpdatedAt  int64
	LastActivity int64
	LastTimeline int64
	NextPollAt   int64
	LastError    string
	MergeSHA     string
	ReleaseETag  string
	ReleasedIn   string
}

func (it *Item) Ref() string { return fmt.Sprintf("%s/%s#%d", it.Owner, it.Repo, it.Number) }

const itemCols = `id, owner, repo, number, kind, title, state, author, comments, html_url, private,
	token_user_id, etag, timeline_page, gh_updated_at, last_activity, last_timeline, next_poll_at, last_error,
	merge_sha, release_etag, released_in`

func scanItem(row interface{ Scan(...any) error }) (*Item, error) {
	it := &Item{}
	err := row.Scan(&it.ID, &it.Owner, &it.Repo, &it.Number, &it.Kind, &it.Title, &it.State, &it.Author,
		&it.Comments, &it.HTMLURL, &it.Private, &it.TokenUserID, &it.ETag, &it.TimelinePage,
		&it.GHUpdatedAt, &it.LastActivity, &it.LastTimeline, &it.NextPollAt, &it.LastError,
		&it.MergeSHA, &it.ReleaseETag, &it.ReleasedIn)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFound
	}
	return it, err
}

func (db *DB) ItemByRef(ctx context.Context, owner, repo string, number int) (*Item, error) {
	return scanItem(db.QueryRowContext(ctx, `SELECT `+itemCols+` FROM items WHERE owner = ? AND repo = ? AND number = ?`,
		strings.ToLower(owner), strings.ToLower(repo), number))
}

func (db *DB) ItemByID(ctx context.Context, id int64) (*Item, error) {
	return scanItem(db.QueryRowContext(ctx, `SELECT `+itemCols+` FROM items WHERE id = ?`, id))
}

func (db *DB) InsertItem(ctx context.Context, it *Item) error {
	res, err := db.ExecContext(ctx, `INSERT INTO items (owner, repo, number, kind, title, state, author, comments,
		html_url, private, token_user_id, etag, gh_updated_at, last_activity, next_poll_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		strings.ToLower(it.Owner), strings.ToLower(it.Repo), it.Number, it.Kind, it.Title, it.State, it.Author,
		it.Comments, it.HTMLURL, it.Private, it.TokenUserID, it.ETag, it.GHUpdatedAt, it.LastActivity, it.NextPollAt)
	if err != nil {
		return err
	}
	it.ID, err = res.LastInsertId()
	return err
}

func (db *DB) SaveItem(ctx context.Context, it *Item) error {
	_, err := db.ExecContext(ctx, `UPDATE items SET kind = ?, title = ?, state = ?, author = ?, comments = ?,
		html_url = ?, private = ?, token_user_id = ?, etag = ?, timeline_page = ?, gh_updated_at = ?,
		last_activity = ?, last_timeline = ?, next_poll_at = ?, last_error = ?, merge_sha = ?, release_etag = ?,
		released_in = ? WHERE id = ?`,
		it.Kind, it.Title, it.State, it.Author, it.Comments, it.HTMLURL, it.Private, it.TokenUserID, it.ETag,
		it.TimelinePage, it.GHUpdatedAt, it.LastActivity, it.LastTimeline, it.NextPollAt, it.LastError,
		it.MergeSHA, it.ReleaseETag, it.ReleasedIn, it.ID)
	return err
}

func (db *DB) DueItems(ctx context.Context, now time.Time, limit int) ([]*Item, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+itemCols+` FROM items WHERE next_poll_at <= ?
		AND EXISTS (SELECT 1 FROM watches w WHERE w.item_id = items.id) ORDER BY next_poll_at LIMIT ?`,
		now.Unix(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Item
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// DeleteOrphanItems removes items nobody watches any more (and their events).
func (db *DB) DeleteOrphanItems(ctx context.Context) error {
	_, err := db.ExecContext(ctx, `DELETE FROM items WHERE NOT EXISTS (SELECT 1 FROM watches w WHERE w.item_id = items.id)`)
	return err
}

// TokenCandidates returns GitHub tokens of users watching the item, those
// that listed its repo first, then the user recorded on the item's.
func (db *DB) TokenCandidates(ctx context.Context, it *Item) ([]*GitHubToken, error) {
	return db.queryTokens(ctx, `JOIN watches w ON w.user_id = t.user_id WHERE w.item_id = ?
		ORDER BY `+listsRepo+` DESC, (t.user_id = ?) DESC, t.id`, it.ID, it.Owner+"/"+it.Repo, it.TokenUserID)
}

// ---- GitHub tokens ----

type GitHubToken struct {
	ID        int64
	UserID    int64
	Sealed    []byte
	Login     string
	Repos     []TokenRepo
	ListedAt  int64
	ListError string
	CreatedAt int64
}

// TokenRepo is a repo a token can see.
type TokenRepo struct {
	Name    string `json:"name"` // owner/name as GitHub spells it
	Private bool   `json:"private,omitempty"`
	Scan    bool   `json:"scan,omitempty"` // its user can push to it and it isn't a fork, so its code is scanned
}

func (r TokenRepo) Key() string { return strings.ToLower(r.Name) }

// Scanned is the repos whose code the token's scans cover.
func (t *GitHubToken) Scanned() []TokenRepo {
	var out []TokenRepo
	for _, r := range t.Repos {
		if r.Scan {
			out = append(out, r)
		}
	}
	return out
}

const tokenCols = `t.id, t.user_id, t.token, t.login, t.repos, t.listed_at, t.list_error, t.created_at`

// listsRepo is true for tokens whose last listing included the repo in the
// query argument (lowercase owner/name).
const listsRepo = `EXISTS (SELECT 1 FROM json_each(t.repos) r WHERE lower(json_extract(r.value, '$.name')) = ?)`

func (db *DB) queryTokens(ctx context.Context, rest string, args ...any) ([]*GitHubToken, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+tokenCols+` FROM github_tokens t `+rest, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*GitHubToken
	for rows.Next() {
		t := &GitHubToken{}
		var repos string
		if err := rows.Scan(&t.ID, &t.UserID, &t.Sealed, &t.Login, &repos, &t.ListedAt, &t.ListError, &t.CreatedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(repos), &t.Repos); err != nil {
			return nil, fmt.Errorf("token %d repos: %w", t.ID, err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// GitHubTokens returns a user's tokens, oldest first.
func (db *DB) GitHubTokens(ctx context.Context, uid int64) ([]*GitHubToken, error) {
	return db.queryTokens(ctx, `WHERE t.user_id = ? ORDER BY t.id`, uid)
}

// GitHubTokensFor returns a user's tokens, those that listed repo (owner/name) first.
func (db *DB) GitHubTokensFor(ctx context.Context, uid int64, repo string) ([]*GitHubToken, error) {
	return db.queryTokens(ctx, `WHERE t.user_id = ? ORDER BY `+listsRepo+` DESC, t.id`, uid, strings.ToLower(repo))
}

func (db *DB) UserGitHubToken(ctx context.Context, uid, id int64) (*GitHubToken, error) {
	ts, err := db.queryTokens(ctx, `WHERE t.user_id = ? AND t.id = ?`, uid, id)
	if err != nil {
		return nil, err
	}
	if len(ts) == 0 {
		return nil, errNotFound
	}
	return ts[0], nil
}

func (db *DB) InsertGitHubToken(ctx context.Context, t *GitHubToken) error {
	res, err := db.ExecContext(ctx, `INSERT INTO github_tokens (user_id, token, login, created_at) VALUES (?, ?, ?, ?)`,
		t.UserID, t.Sealed, t.Login, t.CreatedAt)
	if err != nil {
		return err
	}
	t.ID, err = res.LastInsertId()
	return err
}

func (db *DB) SetTokenRepos(ctx context.Context, id int64, repos []TokenRepo, listedAt int64) error {
	b, _ := json.Marshal(append([]TokenRepo{}, repos...))
	_, err := db.ExecContext(ctx, `UPDATE github_tokens SET repos = ?, listed_at = ?, list_error = '' WHERE id = ?`,
		string(b), listedAt, id)
	return err
}

func (db *DB) SetTokenListError(ctx context.Context, id int64, msg string) error {
	_, err := db.ExecContext(ctx, `UPDATE github_tokens SET list_error = ? WHERE id = ?`, msg, id)
	return err
}

func (db *DB) DeleteGitHubToken(ctx context.Context, uid, id int64) error {
	_, err := db.ExecContext(ctx, `DELETE FROM github_tokens WHERE user_id = ? AND id = ?`, uid, id)
	return err
}

// ---- events ----

type Event struct {
	ID         int64
	ItemID     int64
	GHKey      string
	Kind       string
	Category   string
	Actor      string
	Summary    string
	Body       string
	URL        string
	OccurredAt int64
	IngestedAt int64
}

// InsertEvent stores the event unless it was seen before. It reports whether it was new.
func (db *DB) InsertEvent(ctx context.Context, e *Event) (bool, error) {
	res, err := db.ExecContext(ctx, `INSERT INTO events (item_id, gh_key, kind, category, actor, summary, body, url,
		occurred_at, ingested_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (item_id, gh_key) DO NOTHING`,
		e.ItemID, e.GHKey, e.Kind, e.Category, e.Actor, e.Summary, e.Body, e.URL, e.OccurredAt, e.IngestedAt)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n == 1 {
		e.ID, _ = res.LastInsertId()
	}
	return n == 1, nil
}

func (db *DB) MaxEventID(ctx context.Context, itemID int64) (int64, error) {
	var id sql.NullInt64
	err := db.QueryRowContext(ctx, `SELECT MAX(id) FROM events WHERE item_id = ?`, itemID).Scan(&id)
	return id.Int64, err
}

func (db *DB) EventsAfter(ctx context.Context, itemID, afterID int64) ([]*Event, error) {
	return db.queryEvents(ctx, `SELECT id, item_id, gh_key, kind, category, actor, summary, body, url, occurred_at,
		ingested_at FROM events WHERE item_id = ? AND id > ? ORDER BY id`, itemID, afterID)
}

func (db *DB) RecentEvents(ctx context.Context, itemID int64, limit int) ([]*Event, error) {
	return db.queryEvents(ctx, `SELECT id, item_id, gh_key, kind, category, actor, summary, body, url, occurred_at,
		ingested_at FROM events WHERE item_id = ? ORDER BY id DESC LIMIT ?`, itemID, limit)
}

func (db *DB) queryEvents(ctx context.Context, q string, args ...any) ([]*Event, error) {
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Event
	for rows.Next() {
		e := &Event{}
		if err := rows.Scan(&e.ID, &e.ItemID, &e.GHKey, &e.Kind, &e.Category, &e.Actor, &e.Summary, &e.Body,
			&e.URL, &e.OccurredAt, &e.IngestedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ---- watches ----

type Watch struct {
	ID              int64
	UserID          int64
	ItemID          int64
	Note            string
	Filter          string
	Delivery        string
	Status          string
	NotifiedEventID int64
	ViewedEventID   int64
	CreatedAt       int64

	Item     *Item     // joined
	NewCount int       // events after ViewedEventID
	Refs     []CodeRef // code comments that reference the item
}

// Why is what the user reads about why they watch the item: the note, then
// each code comment that references it.
func (w *Watch) Why() string {
	var parts []string
	if w.Note != "" {
		parts = append(parts, w.Note)
	}
	for _, c := range w.Refs {
		parts = append(parts, fmt.Sprintf("%s ([%s/%s:%d](%s))", c.Note, c.Repo, c.Path, c.Line, c.URL()))
	}
	return strings.Join(parts, "\n\n")
}

const watchCols = `w.id, w.user_id, w.item_id, w.note, w.filter, w.delivery, w.status, w.notified_event_id,
	w.viewed_event_id, w.created_at, (SELECT COUNT(*) FROM events e WHERE e.item_id = w.item_id AND e.id > w.viewed_event_id)`

func scanWatch(row interface{ Scan(...any) error }, withItem bool) (*Watch, error) {
	w := &Watch{}
	dest := []any{&w.ID, &w.UserID, &w.ItemID, &w.Note, &w.Filter, &w.Delivery, &w.Status, &w.NotifiedEventID,
		&w.ViewedEventID, &w.CreatedAt, &w.NewCount}
	if withItem {
		w.Item = &Item{}
		it := w.Item
		dest = append(dest, &it.ID, &it.Owner, &it.Repo, &it.Number, &it.Kind, &it.Title, &it.State, &it.Author,
			&it.Comments, &it.HTMLURL, &it.Private, &it.TokenUserID, &it.ETag, &it.TimelinePage,
			&it.GHUpdatedAt, &it.LastActivity, &it.LastTimeline, &it.NextPollAt, &it.LastError,
			&it.MergeSHA, &it.ReleaseETag, &it.ReleasedIn)
	}
	err := row.Scan(dest...)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFound
	}
	return w, err
}

var itemColsI = "i." + strings.ReplaceAll(itemCols, ", ", ", i.")

func (db *DB) queryWatches(ctx context.Context, where string, args ...any) ([]*Watch, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+watchCols+`, `+itemColsI+` FROM watches w
		JOIN items i ON i.id = w.item_id WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Watch
	for rows.Next() {
		w, err := scanWatch(rows, true)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	if err := rows.Err(); err != nil || len(out) == 0 {
		return out, err
	}
	return out, db.loadCodeRefs(ctx, out)
}

func (db *DB) loadCodeRefs(ctx context.Context, ws []*Watch) error {
	byID := map[int64]*Watch{}
	ids := make([]int64, len(ws))
	for i, w := range ws {
		byID[w.ID], ids[i] = w, w.ID
	}
	idsJSON, _ := json.Marshal(ids)
	rows, err := db.QueryContext(ctx, `SELECT watch_id, repo, sha, path, line, note FROM code_refs
		WHERE watch_id IN (SELECT value FROM json_each(?)) ORDER BY repo, path, line`, string(idsJSON))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var c CodeRef
		if err := rows.Scan(&c.WatchID, &c.Repo, &c.SHA, &c.Path, &c.Line, &c.Note); err != nil {
			return err
		}
		byID[c.WatchID].Refs = append(byID[c.WatchID].Refs, c)
	}
	return rows.Err()
}

func (db *DB) WatchByID(ctx context.Context, id int64) (*Watch, error) {
	ws, err := db.queryWatches(ctx, `w.id = ?`, id)
	if err != nil {
		return nil, err
	}
	if len(ws) == 0 {
		return nil, errNotFound
	}
	return ws[0], nil
}

func (db *DB) UserWatch(ctx context.Context, uid, id int64) (*Watch, error) {
	w, err := db.WatchByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if w.UserID != uid {
		return nil, errNotFound
	}
	return w, nil
}

func (db *DB) UserWatchForItem(ctx context.Context, uid, itemID int64) (*Watch, error) {
	ws, err := db.queryWatches(ctx, `w.user_id = ? AND w.item_id = ?`, uid, itemID)
	if err != nil {
		return nil, err
	}
	if len(ws) == 0 {
		return nil, errNotFound
	}
	return ws[0], nil
}

// ListWatches returns a user's watches with the given status, most recently active first.
// q, when set, matches titles, notes and repo names.
func (db *DB) ListWatches(ctx context.Context, uid int64, status, q string) ([]*Watch, error) {
	where := `w.user_id = ? AND w.status = ?`
	args := []any{uid, status}
	if q = strings.TrimSpace(q); q != "" {
		like := "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(strings.ToLower(q)) + "%"
		where += ` AND (lower(i.title) LIKE ? ESCAPE '\' OR lower(w.note) LIKE ? ESCAPE '\'
			OR (i.owner || '/' || i.repo) LIKE ? ESCAPE '\'
			OR EXISTS (SELECT 1 FROM code_refs c WHERE c.watch_id = w.id AND lower(c.note) LIKE ? ESCAPE '\'))`
		args = append(args, like, like, like, like)
	}
	return db.queryWatches(ctx, where+` ORDER BY MAX(i.last_activity, w.created_at) DESC`, args...)
}

func (db *DB) CountWatches(ctx context.Context, uid int64) (map[string]int, error) {
	rows, err := db.QueryContext(ctx, `SELECT status, COUNT(*) FROM watches WHERE user_id = ? GROUP BY status`, uid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var s string
		var n int
		if err := rows.Scan(&s, &n); err != nil {
			return nil, err
		}
		out[s] = n
	}
	return out, rows.Err()
}

// WatchesWithPending returns every watch that has events it hasn't been emailed about yet.
func (db *DB) WatchesWithPending(ctx context.Context) ([]*Watch, error) {
	return db.queryWatches(ctx, `EXISTS (SELECT 1 FROM events e WHERE e.item_id = w.item_id AND e.id > w.notified_event_id)`)
}

func (db *DB) InsertWatch(ctx context.Context, w *Watch) error {
	res, err := db.ExecContext(ctx, `INSERT INTO watches (user_id, item_id, note, filter, delivery, status,
		notified_event_id, viewed_event_id, created_at) VALUES (?, ?, ?, ?, ?, 'active', ?, ?, ?)`,
		w.UserID, w.ItemID, w.Note, w.Filter, w.Delivery, w.NotifiedEventID, w.ViewedEventID, w.CreatedAt)
	if err != nil {
		return err
	}
	w.ID, err = res.LastInsertId()
	return err
}

func (db *DB) SetWatchNote(ctx context.Context, id int64, note string) error {
	_, err := db.ExecContext(ctx, `UPDATE watches SET note = ? WHERE id = ?`, note, id)
	return err
}

func (db *DB) SetWatchPrefs(ctx context.Context, id int64, filter, delivery string) error {
	_, err := db.ExecContext(ctx, `UPDATE watches SET filter = ?, delivery = ? WHERE id = ?`, filter, delivery, id)
	return err
}

// SetWatchStatus changes status. Leaving "muted" skips what arrived while muted,
// so unmuting doesn't send a backlog.
func (db *DB) SetWatchStatus(ctx context.Context, id int64, status string) error {
	_, err := db.ExecContext(ctx, `UPDATE watches SET
		notified_event_id = CASE WHEN status = 'muted' AND ? != 'muted'
			THEN (SELECT COALESCE(MAX(e.id), 0) FROM events e WHERE e.item_id = watches.item_id)
			ELSE notified_event_id END,
		status = ? WHERE id = ?`, status, status, id)
	return err
}

func (db *DB) SetWatchNotified(ctx context.Context, id, eventID int64) error {
	_, err := db.ExecContext(ctx, `UPDATE watches SET notified_event_id = MAX(notified_event_id, ?) WHERE id = ?`, eventID, id)
	return err
}

func (db *DB) SetWatchViewed(ctx context.Context, id, eventID int64) error {
	_, err := db.ExecContext(ctx, `UPDATE watches SET viewed_event_id = MAX(viewed_event_id, ?) WHERE id = ?`, eventID, id)
	return err
}

func (db *DB) DeleteWatch(ctx context.Context, id int64) error {
	if _, err := db.ExecContext(ctx, `DELETE FROM watches WHERE id = ?`, id); err != nil {
		return err
	}
	return db.DeleteOrphanItems(ctx)
}

// ReplaceCodeRefs makes refs the complete set of code comments in repo for
// the user's watches, except that watches in keep hold on to their current
// refs from repo. Watches left with no note and no refs are deleted.
func (db *DB) ReplaceCodeRefs(ctx context.Context, uid int64, repo string, refs []CodeRef, keep []int64) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	keepJSON, _ := json.Marshal(append([]int64{}, keep...))
	if _, err := tx.ExecContext(ctx, `DELETE FROM code_refs WHERE repo = ?
		AND watch_id IN (SELECT id FROM watches WHERE user_id = ?)
		AND watch_id NOT IN (SELECT value FROM json_each(?))`, repo, uid, string(keepJSON)); err != nil {
		return err
	}
	for _, c := range refs {
		if _, err := tx.ExecContext(ctx, `INSERT INTO code_refs (watch_id, repo, sha, path, line, note)
			VALUES (?, ?, ?, ?, ?, ?)`, c.WatchID, repo, c.SHA, c.Path, c.Line, c.Note); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM watches WHERE user_id = ? AND note = ''
		AND NOT EXISTS (SELECT 1 FROM code_refs c WHERE c.watch_id = watches.id)`, uid); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM items WHERE NOT EXISTS (SELECT 1 FROM watches w WHERE w.item_id = items.id)`); err != nil {
		return err
	}
	return tx.Commit()
}

type CodeRepo struct {
	PushedAt string
	SHA      string
	TokenID  int64             // the token that read it; 0 if none could
	Refusals map[int64]Refusal // by token id, as of PushedAt
}

// Refusal is GitHub refusing a token a repo's code.
type Refusal struct {
	Why string `json:"why"` // refusedContents or refusedAccess
	At  int64  `json:"at"`
}

const (
	refusedContents = "contents" // the token sees the repo but lacks the Contents permission
	refusedAccess   = "access"   // the token doesn't cover the repo at all
)

// CodeRepos returns the user's scanned repos by lowercase owner/name.
func (db *DB) CodeRepos(ctx context.Context, uid int64) (map[string]CodeRepo, error) {
	rows, err := db.QueryContext(ctx, `SELECT repo, pushed_at, sha, COALESCE(token_id, 0), refusals
		FROM code_repos WHERE user_id = ?`, uid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]CodeRepo{}
	for rows.Next() {
		var name, refusals string
		var r CodeRepo
		if err := rows.Scan(&name, &r.PushedAt, &r.SHA, &r.TokenID, &refusals); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(refusals), &r.Refusals); err != nil {
			return nil, fmt.Errorf("code repo %s: %w", name, err)
		}
		out[name] = r
	}
	return out, rows.Err()
}

func (db *DB) SaveCodeRepo(ctx context.Context, uid int64, repo string, r CodeRepo) error {
	refusals, _ := json.Marshal(r.Refusals)
	if r.Refusals == nil {
		refusals = []byte("{}")
	}
	tokenID := sql.NullInt64{Int64: r.TokenID, Valid: r.TokenID != 0}
	_, err := db.ExecContext(ctx, `INSERT INTO code_repos (user_id, repo, pushed_at, sha, token_id, refusals)
		VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT (user_id, repo) DO UPDATE SET pushed_at = excluded.pushed_at,
		sha = excluded.sha, token_id = excluded.token_id, refusals = excluded.refusals`,
		uid, repo, r.PushedAt, r.SHA, tokenID, string(refusals))
	return err
}

// ForgetRefusals clears what GitHub refused a token, so its repos are tried
// again, as after its permissions change.
func (db *DB) ForgetRefusals(ctx context.Context, uid, tokenID int64) error {
	_, err := db.ExecContext(ctx, `UPDATE code_repos SET refusals = json_remove(refusals, '$."' || ? || '"')
		WHERE user_id = ?`, tokenID, uid)
	return err
}

func (db *DB) DeleteCodeRepo(ctx context.Context, uid int64, repo string) error {
	_, err := db.ExecContext(ctx, `DELETE FROM code_repos WHERE user_id = ? AND repo = ?`, uid, repo)
	return err
}
