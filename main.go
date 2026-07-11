package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"html/template"
	"io"
	"log/slog"
	mrand "math/rand"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/google/uuid"
	"github.com/gotd/contrib/middleware/floodwait"
	"github.com/gotd/contrib/middleware/ratelimit"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/dcs"
	"github.com/gotd/td/tg"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"golang.org/x/time/rate"
)

// ============================================================
// CONFIG
// ============================================================

type Config struct {
	MainBotToken  string   // BOT_TOKEN — sirf polling ke liye (main bot)
	BotTokens     []string // BOT_TOKEN + MULTI_TOKEN1,2,3... — sab MTProto ke liye
	APIID         int
	APIHash       string
	AdminID       int64
	DBURI         string
	RedisURI      string
	DBChannelID   int64
	LogChannelID  int64
	MainChannelID int64
	FQDN          string
	Port          string
	DashboardToken string // ADMIN_DASHBOARD_TOKEN — secret to open /admin dashboard

	// Streaming config (from env vars)
	StreamConcurrency int // STREAM_CONCURRENCY (default: 4)
	StreamBufferCount int // STREAM_BUFFER_COUNT (default: 8)
	StreamTimeoutSec  int // STREAM_TIMEOUT_SEC  (default: 30)
	StreamMaxRetries  int // STREAM_MAX_RETRIES  (default: 3)

	// PasswordPromptVideoURL — agar set hai, /watch ke password-gate page
	// par (password box ke upar) yeh video autoplay+loop chalega. Ismein
	// "password chahiye to Instagram pe DM karo" jaisi instruction video
	// daal sakte ho. Empty = video hide, sirf plain password form dikhega.
	PasswordPromptVideoURL string // PASSWORD_PROMPT_VIDEO_URL

	// PasswordPromptImages — comma-separated list of image URLs. If set,
	// the password-gate page shows an image below the password box that
	// randomly changes every few seconds (like a slideshow). Empty = hidden.
	PasswordPromptImages []string // PASSWORD_PROMPT_IMAGES

	// ContactTelegramUsername / ContactInstagramUsername — shown on the
	// password-gate page below the password box so people know where to
	// ask you for the password. Empty = that particular link is hidden.
	ContactTelegramUsername  string // CONTACT_TELEGRAM_USERNAME  (default: raj_dev_01)
	ContactInstagramUsername string // CONTACT_INSTAGRAM_USERNAME
}

func loadConfig() (*Config, error) {
	cfg := &Config{}
	var errs []string

	// ── TOKEN LOADING — us repo jaisa ──
	// BOT_TOKEN  = MAIN bot (polling karta hai — messages receive karta hai)
	// MULTI_TOKEN1,2... = WORKER bots (sirf streaming ke liye)
	// Dono milake BotTokens mein jaate hain MTProto ke liye
	mainToken := strings.TrimSpace(os.Getenv("BOT_TOKEN"))
	if mainToken == "" {
		errs = append(errs, "BOT_TOKEN missing — yeh main bot ka token hai")
	} else {
		cfg.MainBotToken = mainToken
		cfg.BotTokens = append(cfg.BotTokens, mainToken)
	}
	// Worker tokens — MULTI_TOKEN1 se MULTI_TOKEN20 tak
	for i := 1; i <= 20; i++ {
		t := strings.TrimSpace(os.Getenv(fmt.Sprintf("MULTI_TOKEN%d", i)))
		if t != "" {
			cfg.BotTokens = append(cfg.BotTokens, t)
		}
	}

	apiIDStr := os.Getenv("API_ID")
	if apiIDStr == "" {
		errs = append(errs, "API_ID missing")
	} else if id, err := strconv.Atoi(apiIDStr); err != nil {
		errs = append(errs, "API_ID invalid")
	} else {
		cfg.APIID = id
	}

	cfg.APIHash = os.Getenv("API_HASH")
	if cfg.APIHash == "" {
		errs = append(errs, "API_HASH missing")
	}

	adminStr := os.Getenv("ADMIN_ID")
	if adminStr == "" {
		errs = append(errs, "ADMIN_ID missing")
	} else if id, err := strconv.ParseInt(adminStr, 10, 64); err != nil {
		errs = append(errs, "ADMIN_ID invalid")
	} else {
		cfg.AdminID = id
	}

	cfg.DBURI = firstEnv("DB_URI", "DATABASE_URL")
	if cfg.DBURI == "" {
		errs = append(errs, "DB_URI or DATABASE_URL missing")
	}

	cfg.RedisURI = firstEnv("REDIS_URI", "REDIS_URL")
	if cfg.RedisURI == "" {
		errs = append(errs, "REDIS_URI or REDIS_URL missing")
	}

	cfg.FQDN = os.Getenv("FQDN")
	if cfg.FQDN == "" {
		errs = append(errs, "FQDN missing")
	}

	dbChStr := os.Getenv("DB_CHANNEL_ID")
	if dbChStr == "" {
		errs = append(errs, "DB_CHANNEL_ID missing")
	} else if id, err := strconv.ParseInt(dbChStr, 10, 64); err != nil {
		errs = append(errs, "DB_CHANNEL_ID invalid")
	} else {
		cfg.DBChannelID = id
	}

	logChStr := os.Getenv("LOG_CHANNEL_ID")
	if logChStr == "" {
		errs = append(errs, "LOG_CHANNEL_ID missing")
	} else if id, err := strconv.ParseInt(logChStr, 10, 64); err != nil {
		errs = append(errs, "LOG_CHANNEL_ID invalid")
	} else {
		cfg.LogChannelID = id
	}

	if v := os.Getenv("MAIN_CHANNEL_ID"); v != "" {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil {
			cfg.MainChannelID = id
		}
	}
	cfg.Port = firstEnv("PORT", "8080")

	// Dashboard token — agar ADMIN_DASHBOARD_TOKEN nahi diya, ek random token
	// generate karke admin ko startup pe Telegram se bhej diya jaayega.
	cfg.DashboardToken = strings.TrimSpace(os.Getenv("ADMIN_DASHBOARD_TOKEN"))
	if cfg.DashboardToken == "" {
		cfg.DashboardToken = randomToken(24)
	}

	// Stream config with defaults
	cfg.StreamConcurrency = envInt("STREAM_CONCURRENCY", 4)
	cfg.StreamBufferCount = envInt("STREAM_BUFFER_COUNT", 8)
	cfg.StreamTimeoutSec  = envInt("STREAM_TIMEOUT_SEC", 30)
	cfg.StreamMaxRetries  = envInt("STREAM_MAX_RETRIES", 3)

	cfg.PasswordPromptVideoURL = strings.TrimSpace(os.Getenv("PASSWORD_PROMPT_VIDEO_URL"))

	if raw := strings.TrimSpace(os.Getenv("PASSWORD_PROMPT_IMAGES")); raw != "" {
		for _, u := range strings.Split(raw, ",") {
			if u = strings.TrimSpace(u); u != "" {
				cfg.PasswordPromptImages = append(cfg.PasswordPromptImages, u)
			}
		}
	}

	cfg.ContactTelegramUsername = strings.TrimPrefix(strings.TrimSpace(os.Getenv("CONTACT_TELEGRAM_USERNAME")), "@")
	if cfg.ContactTelegramUsername == "" {
		cfg.ContactTelegramUsername = "raj_dev_01"
	}
	cfg.ContactInstagramUsername = strings.TrimPrefix(strings.TrimSpace(os.Getenv("CONTACT_INSTAGRAM_USERNAME")), "@")

	if len(errs) > 0 {
		return nil, fmt.Errorf("config errors: %s", strings.Join(errs, " | "))
	}
	return cfg, nil
}

func (c *Config) baseURL() string {
	fqdn := strings.TrimRight(c.FQDN, "/")
	if !strings.HasPrefix(fqdn, "http") {
		fqdn = "https://" + fqdn
	}
	return fqdn
}

func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

func envInt(key string, defaultVal int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultVal
}

// ============================================================
// DATABASE
// ============================================================

type FileRecord struct {
	ID           string
	MessageID    int       // Telegram message ID in storage channel
	ChannelID    int64     // Storage channel ID
	FileName     string
	FileSize     int64
	MimeType     string
	Hash         string    // Short hash for URL verification
	UploaderID   int64
	UploaderName string
	CreatedAt    time.Time
	ExpiresAt    *time.Time // nil = permanent/unlimited link (default)
	ViewCount    int64      // total UNIQUE-DEVICE /watch page visits (permanent, never resets)
	PasswordHash *string    // nil = no password. sha256 hex of the password, set via /setpass
	PasswordPlain *string   // plaintext password, kept ONLY so the admin dashboard can show it
}

type UserRecord struct {
	ID        int64
	Username  string
	FirstName string
	IsBanned  bool
}

// ApprovalRecord — one row per unique visitor device. AccessID is the
// 5-digit number shown to that visitor on the password page; the admin
// approves a specific visitor by running /approve <AccessID> in the bot.
// Until Approved is true, that device cannot stream even with the
// correct password — this is a second, manual gate on top of the password.
type ApprovalRecord struct {
	AccessID    int
	DeviceID    string
	Slug        string
	VisitorName string
	Approved    bool
	Blocked     bool
	CreatedAt   time.Time
	ApprovedAt  *time.Time
}

type DB struct{ pool *pgxpool.Pool }

func newDB(ctx context.Context, dsn string) (*DB, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse db uri: %w", err)
	}
	cfg.MaxConns = 10
	cfg.MinConns = 2
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		return nil, fmt.Errorf("ping db: %w", err)
	}
	db := &DB{pool: pool}
	return db, db.migrate(ctx)
}

func (db *DB) migrate(ctx context.Context) error {
	// Step 1: Create tables if they don't exist
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS users (
			id         BIGINT PRIMARY KEY,
			username   TEXT NOT NULL DEFAULT '',
			first_name TEXT NOT NULL DEFAULT '',
			is_banned  BOOLEAN NOT NULL DEFAULT FALSE,
			joined_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS files (
			id            TEXT PRIMARY KEY,
			message_id    INTEGER NOT NULL UNIQUE,
			channel_id    BIGINT NOT NULL DEFAULT 0,
			file_name     TEXT NOT NULL DEFAULT '',
			file_size     BIGINT NOT NULL DEFAULT 0,
			mime_type     TEXT NOT NULL DEFAULT 'application/octet-stream',
			hash          TEXT NOT NULL DEFAULT '',
			uploader_id   BIGINT NOT NULL DEFAULT 0,
			uploader_name TEXT NOT NULL DEFAULT '',
			created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE INDEX IF NOT EXISTS idx_files_message_id ON files(message_id)`,
		`CREATE INDEX IF NOT EXISTS idx_files_created_at ON files(created_at DESC)`,
		// file_views — ek row per (file, device). Isse "unique device" view counting hoti hai:
		// same device dobara dekhe toh count nahi badhta, sirf naya device count badhata hai.
		// Yeh permanent hai — kabhi delete nahi hota, sirf naye rows add hote hain.
		`CREATE TABLE IF NOT EXISTS file_views (
			file_id         TEXT NOT NULL,
			device_id       TEXT NOT NULL,
			first_viewed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			PRIMARY KEY (file_id, device_id)
		)`,
		// approvals — one row per unique visitor device on a password-protected
		// page. access_id is the 5-digit code shown to that visitor; the admin
		// runs /approve <access_id> in the bot to flip approved to TRUE. Until
		// then, that device can't stream even after entering the correct
		// password — an extra manual gate on top of the password itself.
		`CREATE TABLE IF NOT EXISTS approvals (
			access_id    INTEGER PRIMARY KEY,
			device_id    TEXT NOT NULL UNIQUE,
			slug         TEXT NOT NULL DEFAULT '',
			visitor_name TEXT NOT NULL DEFAULT '',
			approved     BOOLEAN NOT NULL DEFAULT FALSE,
			blocked      BOOLEAN NOT NULL DEFAULT FALSE,
			created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			approved_at  TIMESTAMPTZ NULL
		)`,
	} {
		if _, err := db.pool.Exec(ctx, q); err != nil {
			return fmt.Errorf("migration failed: %w", err)
		}
	}

	// Step 2: Add missing columns if table already existed with old schema
	// These are safe — IF NOT EXISTS equivalent using DO $$ blocks
	alterQueries := []string{
		`DO $$ BEGIN
			IF NOT EXISTS (SELECT 1 FROM information_schema.columns
				WHERE table_name='files' AND column_name='channel_id') THEN
				ALTER TABLE files ADD COLUMN channel_id BIGINT NOT NULL DEFAULT 0;
			END IF;
		END $$`,
		`DO $$ BEGIN
			IF NOT EXISTS (SELECT 1 FROM information_schema.columns
				WHERE table_name='files' AND column_name='hash') THEN
				ALTER TABLE files ADD COLUMN hash TEXT NOT NULL DEFAULT '';
			END IF;
		END $$`,
		`DO $$ BEGIN
			IF NOT EXISTS (SELECT 1 FROM information_schema.columns
				WHERE table_name='files' AND column_name='expires_at') THEN
				ALTER TABLE files ADD COLUMN expires_at TIMESTAMPTZ NULL;
			END IF;
		END $$`,
		`DO $$ BEGIN
			IF NOT EXISTS (SELECT 1 FROM information_schema.columns
				WHERE table_name='files' AND column_name='view_count') THEN
				ALTER TABLE files ADD COLUMN view_count BIGINT NOT NULL DEFAULT 0;
			END IF;
		END $$`,
		`DO $$ BEGIN
			IF NOT EXISTS (SELECT 1 FROM information_schema.columns
				WHERE table_name='files' AND column_name='password_hash') THEN
				ALTER TABLE files ADD COLUMN password_hash TEXT NULL;
			END IF;
		END $$`,
		`DO $$ BEGIN
			IF NOT EXISTS (SELECT 1 FROM information_schema.columns
				WHERE table_name='files' AND column_name='password_plain') THEN
				ALTER TABLE files ADD COLUMN password_plain TEXT NULL;
			END IF;
		END $$`,
		// Remove tg_file_id column if it exists (old schema cleanup)
		`DO $$ BEGIN
			IF EXISTS (SELECT 1 FROM information_schema.columns
				WHERE table_name='files' AND column_name='tg_file_id') THEN
				ALTER TABLE files DROP COLUMN tg_file_id;
			END IF;
		END $$`,
		`DO $$ BEGIN
			IF NOT EXISTS (SELECT 1 FROM information_schema.columns
				WHERE table_name='approvals' AND column_name='visitor_name') THEN
				ALTER TABLE approvals ADD COLUMN visitor_name TEXT NOT NULL DEFAULT '';
			END IF;
		END $$`,
		`DO $$ BEGIN
			IF NOT EXISTS (SELECT 1 FROM information_schema.columns
				WHERE table_name='approvals' AND column_name='blocked') THEN
				ALTER TABLE approvals ADD COLUMN blocked BOOLEAN NOT NULL DEFAULT FALSE;
			END IF;
		END $$`,
	}

	for _, q := range alterQueries {
		if _, err := db.pool.Exec(ctx, q); err != nil {
			return fmt.Errorf("alter migration failed: %w", err)
		}
	}

	return nil
}

func (db *DB) close() { db.pool.Close() }

func (db *DB) saveFile(ctx context.Context, f *FileRecord) error {
	_, err := db.pool.Exec(ctx, `
		INSERT INTO files (id,message_id,channel_id,file_name,file_size,mime_type,hash,uploader_id,uploader_name)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT (message_id) DO UPDATE SET
			id=EXCLUDED.id, channel_id=EXCLUDED.channel_id,
			file_name=EXCLUDED.file_name, file_size=EXCLUDED.file_size,
			mime_type=EXCLUDED.mime_type, hash=EXCLUDED.hash,
			uploader_id=EXCLUDED.uploader_id, uploader_name=EXCLUDED.uploader_name`,
		f.ID, f.MessageID, f.ChannelID, f.FileName,
		f.FileSize, f.MimeType, f.Hash,
		f.UploaderID, f.UploaderName,
	)
	return err
}

func (db *DB) getFileByID(ctx context.Context, id string) (*FileRecord, error) {
	f := &FileRecord{}
	err := db.pool.QueryRow(ctx, `
		SELECT id,message_id,channel_id,file_name,file_size,mime_type,hash,uploader_id,uploader_name,created_at,expires_at,view_count,password_hash,password_plain
		FROM files WHERE id=$1`, id).Scan(
		&f.ID, &f.MessageID, &f.ChannelID, &f.FileName,
		&f.FileSize, &f.MimeType, &f.Hash,
		&f.UploaderID, &f.UploaderName, &f.CreatedAt,
		&f.ExpiresAt, &f.ViewCount, &f.PasswordHash, &f.PasswordPlain,
	)
	return f, err
}

// setPassword sets (hash != nil) or clears (hash == nil) the password for a
// link. plain is the plaintext password shown on the admin dashboard —
// pass nil to clear it (when hash is nil).
func (db *DB) setPassword(ctx context.Context, id string, hash *string, plain *string) error {
	tag, err := db.pool.Exec(ctx, `UPDATE files SET password_hash=$1, password_plain=$2 WHERE id=$3`, hash, plain, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("file not found: %s", id)
	}
	return nil
}

// recordUniqueView registers a (file, device) pair. If it's the device's first
// visit to this file, the permanent view_count goes up by 1; otherwise the
// existing total is returned unchanged. Rows in file_views are never deleted.
func (db *DB) recordUniqueView(ctx context.Context, fileID, deviceID string) (isNew bool, total int64, err error) {
	tag, err := db.pool.Exec(ctx,
		`INSERT INTO file_views (file_id, device_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`,
		fileID, deviceID)
	if err != nil {
		return false, 0, err
	}
	isNew = tag.RowsAffected() > 0
	if isNew {
		err = db.pool.QueryRow(ctx,
			`UPDATE files SET view_count = view_count + 1 WHERE id=$1 RETURNING view_count`,
			fileID).Scan(&total)
	} else {
		err = db.pool.QueryRow(ctx, `SELECT view_count FROM files WHERE id=$1`, fileID).Scan(&total)
	}
	return
}

// topFilesByViews returns the most-viewed files for the admin dashboard.
// searchFiles finds files whose name contains the query (case-insensitive).
// Used by the /search endpoint so users can type a topic name (e.g. "human
// behaviour") and jump straight to that video without needing a new link.
func (db *DB) searchFiles(ctx context.Context, query string, limit int) ([]*FileRecord, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT id,message_id,channel_id,file_name,file_size,mime_type,hash,uploader_id,uploader_name,created_at,expires_at,view_count,password_hash,password_plain
		FROM files WHERE file_name ILIKE '%' || $1 || '%'
		ORDER BY view_count DESC LIMIT $2`, query, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*FileRecord
	for rows.Next() {
		f := &FileRecord{}
		if err := rows.Scan(&f.ID, &f.MessageID, &f.ChannelID, &f.FileName,
			&f.FileSize, &f.MimeType, &f.Hash, &f.UploaderID, &f.UploaderName,
			&f.CreatedAt, &f.ExpiresAt, &f.ViewCount, &f.PasswordHash, &f.PasswordPlain); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (db *DB) topFilesByViews(ctx context.Context, limit int) ([]*FileRecord, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT id,message_id,channel_id,file_name,file_size,mime_type,hash,uploader_id,uploader_name,created_at,expires_at,view_count,password_hash,password_plain
		FROM files ORDER BY view_count DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*FileRecord
	for rows.Next() {
		f := &FileRecord{}
		if err := rows.Scan(&f.ID, &f.MessageID, &f.ChannelID, &f.FileName,
			&f.FileSize, &f.MimeType, &f.Hash, &f.UploaderID, &f.UploaderName,
			&f.CreatedAt, &f.ExpiresAt, &f.ViewCount, &f.PasswordHash, &f.PasswordPlain); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// sumViews returns the sum of view_count across every file (for dashboard totals).
func (db *DB) sumViews(ctx context.Context) (int64, error) {
	var n int64
	return n, db.pool.QueryRow(ctx, `SELECT COALESCE(SUM(view_count),0) FROM files`).Scan(&n)
}

// setExpiry sets (or clears, when expiresAt is nil) the expiry timestamp for a link.
func (db *DB) setExpiry(ctx context.Context, id string, expiresAt *time.Time) error {
	tag, err := db.pool.Exec(ctx, `UPDATE files SET expires_at=$1 WHERE id=$2`, expiresAt, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("file not found: %s", id)
	}
	return nil
}

// incrementViews bumps the view counter by 1 and returns the new total.
func (db *DB) incrementViews(ctx context.Context, id string) (int64, error) {
	var n int64
	err := db.pool.QueryRow(ctx,
		`UPDATE files SET view_count = view_count + 1 WHERE id=$1 RETURNING view_count`, id).Scan(&n)
	return n, err
}

func (db *DB) deleteFileByMsgID(ctx context.Context, msgID int) (bool, error) {
	tag, err := db.pool.Exec(ctx, `DELETE FROM files WHERE message_id=$1`, msgID)
	return tag.RowsAffected() > 0, err
}

func (db *DB) countFiles(ctx context.Context) (int64, error) {
	var n int64
	return n, db.pool.QueryRow(ctx, `SELECT COUNT(*) FROM files`).Scan(&n)
}

// deleteAllFiles wipes every file record (and their view rows) permanently.
// It does NOT touch users. Returns how many file rows were deleted.
func (db *DB) deleteAllFiles(ctx context.Context) (int64, error) {
	tag, err := db.pool.Exec(ctx, `DELETE FROM files`)
	if err != nil {
		return 0, err
	}
	// Best-effort cleanup of the view-tracking table; not fatal if it fails.
	db.pool.Exec(ctx, `DELETE FROM file_views`) //nolint:errcheck
	return tag.RowsAffected(), nil
}

func (db *DB) upsertUser(ctx context.Context, u *UserRecord) error {
	_, err := db.pool.Exec(ctx, `
		INSERT INTO users (id,username,first_name) VALUES ($1,$2,$3)
		ON CONFLICT (id) DO UPDATE SET
			username=EXCLUDED.username, first_name=EXCLUDED.first_name`,
		u.ID, u.Username, u.FirstName)
	return err
}

func (db *DB) getUser(ctx context.Context, id int64) (*UserRecord, error) {
	u := &UserRecord{}
	return u, db.pool.QueryRow(ctx,
		`SELECT id,username,first_name,is_banned FROM users WHERE id=$1`, id).
		Scan(&u.ID, &u.Username, &u.FirstName, &u.IsBanned)
}

func (db *DB) banUser(ctx context.Context, id int64, ban bool) error {
	_, err := db.pool.Exec(ctx, `UPDATE users SET is_banned=$1 WHERE id=$2`, ban, id)
	return err
}

func (db *DB) countUsers(ctx context.Context) (int64, error) {
	var n int64
	return n, db.pool.QueryRow(ctx, `SELECT COUNT(*) FROM users`).Scan(&n)
}

// getOrCreateApproval returns this device's existing approval row, or
// creates one with a fresh random 5-digit access_id if it doesn't have one
// yet. Same device always gets the same access_id back (device_id is
// UNIQUE), so refreshing the page doesn't hand out a new code every time.
// isNew is true only the first time this device is seen, so the caller can
// notify the admin exactly once per device.
func (db *DB) getOrCreateApproval(ctx context.Context, deviceID, slug string) (rec *ApprovalRecord, isNew bool, err error) {
	rec = &ApprovalRecord{}
	err = db.pool.QueryRow(ctx,
		`SELECT access_id, device_id, slug, visitor_name, approved, blocked, created_at, approved_at
		 FROM approvals WHERE device_id=$1`, deviceID).
		Scan(&rec.AccessID, &rec.DeviceID, &rec.Slug, &rec.VisitorName, &rec.Approved, &rec.Blocked, &rec.CreatedAt, &rec.ApprovedAt)
	if err == nil {
		return rec, false, nil
	}

	// No row yet — generate a random 5-digit code (10000-99999) and retry a
	// few times on the rare collision since access_id is the primary key.
	for attempt := 0; attempt < 8; attempt++ {
		candidate := 10000 + mrand.Intn(90000)
		_, insertErr := db.pool.Exec(ctx,
			`INSERT INTO approvals (access_id, device_id, slug) VALUES ($1,$2,$3)`,
			candidate, deviceID, slug)
		if insertErr == nil {
			return &ApprovalRecord{AccessID: candidate, DeviceID: deviceID, Slug: slug}, true, nil
		}
	}
	return nil, false, fmt.Errorf("could not allocate a unique access id")
}

// setApprovalName saves the name a visitor typed in alongside the password —
// shown later in /user so the admin knows who's who, not just a bare ID.
func (db *DB) setApprovalName(ctx context.Context, deviceID, name string) error {
	_, err := db.pool.Exec(ctx,
		`UPDATE approvals SET visitor_name=$1 WHERE device_id=$2`, name, deviceID)
	return err
}

// listApprovals returns the most recent visitors (any device that has ever
// hit a gated link), newest first, for the /user command.
func (db *DB) listApprovals(ctx context.Context, limit int) ([]*ApprovalRecord, error) {
	rows, err := db.pool.Query(ctx,
		`SELECT access_id, device_id, slug, visitor_name, approved, blocked, created_at, approved_at
		 FROM approvals ORDER BY created_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*ApprovalRecord
	for rows.Next() {
		rec := &ApprovalRecord{}
		if err := rows.Scan(&rec.AccessID, &rec.DeviceID, &rec.Slug, &rec.VisitorName, &rec.Approved, &rec.Blocked, &rec.CreatedAt, &rec.ApprovedAt); err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// approveByID flips approved to TRUE for the given 5-digit access code.
// Returns false (no error) if no such code exists, so the bot command can
// tell the admin "ID not found" vs a real DB error.
func (db *DB) approveByID(ctx context.Context, accessID int) (bool, error) {
	tag, err := db.pool.Exec(ctx,
		`UPDATE approvals SET approved=TRUE, approved_at=NOW() WHERE access_id=$1`, accessID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// blockByID flips blocked to TRUE/FALSE for the given 5-digit access code —
// used by /block and /unblock. A blocked visitor is denied access outright
// (no pending page, no re-notify) even if they were previously approved or
// figure out the password again. Returns false (no error) if the ID isn't found.
func (db *DB) blockByID(ctx context.Context, accessID int, block bool) (bool, error) {
	tag, err := db.pool.Exec(ctx,
		`UPDATE approvals SET blocked=$1 WHERE access_id=$2`, block, accessID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// deletePendingApprovals removes every approval row that is neither
// approved nor blocked — used by both the 30-minute auto-cleanup and the
// /clearpending command. Approved visitors and blocked IDs are always kept,
// so approvals never silently expire and blocks never silently lift.
func (db *DB) deletePendingApprovals(ctx context.Context, olderThan *time.Duration) (int64, error) {
	if olderThan != nil {
		tag, err := db.pool.Exec(ctx,
			`DELETE FROM approvals WHERE approved=FALSE AND blocked=FALSE AND created_at < NOW() - $1::interval`,
			fmt.Sprintf("%d seconds", int64(olderThan.Seconds())))
		if err != nil {
			return 0, err
		}
		return tag.RowsAffected(), nil
	}
	tag, err := db.pool.Exec(ctx,
		`DELETE FROM approvals WHERE approved=FALSE AND blocked=FALSE`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// ============================================================
// CACHE
// ============================================================

type Cache struct{ client *redis.Client }

type cachedFile struct {
	MessageID    int        `json:"message_id"`
	ChannelID    int64      `json:"channel_id"`
	FileName     string     `json:"file_name"`
	FileSize     int64      `json:"file_size"`
	MimeType     string     `json:"mime_type"`
	Hash         string     `json:"hash"`
	ExpiresAt    *time.Time `json:"expires_at,omitempty"`
	PasswordHash *string    `json:"password_hash,omitempty"`
}

func newCache(ctx context.Context, uri string) (*Cache, error) {
	opts, err := redis.ParseURL(uri)
	if err != nil {
		return nil, fmt.Errorf("parse redis uri: %w", err)
	}
	opts.DialTimeout = 10 * time.Second
	opts.ReadTimeout = 5 * time.Second
	opts.WriteTimeout = 5 * time.Second
	client := redis.NewClient(opts)
	if err := client.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("ping redis: %w", err)
	}
	return &Cache{client: client}, nil
}

func (c *Cache) close() error { return c.client.Close() }

func (c *Cache) setFile(ctx context.Context, id string, f *cachedFile) {
	b, _ := json.Marshal(f)
	c.client.Set(ctx, "file:"+id, b, time.Hour)
}

func (c *Cache) getFile(ctx context.Context, id string) *cachedFile {
	b, err := c.client.Get(ctx, "file:"+id).Bytes()
	if err != nil {
		return nil
	}
	var f cachedFile
	if json.Unmarshal(b, &f) != nil {
		return nil
	}
	return &f
}

func (c *Cache) delFile(ctx context.Context, id string) { c.client.Del(ctx, "file:"+id) }

// clearAllFileCache wipes every cached file entry ("file:*") — used by /dminem
// so stale cache doesn't keep serving links after a full delete-all.
func (c *Cache) clearAllFileCache(ctx context.Context) {
	iter := c.client.Scan(ctx, 0, "file:*", 200).Iterator()
	for iter.Next(ctx) {
		c.client.Del(ctx, iter.Val())
	}
}

func (c *Cache) setFsub(ctx context.Context, userID int64, ok bool) {
	val := "0"
	if ok {
		val = "1"
	}
	c.client.Set(ctx, fmt.Sprintf("fsub:%d", userID), val, 5*time.Minute)
}

func (c *Cache) getFsub(ctx context.Context, userID int64) (ok, found bool) {
	val, err := c.client.Get(ctx, fmt.Sprintf("fsub:%d", userID)).Result()
	if err != nil {
		return false, false
	}
	return val == "1", true
}

func (c *Cache) delFsub(ctx context.Context, userID int64) {
	c.client.Del(ctx, fmt.Sprintf("fsub:%d", userID))
}

// ── LIVE VIEWERS ──
// Har device ek "heartbeat" bhejta hai (watch page se) har ~15s mein.
// Redis sorted-set mein score = last-seen unix timestamp. 30s se purana
// heartbeat "offline" maan liya jaata hai. Isse "abhi kitne log dekh rahe
// hain" real-time dikh jaata hai, bina kisi extra DB load ke.
const liveWindowSecs = 30

func (c *Cache) heartbeat(ctx context.Context, slug, deviceID string) {
	now := float64(time.Now().Unix())
	cutoff := fmt.Sprintf("%f", now-liveWindowSecs)

	key := "live:" + slug
	c.client.ZAdd(ctx, key, redis.Z{Score: now, Member: deviceID})
	c.client.ZRemRangeByScore(ctx, key, "-inf", cutoff)
	c.client.Expire(ctx, key, 2*time.Minute)

	gkey := "live:__all__"
	c.client.ZAdd(ctx, gkey, redis.Z{Score: now, Member: slug + ":" + deviceID})
	c.client.ZRemRangeByScore(ctx, gkey, "-inf", cutoff)
	c.client.Expire(ctx, gkey, 2*time.Minute)
}

func (c *Cache) liveCount(ctx context.Context, slug string) int64 {
	now := float64(time.Now().Unix())
	key := "live:" + slug
	c.client.ZRemRangeByScore(ctx, key, "-inf", fmt.Sprintf("%f", now-liveWindowSecs))
	n, _ := c.client.ZCard(ctx, key).Result()
	return n
}

func (c *Cache) liveCountAll(ctx context.Context) int64 {
	now := float64(time.Now().Unix())
	key := "live:__all__"
	c.client.ZRemRangeByScore(ctx, key, "-inf", fmt.Sprintf("%f", now-liveWindowSecs))
	n, _ := c.client.ZCard(ctx, key).Result()
	return n
}

// ============================================================
// BOT POOL
// ============================================================

type BotPool struct {
	bots   []*tgbotapi.BotAPI
	index  atomic.Uint64
	mu     sync.RWMutex
	logger *zap.Logger
}

func newBotPool(tokens []string, logger *zap.Logger) (*BotPool, error) {
	p := &BotPool{logger: logger}
	for i, token := range tokens {
		bot, err := tgbotapi.NewBotAPI(token)
		if err != nil {
			return nil, fmt.Errorf("bot %d: %w", i+1, err)
		}
		p.bots = append(p.bots, bot)
		logger.Info("bot ready", zap.String("username", "@"+bot.Self.UserName))
	}
	return p, nil
}

func (p *BotPool) primary() *tgbotapi.BotAPI {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.bots[0]
}

func (p *BotPool) next() *tgbotapi.BotAPI {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.bots[int(p.index.Add(1)-1)%len(p.bots)]
}

func (p *BotPool) count() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.bots)
}

func (p *BotPool) isMember(channelID, userID int64) (bool, error) {
	m, err := p.primary().GetChatMember(tgbotapi.GetChatMemberConfig{
		ChatConfigWithUser: tgbotapi.ChatConfigWithUser{ChatID: channelID, UserID: userID},
	})
	if err != nil {
		return false, err
	}
	s := m.Status
	return s == "creator" || s == "administrator" || s == "member" || s == "restricted", nil
}

func (p *BotPool) send(chatID int64, text string) {
	p.primary().Send(tgbotapi.NewMessage(chatID, text)) //nolint:errcheck
}

func (p *BotPool) sendMD(chatID int64, text string) {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "MarkdownV2"
	p.primary().Send(msg) //nolint:errcheck
}

func (p *BotPool) sendKB(chatID int64, text string, kb tgbotapi.InlineKeyboardMarkup) {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "MarkdownV2"
	msg.ReplyMarkup = kb
	p.primary().Send(msg) //nolint:errcheck
}

func (p *BotPool) delMsg(chatID int64, msgID int) {
	p.primary().Request(tgbotapi.NewDeleteMessage(chatID, msgID)) //nolint:errcheck
}

func (p *BotPool) stopUpdates() { p.primary().StopReceivingUpdates() }

// ============================================================
// MTPROTO CLIENT POOL
// This is the KEY difference — proper MTProto like fast-stream-bot!
// Uses message_id + channel_id to fetch files directly
// NO size limit — works for any file size!
// ============================================================

// Dynamic block size — inspired from fast-stream-bot's pipe.go
func calculateBlockSize(start, end int64) int64 {
	size := end - start + 1
	switch {
	case size < 512*1024:       return 64 * 1024   // < 512KB  → 64KB blocks
	case size < 4*1024*1024:    return 256 * 1024  // < 4MB    → 256KB blocks
	case size < 32*1024*1024:   return 512 * 1024  // < 32MB   → 512KB blocks
	default:                    return 1024 * 1024  // default  → 1MB blocks
	}
}

const telegramChunkSize = 1024 * 1024 // max chunk size

type MTProtoPool struct {
	bots   []*mtBot
	index  atomic.Uint64
	mu     sync.RWMutex
	logger *zap.Logger
}

type mtBot struct {
	client *telegram.Client
	api    *tg.Client
	token  string
	ready  bool
	mu     sync.Mutex
}

func (b *mtBot) isReady() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.ready
}

func newMTProtoPool(apiID int, apiHash string, tokens []string, logger *zap.Logger) *MTProtoPool {
	pool := &MTProtoPool{logger: logger}
	for _, token := range tokens {
		pool.bots = append(pool.bots, &mtBot{token: token})
	}
	// Start all MTProto clients
	for i, bot := range pool.bots {
		go func(idx int, b *mtBot) {
			pool.startBot(context.Background(), apiID, apiHash, b)
		}(i, bot)
	}
	return pool
}

// getFloodMiddleware returns flood wait + rate limiter — from fast-stream-bot's middleware.go
func getFloodMiddleware() []telegram.Middleware {
	waiter := floodwait.NewSimpleWaiter().WithMaxRetries(10)
	limiter := ratelimit.New(rate.Every(100*time.Millisecond), 5)
	return []telegram.Middleware{waiter, limiter}
}

func (p *MTProtoPool) startBot(ctx context.Context, apiID int, apiHash string, b *mtBot) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// No DC hardcoded — auto-detect handles USER_MIGRATE correctly
		// Flood wait + rate limiter from fast-stream-bot middleware
		client := telegram.NewClient(apiID, apiHash, telegram.Options{
			DCList:      dcs.Prod(),
			Logger:      p.logger.Named("mtproto"),
			Middlewares: getFloodMiddleware(),
		})

		err := client.Run(ctx, func(ctx context.Context) error {
			if _, err := client.Auth().Bot(ctx, b.token); err != nil {
				return fmt.Errorf("bot auth failed: %w", err)
			}

			b.mu.Lock()
			b.client = client
			b.api = tg.NewClient(client)
			b.ready = true
			b.mu.Unlock()

			p.logger.Info("MTProto bot authenticated")

			<-ctx.Done()
			return nil
		})

		b.mu.Lock()
		b.ready = false
		b.mu.Unlock()

		if err != nil && ctx.Err() == nil {
			p.logger.Warn("MTProto reconnecting...", zap.Error(err))
			time.Sleep(5 * time.Second)
		} else {
			return
		}
	}
}

func (p *MTProtoPool) next() *mtBot {
	p.mu.RLock()
	defer p.mu.RUnlock()
	n := int(p.index.Add(1)-1) % len(p.bots)
	return p.bots[n]
}

func (p *MTProtoPool) isAnyReady() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, b := range p.bots {
		if b.isReady() {
			return true
		}
	}
	return false
}

// getFileLocation fetches InputDocumentFileLocation from message
// This is EXACTLY how fast-stream-bot works!
func (p *MTProtoPool) getFileLocation(ctx context.Context, channelID int64, messageID int) (*tg.InputDocumentFileLocation, int64, error) {
	bot := p.next()
	if !bot.isReady() {
		return nil, 0, fmt.Errorf("MTProto bot not ready")
	}

	bot.mu.Lock()
	api := bot.api
	bot.mu.Unlock()

	// Build input channel — same as fast-stream-bot's GetChannelPeer
	inputChan := &tg.InputChannel{ChannelID: channelID}

	// Resolve channel to get access hash
	result, err := api.ChannelsGetChannels(ctx, []tg.InputChannelClass{inputChan})
	if err != nil {
		return nil, 0, fmt.Errorf("get channel: %w", err)
	}

	var accessHash int64
	if chats, ok := result.(*tg.MessagesChats); ok {
		for _, chat := range chats.Chats {
			if ch, ok := chat.(*tg.Channel); ok && ch.ID == channelID {
				accessHash = ch.AccessHash
				break
			}
		}
	}

	inputChan.AccessHash = accessHash

	// Get the message — same as fast-stream-bot's GetChannelMessage
	msgs, err := api.ChannelsGetMessages(ctx, &tg.ChannelsGetMessagesRequest{
		Channel: inputChan,
		ID:      []tg.InputMessageClass{&tg.InputMessageID{ID: messageID}},
	})
	if err != nil {
		return nil, 0, fmt.Errorf("get message: %w", err)
	}

	// Extract document — same as fast-stream-bot's GetMediaFromMessage
	var messages []tg.MessageClass
	switch m := msgs.(type) {
	case *tg.MessagesMessages:
		messages = m.Messages
	case *tg.MessagesMessagesSlice:
		messages = m.Messages
	case *tg.MessagesChannelMessages:
		messages = m.Messages
	}

	for _, msg := range messages {
		m, ok := msg.(*tg.Message)
		if !ok {
			continue
		}
		media, ok := m.Media.(*tg.MessageMediaDocument)
		if !ok {
			continue
		}
		doc, ok := media.Document.(*tg.Document)
		if !ok {
			continue
		}
		return &tg.InputDocumentFileLocation{
			ID:            doc.ID,
			AccessHash:    doc.AccessHash,
			FileReference: doc.FileReference,
		}, doc.Size, nil
	}

	return nil, 0, fmt.Errorf("no document in message %d", messageID)
}

// TgFileReader — directly inspired from fast-stream-bot's stream.go
// This is why their bot works so smoothly!
// TgFileReader — concurrent prefetching streaming reader
// Combines fast-stream-bot's pipe.go logic with our MTProto pool
type TgFileReader struct {
	ctx    context.Context
	cancel context.CancelFunc
	api    *tg.Client
	cfg    *Config

	location  *tg.InputDocumentFileLocation
	start     int64
	end       int64
	blockSize int64
	totalBytes int64

	// prefetch pipeline
	blockQueue   chan []byte
	currentBlock []byte
	blockOffset  int64
	bytesRead    int64

	closeOnce sync.Once
}

func newTgFileReader(ctx context.Context, api *tg.Client, cfg *Config,
	location *tg.InputDocumentFileLocation, fileSize, start, end int64) *TgFileReader {

	ctx, cancel := context.WithCancel(ctx)
	blockSize := calculateBlockSize(start, end)

	r := &TgFileReader{
		ctx:        ctx,
		cancel:     cancel,
		api:        api,
		cfg:        cfg,
		location:   location,
		start:      start,
		end:        end,
		blockSize:  blockSize,
		totalBytes: end - start + 1,
		blockQueue: make(chan []byte, cfg.StreamBufferCount),
	}
	go r.prefetch()
	return r
}

func (r *TgFileReader) Close() {
	r.closeOnce.Do(func() { r.cancel() })
}

// Read implements io.Reader with concurrent prefetch
func (r *TgFileReader) Read(p []byte) (n int, err error) {
	if r.bytesRead >= r.totalBytes {
		return 0, io.EOF
	}

	// Need a new block from the prefetch queue?
	if r.blockOffset >= int64(len(r.currentBlock)) {
		select {
		case block, ok := <-r.blockQueue:
			if !ok {
				if r.bytesRead >= r.totalBytes {
					return 0, io.EOF
				}
				return 0, fmt.Errorf("pipe drained")
			}
			r.currentBlock = block
			r.blockOffset = 0
		case <-r.ctx.Done():
			return 0, r.ctx.Err()
		}
	}

	n = copy(p, r.currentBlock[r.blockOffset:])
	r.blockOffset += int64(n)
	r.bytesRead += int64(n)
	return n, nil
}

// prefetch runs concurrently, fetching blocks in parallel batches
// Inspired from fast-stream-bot's pipe.go prefetch()
func (r *TgFileReader) prefetch() {
	defer close(r.blockQueue)

	alignedStart := r.start - (r.start % r.blockSize)
	leftTrim     := r.start - alignedStart
	rightTrim    := (r.end % r.blockSize) + 1
	totalBlocks  := int((r.end - alignedStart + r.blockSize) / r.blockSize)

	currentBlock := 0
	offset       := alignedStart

	for currentBlock < totalBlocks {
		select {
		case <-r.ctx.Done():
			return
		default:
		}

		// Fetch a concurrent batch
		batchSize := r.cfg.StreamConcurrency
		if batchSize > totalBlocks - currentBlock {
			batchSize = totalBlocks - currentBlock
		}
		blocks := make([][]byte, batchSize)

		var wg sync.WaitGroup
		var fetchErr error
		var errMu sync.Mutex

		for i := 0; i < batchSize; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				blockNum    := currentBlock + idx
				blockOffset := offset + int64(idx)*r.blockSize

				data, err := r.downloadWithRetry(blockOffset)
				if err != nil {
					errMu.Lock()
					if fetchErr == nil { fetchErr = err }
					errMu.Unlock()
					return
				}

				dataLen := int64(len(data))
				// Trim first/last block to exact requested range
				if totalBlocks == 1 {
					if rightTrim > dataLen { rightTrim = dataLen }
					if leftTrim  > dataLen { leftTrim  = dataLen }
					data = data[leftTrim:rightTrim]
				} else if blockNum == 0 {
					if leftTrim > dataLen { leftTrim = dataLen }
					data = data[leftTrim:]
				} else if blockNum == totalBlocks-1 {
					if dataLen > rightTrim { data = data[:rightTrim] }
				}
				blocks[idx] = data
			}(i)
		}
		wg.Wait()

		if fetchErr != nil && r.ctx.Err() == nil {
			return
		}

		for _, block := range blocks {
			if block == nil { return }
			select {
			case r.blockQueue <- block:
			case <-r.ctx.Done():
				return
			}
		}

		currentBlock += batchSize
		offset       += r.blockSize * int64(batchSize)
	}
}

// downloadWithRetry fetches a block with exponential backoff
func (r *TgFileReader) downloadWithRetry(offset int64) ([]byte, error) {
	backoff := 100 * time.Millisecond
	const maxBackoff = 15 * time.Second
	var lastErr error

	for attempt := 0; attempt < r.cfg.StreamMaxRetries; attempt++ {
		if r.ctx.Err() != nil {
			return nil, r.ctx.Err()
		}

		timeout := time.Duration(r.cfg.StreamTimeoutSec) * time.Second
		ctx, cancel := context.WithTimeout(r.ctx, timeout)
		data, err := r.downloadBlock(ctx, offset)
		cancel()

		if err == nil {
			return data, nil
		}
		lastErr = err

		if r.ctx.Err() != nil {
			return nil, r.ctx.Err()
		}

		select {
		case <-time.After(backoff):
			backoff *= 2
			if backoff > maxBackoff { backoff = maxBackoff }
		case <-r.ctx.Done():
			return nil, r.ctx.Err()
		}
	}
	return nil, fmt.Errorf("max retries exceeded: %w", lastErr)
}

// downloadBlock fetches a single block from Telegram
func (r *TgFileReader) downloadBlock(ctx context.Context, offset int64) ([]byte, error) {
	res, err := r.api.UploadGetFile(ctx, &tg.UploadGetFileRequest{
		Location: r.location,
		Offset:   offset,
		Limit:    int(r.blockSize),
	})
	if err != nil {
		return nil, err
	}
	switch result := res.(type) {
	case *tg.UploadFile:
		return result.Bytes, nil
	default:
		return nil, fmt.Errorf("unexpected response: %T", res)
	}
}

// ============================================================
// APP
// ============================================================

type App struct {
	cfg      *Config
	db       *DB
	cache    *Cache
	pool     *BotPool
	mtPool   *MTProtoPool
	logger   *zap.Logger
}

// ============================================================
// TELEGRAM BOT HANDLERS
// ============================================================

func (a *App) dispatch(ctx context.Context, update tgbotapi.Update) {
	switch {
	case update.Message != nil:
		a.onMessage(ctx, update.Message)
	case update.CallbackQuery != nil:
		a.onCallback(ctx, update.CallbackQuery)
	}
}

func (a *App) onMessage(ctx context.Context, msg *tgbotapi.Message) {
	if !msg.Chat.IsPrivate() {
		return
	}
	userID := msg.From.ID

	// ── ADMIN ONLY PROTECTION ──
	if userID != a.cfg.AdminID {
		a.pool.send(msg.Chat.ID, "🔒 Private Bot\n\nYeh bot sirf personal use ke liye hai.")
		return
	}

	_ = a.db.upsertUser(ctx, &UserRecord{
		ID: userID, Username: msg.From.UserName, FirstName: msg.From.FirstName,
	})

	user, err := a.db.getUser(ctx, userID)
	if err == nil && user.IsBanned {
		a.pool.send(msg.Chat.ID, "⛔ You are banned.")
		return
	}

	if a.cfg.MainChannelID != 0 {
		ok, found := a.cache.getFsub(ctx, userID)
		if !found {
			ok, _ = a.pool.isMember(a.cfg.MainChannelID, userID)
			a.cache.setFsub(ctx, userID, ok)
		}
		if !ok {
			a.sendFsubPrompt(msg.Chat.ID)
			return
		}
	}

	switch {
	case msg.IsCommand():
		a.onCommand(ctx, msg)
	case msg.Document != nil || msg.Video != nil || msg.Audio != nil ||
		msg.Voice != nil || msg.VideoNote != nil || len(msg.Photo) > 0:
		a.onFile(ctx, msg)
	default:
		a.pool.send(msg.Chat.ID, "📎 Send me any file to get a permanent streaming link!")
	}
}

func (a *App) onCommand(ctx context.Context, msg *tgbotapi.Message) {
	switch msg.Command() {
	case "start":
		a.pool.sendMD(msg.Chat.ID, fmt.Sprintf(
			"👋 Hello *%s*\\!\n\nSend me any file \\(any size\\!\\) and get a permanent streaming link\\.\n\nWorks in Chrome, Firefox and VLC\\!",
			mdEscape(msg.From.FirstName),
		))
	case "help":
		a.pool.sendMD(msg.Chat.ID,
			"*Commands:*\n/start \\- Welcome\n/help \\- Help\n/stats \\- Stats \\(admin\\)\n/expire \\- Set/remove link expiry \\(admin\\)\n/setpass \\- Password\\-protect a link \\(uploader/admin\\)\n/approve \\- Approve a visitor's Access ID \\(admin\\)\n/block \\- Block a visitor's Access ID \\(admin\\)\n/unblock \\- Unblock a visitor's Access ID \\(admin\\)\n/user \\- List recent visitors and their Access IDs \\(admin\\)\n/clearpending \\- Delete all pending visitors \\(admin\\)\n/dashboard \\- Get admin dashboard link \\(admin\\)\n/dminem \\- Delete ALL files \\(admin, asks confirmation\\)\n\nSend any file to get a link\\!\n\nBy default links are *permanent*\\. Use /expire \\<file\\_id\\> \\<time\\> to make one expire \\(e\\.g\\. `7d`, `12h`, `1y`, or `off` to remove it\\)\\.")
	case "stats":
		if msg.From.ID != a.cfg.AdminID {
			a.pool.send(msg.Chat.ID, "❌ Admin only.")
			return
		}
		files, _ := a.db.countFiles(ctx)
		users, _ := a.db.countUsers(ctx)
		totalViews, _ := a.db.sumViews(ctx)
		live := a.cache.liveCountAll(ctx)
		a.pool.sendMD(msg.Chat.ID, fmt.Sprintf(
			"📊 *Stats*\n\n📁 Files: `%d`\n👥 Users: `%d`\n🤖 Bots: `%d`\n👁 Total unique views: `%d`\n🔴 Live now: `%d`",
			files, users, a.pool.count(), totalViews, live,
		))
	case "dashboard":
		if msg.From.ID != a.cfg.AdminID {
			a.pool.send(msg.Chat.ID, "❌ Admin only.")
			return
		}
		link := fmt.Sprintf("%s/admin?token=%s", a.cfg.baseURL(), a.cfg.DashboardToken)
		a.pool.send(msg.Chat.ID, fmt.Sprintf(
			"🖥️ Admin Dashboard\n\n%s\n\n⚠️ Yeh link kisi ko share mat karna — jiske paas yeh link hai woh dashboard dekh sakta hai.",
			link,
		))
	case "dminem":
		if msg.From.ID != a.cfg.AdminID {
			return
		}
		count, _ := a.db.countFiles(ctx)
		kb := tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("⚠️ Haan, SAB delete karo", "confirm_delete_all"),
				tgbotapi.NewInlineKeyboardButtonData("❌ Cancel", "cancel_delete_all"),
			),
		)
		a.pool.sendKB(msg.Chat.ID, fmt.Sprintf(
			"⚠️ Yeh *%d* files ko *PERMANENTLY* delete kar dega \\— sabhi links kaam karna band ho jaayenge\\.\n\nPakka delete karna hai?",
			count,
		), kb)
	case "setpass":
		parts := strings.Fields(msg.CommandArguments())
		if len(parts) < 2 {
			a.pool.send(msg.Chat.ID,
				"Usage: /setpass <file_id> <password>\nOr: /setpass <file_id> off   (remove password)")
			return
		}
		slug := parts[0]
		rec, err := a.db.getFileByID(ctx, slug)
		if err != nil {
			a.pool.send(msg.Chat.ID, "❌ File not found.")
			return
		}
		if msg.From.ID != a.cfg.AdminID && msg.From.ID != rec.UploaderID {
			a.pool.send(msg.Chat.ID, "❌ Sirf uploader ya admin password set kar sakta hai.")
			return
		}
		if strings.EqualFold(parts[1], "off") {
			if err := a.db.setPassword(ctx, slug, nil, nil); err != nil {
				a.pool.send(msg.Chat.ID, "❌ Failed to remove password.")
				return
			}
			a.cache.delFile(ctx, slug)
			a.pool.send(msg.Chat.ID, "✅ Password removed — link ab bina password ke khulega.")
			return
		}
		password := strings.Join(parts[1:], " ")
		hash := sha256Hex(password)
		if err := a.db.setPassword(ctx, slug, &hash, &password); err != nil {
			a.pool.send(msg.Chat.ID, "❌ Failed to set password.")
			return
		}
		a.cache.delFile(ctx, slug)
		a.pool.sendMD(msg.Chat.ID, fmt.Sprintf(
			"🔒 Password set for `%s`\\.\n\nAb koi bhi is link ko kholega, pehle yeh password daalna hoga\\.",
			mdEscape(slug),
		))
	case "user":
		if msg.From.ID != a.cfg.AdminID {
			a.pool.send(msg.Chat.ID, "❌ Admin only.")
			return
		}
		list, err := a.db.listApprovals(ctx, 30)
		if err != nil {
			a.pool.send(msg.Chat.ID, "❌ Failed to load visitors.")
			return
		}
		if len(list) == 0 {
			a.pool.send(msg.Chat.ID, "Abhi tak koi visitor nahi aaya.")
			return
		}
		var b strings.Builder
		b.WriteString("👥 *Recent Visitors*\n\n")
		for _, v := range list {
			name := v.VisitorName
			if name == "" {
				name = "—"
			}
			status := "⏳ pending"
			if v.Approved {
				status = "✅ approved"
			}
			if v.Blocked {
				status = "🚫 blocked"
			}
			fmt.Fprintf(&b, "🆔 `%05d` \\— %s \\(%s\\)\n", v.AccessID, mdEscape(name), status)
		}
		a.pool.sendMD(msg.Chat.ID, b.String())
	case "approve":
		if msg.From.ID != a.cfg.AdminID {
			a.pool.send(msg.Chat.ID, "❌ Admin only.")
			return
		}
		var accessID int
		if _, err := fmt.Sscanf(strings.TrimSpace(msg.CommandArguments()), "%d", &accessID); err != nil {
			a.pool.send(msg.Chat.ID, "Usage: /approve <access_id>\n\nAccess ID woh 5-digit number hai jo visitor ko password page par dikhta hai.")
			return
		}
		found, err := a.db.approveByID(ctx, accessID)
		if err != nil {
			a.pool.send(msg.Chat.ID, "❌ Approve failed, dobara try karo.")
			return
		}
		if !found {
			a.pool.send(msg.Chat.ID, fmt.Sprintf("❌ Access ID `%05d` nahi mila.", accessID))
			return
		}
		a.pool.sendMD(msg.Chat.ID, fmt.Sprintf(
			"✅ Access ID `%05d` approve ho gaya\\. Uska page apne aap unlock ho jaayega\\.",
			accessID,
		))
	case "ban":
		if msg.From.ID != a.cfg.AdminID {
			return
		}
		var id int64
		if _, err := fmt.Sscanf(msg.CommandArguments(), "%d", &id); err != nil {
			a.pool.send(msg.Chat.ID, "Usage: /ban <user_id>")
			return
		}
		_ = a.db.banUser(ctx, id, true)
		a.pool.send(msg.Chat.ID, fmt.Sprintf("✅ User %d banned.", id))
	case "unban":
		if msg.From.ID != a.cfg.AdminID {
			return
		}
		var id int64
		if _, err := fmt.Sscanf(msg.CommandArguments(), "%d", &id); err != nil {
			a.pool.send(msg.Chat.ID, "Usage: /unban <user_id>")
			return
		}
		_ = a.db.banUser(ctx, id, false)
		a.pool.send(msg.Chat.ID, fmt.Sprintf("✅ User %d unbanned.", id))
	case "block":
		if msg.From.ID != a.cfg.AdminID {
			a.pool.send(msg.Chat.ID, "❌ Admin only.")
			return
		}
		var accessID int
		if _, err := fmt.Sscanf(strings.TrimSpace(msg.CommandArguments()), "%d", &accessID); err != nil {
			a.pool.send(msg.Chat.ID, "Usage: /block <access_id>\n\nAccess ID woh 5-digit number hai jo /user list mein dikhta hai.")
			return
		}
		found, err := a.db.blockByID(ctx, accessID, true)
		if err != nil {
			a.pool.send(msg.Chat.ID, "❌ Block failed, dobara try karo.")
			return
		}
		if !found {
			a.pool.send(msg.Chat.ID, fmt.Sprintf("❌ Access ID `%05d` nahi mila.", accessID))
			return
		}
		a.pool.sendMD(msg.Chat.ID, fmt.Sprintf(
			"🚫 Access ID `%05d` block ho gaya\\. Yeh device ab kisi bhi link ko access nahi kar paayega\\.",
			accessID,
		))
	case "unblock":
		if msg.From.ID != a.cfg.AdminID {
			a.pool.send(msg.Chat.ID, "❌ Admin only.")
			return
		}
		var accessID int
		if _, err := fmt.Sscanf(strings.TrimSpace(msg.CommandArguments()), "%d", &accessID); err != nil {
			a.pool.send(msg.Chat.ID, "Usage: /unblock <access_id>")
			return
		}
		found, err := a.db.blockByID(ctx, accessID, false)
		if err != nil {
			a.pool.send(msg.Chat.ID, "❌ Unblock failed, dobara try karo.")
			return
		}
		if !found {
			a.pool.send(msg.Chat.ID, fmt.Sprintf("❌ Access ID `%05d` nahi mila.", accessID))
			return
		}
		a.pool.sendMD(msg.Chat.ID, fmt.Sprintf("✅ Access ID `%05d` unblock ho gaya\\.", accessID))
	case "clearpending":
		if msg.From.ID != a.cfg.AdminID {
			a.pool.send(msg.Chat.ID, "❌ Admin only.")
			return
		}
		n, err := a.db.deletePendingApprovals(ctx, nil)
		if err != nil {
			a.pool.send(msg.Chat.ID, "❌ Clear failed, dobara try karo.")
			return
		}
		a.pool.send(msg.Chat.ID, fmt.Sprintf("🧹 %d pending visitor(s) clear ho gaye. Approved aur blocked IDs safe hain.", n))
	case "delete":
		if msg.From.ID != a.cfg.AdminID {
			return
		}
		slug := strings.TrimSpace(msg.CommandArguments())
		if slug == "" {
			a.pool.send(msg.Chat.ID, "Usage: /delete <file_id>")
			return
		}
		rec, err := a.db.getFileByID(ctx, slug)
		if err != nil {
			a.pool.send(msg.Chat.ID, "❌ File not found.")
			return
		}
		a.db.deleteFileByMsgID(ctx, rec.MessageID) //nolint:errcheck
		a.cache.delFile(ctx, slug)
		a.pool.send(msg.Chat.ID, "✅ File deleted.")
	case "expire":
		if msg.From.ID != a.cfg.AdminID {
			return
		}
		parts := strings.Fields(msg.CommandArguments())
		if len(parts) != 2 {
			a.pool.send(msg.Chat.ID,
				"Usage: /expire <file_id> <duration>\n\n"+
					"Examples:\n"+
					"/expire abc123 30m   (30 minutes)\n"+
					"/expire abc123 12h   (12 hours)\n"+
					"/expire abc123 7d    (7 days)\n"+
					"/expire abc123 1y    (1 year)\n"+
					"/expire abc123 off   (remove expiry — link becomes permanent again)")
			return
		}
		slug, durStr := parts[0], parts[1]

		if _, err := a.db.getFileByID(ctx, slug); err != nil {
			a.pool.send(msg.Chat.ID, "❌ File not found.")
			return
		}

		dur, clear, err := parseExpiryDuration(durStr)
		if err != nil {
			a.pool.send(msg.Chat.ID, "❌ "+err.Error())
			return
		}

		if clear {
			if err := a.db.setExpiry(ctx, slug, nil); err != nil {
				a.pool.send(msg.Chat.ID, "❌ Failed to update expiry.")
				return
			}
			a.cache.delFile(ctx, slug)
			a.pool.send(msg.Chat.ID, "✅ Expiry removed — link is permanent (unlimited) again.")
			return
		}

		expiresAt := time.Now().Add(dur)
		if err := a.db.setExpiry(ctx, slug, &expiresAt); err != nil {
			a.pool.send(msg.Chat.ID, "❌ Failed to set expiry.")
			return
		}
		a.cache.delFile(ctx, slug)
		a.pool.sendMD(msg.Chat.ID, fmt.Sprintf(
			"✅ Expiry set for `%s`\\.\n\n⏳ Link expires: *%s*",
			mdEscape(slug), mdEscape(expiresAt.Format("02 Jan 2006, 15:04 MST")),
		))
	}
}

func (a *App) onFile(ctx context.Context, msg *tgbotapi.Message) {
	type fInfo struct {
		fileName string
		fileSize int64
		mimeType string
	}

	var fi fInfo
	switch {
	case msg.Document != nil:
		d := msg.Document
		name := d.FileName
		if name == "" {
			name = "document_" + d.FileUniqueID[:8]
		}
		fi = fInfo{name, int64(d.FileSize), d.MimeType}
	case msg.Video != nil:
		v := msg.Video
		name := v.FileName
		if name == "" {
			name = "video_" + v.FileUniqueID[:8] + ".mp4"
		}
		fi = fInfo{name, int64(v.FileSize), "video/mp4"}
	case msg.Audio != nil:
		au := msg.Audio
		name := au.FileName
		if name == "" {
			name = au.Performer + " - " + au.Title + ".mp3"
		}
		fi = fInfo{name, int64(au.FileSize), "audio/mpeg"}
	case msg.Voice != nil:
		v := msg.Voice
		fi = fInfo{"voice_" + v.FileUniqueID[:8] + ".ogg", int64(v.FileSize), "audio/ogg"}
	case msg.VideoNote != nil:
		vn := msg.VideoNote
		fi = fInfo{"videonote_" + vn.FileUniqueID[:8] + ".mp4", int64(vn.FileSize), "video/mp4"}
	case len(msg.Photo) > 0:
		ph := msg.Photo[len(msg.Photo)-1]
		fi = fInfo{"photo_" + ph.FileUniqueID[:8] + ".jpg", int64(ph.FileSize), "image/jpeg"}
	default:
		a.pool.send(msg.Chat.ID, "⚠️ Unsupported file type.")
		return
	}
	if fi.mimeType == "" {
		fi.mimeType = "application/octet-stream"
	}

	procMsg, _ := a.pool.primary().Send(tgbotapi.NewMessage(msg.Chat.ID, "⏳ Processing..."))

	// Forward to storage channel — get the forwarded message ID
	fwdMsg, err := a.pool.next().Send(tgbotapi.NewForward(a.cfg.DBChannelID, msg.Chat.ID, msg.MessageID))
	if err != nil {
		a.logger.Error("forward failed", zap.Error(err))
		if procMsg.MessageID != 0 {
			a.pool.delMsg(msg.Chat.ID, procMsg.MessageID)
		}
		a.pool.send(msg.Chat.ID, "❌ Failed to store file. Try again.")
		return
	}

	// Generate a short hash for URL verification (like fast-stream-bot)
	hash := makeShortHash(fi.fileName, fi.fileSize, fwdMsg.MessageID)
	slug := uuid.New().String()

	// Convert channel ID: -1001234567890 → 1234567890
	channelID := toInternalChannelID(a.cfg.DBChannelID)

	rec := &FileRecord{
		ID:           slug,
		MessageID:    fwdMsg.MessageID,
		ChannelID:    channelID,
		FileName:     fi.fileName,
		FileSize:     fi.fileSize,
		MimeType:     fi.mimeType,
		Hash:         hash,
		UploaderID:   msg.From.ID,
		UploaderName: msg.From.UserName,
	}

	if err := a.db.saveFile(ctx, rec); err != nil {
		a.logger.Error("db save failed", zap.Error(err))
		if procMsg.MessageID != 0 {
			a.pool.delMsg(msg.Chat.ID, procMsg.MessageID)
		}
		a.pool.send(msg.Chat.ID, "❌ Database error. Try again.")
		return
	}

	a.cache.setFile(ctx, slug, &cachedFile{
		MessageID: fwdMsg.MessageID,
		ChannelID: channelID,
		FileName:  fi.fileName,
		FileSize:  fi.fileSize,
		MimeType:  fi.mimeType,
		Hash:      hash,
	})

	if procMsg.MessageID != 0 {
		a.pool.delMsg(msg.Chat.ID, procMsg.MessageID)
	}

	base := a.cfg.baseURL()
	streamLink := fmt.Sprintf("%s/stream/%s", base, slug)
	dlLink := fmt.Sprintf("%s/dl/%s", base, slug)
	watchLink := fmt.Sprintf("%s/watch/%s", base, slug)

	text := fmt.Sprintf(
		"✅ *File Stored\\!*\n\n📄 `%s`\n📦 `%s`\n🆔 `%s`\n\n▶️ [Stream](%s)\n⬇️ [Download](%s)\n📺 [Watch Online](%s)\n\n🔒 Permanent link\\!\n\n_Tap the ID above to copy it — use it with /setpass, /expire, /delete_",
		mdEscape(fi.fileName), formatSize(fi.fileSize), slug, streamLink, dlLink, watchLink,
	)
	kb := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonURL("▶️ Stream", streamLink),
			tgbotapi.NewInlineKeyboardButtonURL("⬇️ Download", dlLink),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonURL("📺 Watch Online", watchLink),
		),
	)
	a.pool.sendKB(msg.Chat.ID, text, kb)

	if a.cfg.LogChannelID != 0 {
		a.pool.sendMD(a.cfg.LogChannelID, fmt.Sprintf(
			"📁 *New File*\n👤 [%s](tg://user?id=%d)\n📄 `%s`\n📦 `%s`\n🔗 %s",
			mdEscape(msg.From.FirstName), msg.From.ID,
			mdEscape(fi.fileName), formatSize(fi.fileSize), streamLink,
		))
	}
}

func (a *App) onCallback(ctx context.Context, cb *tgbotapi.CallbackQuery) {
	if cb.Data == "confirm_delete_all" {
		if cb.From.ID != a.cfg.AdminID {
			a.pool.primary().Request(tgbotapi.NewCallback(cb.ID, "❌ Admin only.")) //nolint:errcheck
			return
		}
		n, err := a.db.deleteAllFiles(ctx)
		a.cache.clearAllFileCache(ctx)
		if err != nil {
			a.pool.primary().Request(tgbotapi.NewCallback(cb.ID, "❌ Failed.")) //nolint:errcheck
			edit := tgbotapi.NewEditMessageText(cb.Message.Chat.ID, cb.Message.MessageID, "❌ Delete failed — try again.")
			a.pool.primary().Send(edit) //nolint:errcheck
			return
		}
		a.pool.primary().Request(tgbotapi.NewCallback(cb.ID, "✅ Deleted!")) //nolint:errcheck
		edit := tgbotapi.NewEditMessageText(cb.Message.Chat.ID, cb.Message.MessageID,
			fmt.Sprintf("🗑️ %d files permanently deleted. All links are now dead.", n))
		a.pool.primary().Send(edit) //nolint:errcheck
		return
	}
	if cb.Data == "cancel_delete_all" {
		a.pool.primary().Request(tgbotapi.NewCallback(cb.ID, "Cancelled.")) //nolint:errcheck
		edit := tgbotapi.NewEditMessageText(cb.Message.Chat.ID, cb.Message.MessageID, "❌ Cancelled — koi file delete nahi hui.")
		a.pool.primary().Send(edit) //nolint:errcheck
		return
	}
	if cb.Data == "verify_fsub" {
		a.cache.delFsub(ctx, cb.From.ID)
		ok, _ := a.pool.isMember(a.cfg.MainChannelID, cb.From.ID)
		a.cache.setFsub(ctx, cb.From.ID, ok)
		if !ok {
			ans := tgbotapi.NewCallback(cb.ID, "❌ Not joined yet!")
			a.pool.primary().Request(ans) //nolint:errcheck
			return
		}
		ans := tgbotapi.NewCallback(cb.ID, "✅ Verified!")
		a.pool.primary().Request(ans) //nolint:errcheck
		edit := tgbotapi.NewEditMessageText(cb.Message.Chat.ID, cb.Message.MessageID,
			"✅ *Access granted\\!* Send me a file now\\.")
		edit.ParseMode = "MarkdownV2"
		a.pool.primary().Send(edit) //nolint:errcheck
		return
	}
	a.pool.primary().Request(tgbotapi.NewCallback(cb.ID, "")) //nolint:errcheck
}

func (a *App) sendFsubPrompt(chatID int64) {
	chID := a.cfg.MainChannelID
	if chID < 0 {
		chID = -chID - 1000000000000
	}
	kb := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonURL("📢 Join Channel",
				fmt.Sprintf("https://t.me/c/%d", chID)),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("✅ I Joined — Verify", "verify_fsub"),
		),
	)
	a.pool.sendKB(chatID, "🔒 *Join Required*\n\nJoin our channel first, then press Verify\\.", kb)
}

// ============================================================
// HTTP STREAMING — Uses TgFileReader just like fast-stream-bot!
// ============================================================

func (a *App) serveHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
	w.Header().Set("Access-Control-Expose-Headers", "Content-Length, Content-Range, Accept-Ranges, Content-Type")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	path := r.URL.Path
	switch {
	case path == "/" || path == "":
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<html><body style="background:#1b1c26;color:#7aa2f7;font-family:monospace;text-align:center;padding:50px">
<h1>⚡ RAJ File Stream Bot</h1>
<p style="color:#8b949e">Supports ANY file size!</p>
<p style="color:#484f58;font-size:12px">%s/stream/{id} | %s/dl/{id}</p>
</body></html>`, a.cfg.baseURL(), a.cfg.baseURL())

	case path == "/health" || path == "/ping":
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"ok","mtproto":%t}`, a.mtPool.isAnyReady())

	case strings.HasPrefix(path, "/stream/"):
		slug := strings.TrimSuffix(strings.TrimPrefix(path, "/stream/"), "/")
		a.handleStream(w, r, slug, false)

	case strings.HasPrefix(path, "/dl/"):
		slug := strings.TrimSuffix(strings.TrimPrefix(path, "/dl/"), "/")
		a.handleStream(w, r, slug, true)

	case strings.HasPrefix(path, "/watch/"):
		slug := strings.TrimSuffix(strings.TrimPrefix(path, "/watch/"), "/")
		a.handleWatch(w, r, slug)

	case strings.HasPrefix(path, "/heartbeat/"):
		slug := strings.TrimSuffix(strings.TrimPrefix(path, "/heartbeat/"), "/")
		a.handleHeartbeat(w, r, slug)

	case strings.HasPrefix(path, "/livecount/"):
		slug := strings.TrimSuffix(strings.TrimPrefix(path, "/livecount/"), "/")
		a.handleLiveCount(w, r, slug)

	case path == "/admin":
		a.handleAdmin(w, r)

	default:
		http.NotFound(w, r)
	}
}

// handleHeartbeat — watch page se har ~15s mein call hota hai taaki
// "abhi kitne log dekh rahe hain" real-time pata chal sake.
func (a *App) handleHeartbeat(w http.ResponseWriter, r *http.Request, slug string) {
	if slug == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	deviceID := getOrSetDeviceID(w, r)
	a.cache.heartbeat(r.Context(), slug, deviceID)
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, `{"ok":true}`)
}

// handleLiveCount returns how many devices are currently (last 30s) watching this file.
func (a *App) handleLiveCount(w http.ResponseWriter, r *http.Request, slug string) {
	if slug == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	n := a.cache.liveCount(r.Context(), slug)
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"live":%d}`, n)
}

func (a *App) handleStream(w http.ResponseWriter, r *http.Request, slug string, download bool) {
	ctx := r.Context()

	if slug == "" {
		http.Error(w, "Missing file ID", http.StatusBadRequest)
		return
	}

	// Resolve metadata from cache or DB
	var messageID int
	var channelID int64
	var fileName, mimeType string
	var fileSize int64
	var expiresAt *time.Time
	var passwordHash *string

	cached := a.cache.getFile(ctx, slug)
	if cached != nil {
		messageID = cached.MessageID
		channelID = cached.ChannelID
		fileName = cached.FileName
		fileSize = cached.FileSize
		mimeType = cached.MimeType
		expiresAt = cached.ExpiresAt
		passwordHash = cached.PasswordHash
	} else {
		rec, err := a.db.getFileByID(ctx, slug)
		if err != nil {
			http.Error(w, "File not found.", http.StatusNotFound)
			return
		}
		messageID = rec.MessageID
		channelID = rec.ChannelID
		fileName = rec.FileName
		fileSize = rec.FileSize
		mimeType = rec.MimeType
		expiresAt = rec.ExpiresAt
		passwordHash = rec.PasswordHash
		a.cache.setFile(ctx, slug, &cachedFile{
			MessageID: messageID, ChannelID: channelID,
			FileName: fileName, FileSize: fileSize, MimeType: mimeType,
			ExpiresAt: expiresAt, PasswordHash: passwordHash,
		})
	}

	if isExpired(expiresAt) {
		http.Error(w, "⏳ This link has expired.", http.StatusGone)
		return
	}

	// Password-protected link — the watch page sets a cookie after the
	// correct password is entered; the <video>/download request carries
	// that same cookie automatically since it's same-origin.
	//
	// Sliding window: the cookie is only valid for 60s, but every time a
	// chunk request comes in with a valid cookie we refresh it for
	// another 60s. So as long as the user keeps actively watching, the
	// stream never breaks — but the moment they close the tab / stop
	// requesting for 1 minute, it expires and they need the password again.
	if passwordHash != nil && *passwordHash != "" {
		if !hasValidPasswordCookie(r, slug, *passwordHash) {
			http.Error(w, "🔒 This link is password protected. Open the /watch page and enter the password first.", http.StatusUnauthorized)
			return
		}
		setPasswordCookie(w, slug, *passwordHash)
	}

	// Access-ID approval gate — same rule as /watch: applies whether or not
	// this file has a password, and closes the loophole where a
	// password-less link (or a direct /stream hit bypassing /watch
	// entirely) could stream without ever being approved.
	deviceID := getOrSetDeviceID(w, r)
	approval, isNewDevice, aErr := a.db.getOrCreateApproval(ctx, deviceID, slug)
	if aErr != nil {
		a.logger.Warn("approval lookup failed (stream)", zap.Error(aErr))
	}
	if approval != nil && approval.Blocked {
		http.Error(w, "🚫 Access blocked.", http.StatusForbidden)
		return
	}
	if approval == nil || !approval.Approved {
		accessID := 0
		visitorName := ""
		if approval != nil {
			accessID = approval.AccessID
			visitorName = approval.VisitorName
		}
		a.notifyNewAccessID(isNewDevice, accessID, fileName, visitorName)
		http.Error(w, fmt.Sprintf(
			"🔒 Not approved yet. Open the /watch page for this link — your Access ID is %05d. Ask admin to run /approve %05d.",
			accessID, accessID), http.StatusUnauthorized)
		return
	}

	if mimeType == "" {
		mimeType = "application/octet-stream"
	}

	// Get file location via MTProto — same as fast-stream-bot!
	if !a.mtPool.isAnyReady() {
		slog.Warn("MTProto not ready yet, waiting...")
		for i := 0; i < 10; i++ {
			time.Sleep(1 * time.Second)
			if a.mtPool.isAnyReady() {
				break
			}
		}
		if !a.mtPool.isAnyReady() {
			http.Error(w, "Service starting up, please retry in a moment.", http.StatusServiceUnavailable)
			return
		}
	}

	location, tgSize, err := a.mtPool.getFileLocation(ctx, channelID, messageID)
	if err != nil {
		a.logger.Error("getFileLocation failed",
			zap.Int("message_id", messageID),
			zap.Int64("channel_id", channelID),
			zap.Error(err),
		)
		http.Error(w, "Cannot retrieve file from Telegram.", http.StatusBadGateway)
		return
	}

	// Use actual size from Telegram if we don't have it
	if fileSize <= 0 && tgSize > 0 {
		fileSize = tgSize
	}

	// Parse Range header — same as fast-stream-bot!
	start, end := int64(0), fileSize-1
	isRange := false
	rangeHeader := r.Header.Get("Range")
	if rangeHeader != "" && fileSize > 0 {
		rng := strings.TrimPrefix(rangeHeader, "bytes=")
		parts := strings.SplitN(rng, "-", 2)
		if len(parts) == 2 {
			s := strings.TrimSpace(parts[0])
			e := strings.TrimSpace(parts[1])
			if s != "" {
				start, _ = strconv.ParseInt(s, 10, 64)
			}
			if e != "" {
				end, _ = strconv.ParseInt(e, 10, 64)
			}
			if end >= fileSize {
				end = fileSize - 1
			}
			if start >= 0 && start <= end {
				isRange = true
			}
		}
	}

	// Set response headers — same as fast-stream-bot's SetupStream!
	disposition := "inline"
	if download {
		disposition = "attachment"
	}
	w.Header().Set("Content-Type", mimeType)
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`%s; filename="%s"`, disposition, sanitizeName(fileName)))

	contentLength := end - start + 1
	w.Header().Set("Content-Length", strconv.FormatInt(contentLength, 10))

	if isRange {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, fileSize))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.WriteHeader(http.StatusOK)
	}

	if r.Method == http.MethodHead {
		return
	}

	// Get MTProto bot for streaming
	bot := a.mtPool.next()
	bot.mu.Lock()
	api := bot.api
	bot.mu.Unlock()

	// Create TgFileReader — exactly like fast-stream-bot!
	reader := newTgFileReader(ctx, api, a.cfg, location, fileSize, start, end)

	// Stream exact bytes using io.CopyN — like fast-stream-bot's routes/stream.go!
	defer reader.Close()
	if _, err := io.CopyN(w, reader, contentLength); err != nil {
		// Client disconnected — normal, ignore
		a.logger.Debug("stream ended", zap.Error(err))
	}
}

// ============================================================
// WATCH PAGE — html/template powered player page
// ============================================================

type WatchData struct {
	FileName    string
	FileSize    string
	MimeType    string
	StreamURL   string
	DownloadURL string
	ViewCount   string
	LiveCount   string
	DeviceID    string
	Slug        string
}

// renderPasswordPrompt shows a small standalone form asking for the link's
// password. Deliberately not part of index.html — it's a lightweight gate,
// not the full player page.
func renderPasswordPrompt(w http.ResponseWriter, slug string, wrong bool, accessID int, prefillName string, videoURL string, promptImages []string, contactTelegram, contactInstagram string) {
	msg := ""
	if wrong {
		msg = `<div style="color:#ff6b7a;margin:0 0 14px;font-size:13.5px;font-weight:600;
background:rgba(255,107,122,.1);border:1px solid rgba(255,107,122,.3);border-radius:8px;padding:10px 12px;">
❌ Galat password, dobara try karo.</div>`
	}
	// Optional instructional video — shown above the password box when
	// PASSWORD_PROMPT_VIDEO_URL is configured. autoplay+muted+loop so it
	// plays without a click (browsers block unmuted autoplay); playsinline
	// keeps it from going fullscreen on iOS. A tap-to-unmute pill sits over
	// the bottom-right corner since sound can't autoplay.
	videoBlock := ""
	if videoURL != "" {
		videoBlock = fmt.Sprintf(`
<div style="position:relative;margin:14px 0 16px;border-radius:12px;overflow:hidden;
box-shadow:0 0 0 1px rgba(56,214,255,.25),0 8px 24px rgba(0,0,0,.5);">
<video id="ppv" src="%s" autoplay muted loop playsinline
style="width:100%%;display:block;background:#000;"></video>
<button type="button" onclick="var v=document.getElementById('ppv');v.muted=!v.muted;this.textContent=v.muted?'🔇 Tap for sound':'🔊 Sound on';"
style="position:absolute;bottom:10px;right:10px;background:rgba(6,8,14,.75);color:#fff;
border:1px solid rgba(255,255,255,.25);backdrop-filter:blur(4px);
border-radius:20px;padding:6px 14px;font-size:12.5px;font-weight:600;cursor:pointer;">🔇 Tap for sound</button>
</div>`,
			html.EscapeString(videoURL))
	}
	// Random auto-changing image — shown below the password box. Cycles
	// through the configured image list every few seconds via JS, picking
	// a random one each time (not just sequential).
	imageBlock := ""
	if len(promptImages) > 0 {
		var urls strings.Builder
		for i, u := range promptImages {
			if i > 0 {
				urls.WriteString(",")
			}
			urls.WriteString(fmt.Sprintf("%q", u))
		}
		imageBlock = fmt.Sprintf(`
<div style="margin-top:16px;">
<img id="ppimg" src="%s" style="width:100%%;border-radius:10px;display:block;
box-shadow:0 4px 16px rgba(0,0,0,.4);transition:opacity .4s ease;">
</div>
<script>
(function(){
var imgs=[%s];
var el=document.getElementById('ppimg');
if(!el||imgs.length<2)return;
setInterval(function(){
var next=imgs[Math.floor(Math.random()*imgs.length)];
el.style.opacity=0;
setTimeout(function(){el.src=next;el.style.opacity=1;},400);
},4000);
})();
</script>`, html.EscapeString(promptImages[0]), urls.String())
	}
	// Contact info — visible below the password box so people know where
	// to ask for the password. Same real Telegram/Instagram brand-icon
	// buttons as the pending-approval page, not plain emoji text links.
	contactBlock := ""
	if contactTelegram != "" || contactInstagram != "" {
		var links strings.Builder
		if contactTelegram != "" {
			fmt.Fprintf(&links, `
<a href="https://t.me/%s" target="_blank" rel="noopener" class="ct-btn tg">
<i class="fa-brands fa-telegram brand"></i><span class="label">@%s</span></a>`,
				url.QueryEscape(contactTelegram), html.EscapeString(contactTelegram))
		}
		if contactInstagram != "" {
			fmt.Fprintf(&links, `
<a href="https://instagram.com/%s" target="_blank" rel="noopener" class="ct-btn ig">
<i class="fa-brands fa-instagram brand"></i><span class="label">@%s</span></a>`,
				url.QueryEscape(contactInstagram), html.EscapeString(contactInstagram))
		}
		contactBlock = fmt.Sprintf(`
<div style="margin-top:16px;padding-top:14px;border-top:1px solid #1c2130;">
%s
</div>`, links.String())
	}

	// Subjects/topics tiled faintly across the background — signals "this
	// whole site is an education/coding platform" at a glance, before the
	// visitor even reads a word.
	subjects := []string{
		"MATH", "PHYSICS", "CHEMISTRY", "BIOLOGY", "SCIENCE", "ENGLISH",
		"PYTHON", "C++", "JAVASCRIPT", "CSS", "LINUX", "HTML",
		"HISTORY", "GEOGRAPHY", "ALGEBRA", "GIT", "SQL", "DSA",
	}
	var subjectTiles strings.Builder
	for _, s := range subjects {
		fmt.Fprintf(&subjectTiles, `<span class="stile">%s</span>`, html.EscapeString(s))
	}

	// Every visitor gets their own unique 5-digit Access ID shown right here.
	// They send this number to the admin, who runs /approve <id> in the bot —
	// only then can this specific device actually stream, even after the
	// password is correct.
	accessIDBlock := fmt.Sprintf(`
<div style="margin:0 0 16px;padding:12px;border-radius:10px;background:#0d1322;border:1px solid #1a2436;text-align:center;">
  <div style="font-size:10.5px;letter-spacing:.5px;color:#6b7bab;margin-bottom:4px;">YOUR ACCESS ID — send this to admin for approval</div>
  <div style="font-family:'JetBrains Mono',monospace;font-size:22px;font-weight:800;color:#38d6ff;letter-spacing:3px;">%05d</div>
</div>`, accessID)

	// Telegram contact used both by the "join as dev" footer and by the
	// visible contact block above — falls back to raj_dev_01 if unset.
	devContact := contactTelegram
	if devContact == "" {
		devContact = "raj_dev_01"
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!DOCTYPE html><html><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>🔒 Password Required — Astratoonix</title>
<link href="https://fonts.googleapis.com/css2?family=JetBrains+Mono:wght@500;700;800&family=Inter:wght@400;500;600;700&display=swap" rel="stylesheet">
<link rel="stylesheet" href="https://cdnjs.cloudflare.com/ajax/libs/font-awesome/6.5.1/css/all.min.css">
<style>
.ct-btn {
  display:flex; align-items:center; gap:10px; text-decoration:none; color:#fff;
  padding:11px 14px; border-radius:12px; margin-bottom:10px; font-size:13.5px; font-weight:600;
  font-family:'Inter',sans-serif;
}
.ct-btn:last-child { margin-bottom:0; }
.ct-btn.tg { background:#0088cc; }
.ct-btn.ig { background:linear-gradient(45deg,#f09433,#e6683c,#dc2743,#cc2366,#bc1888); }
.ct-btn i.brand { font-size:16px; flex-shrink:0; }
.ct-btn span.label { flex:1; text-align:left; }
* { margin:0; padding:0; box-sizing:border-box; }
html,body { height:100%%; }
body {
  background:#060810; color:#e7ecf7; font-family:'Inter',system-ui,sans-serif;
  min-height:100vh; overflow-x:hidden;
}
.stage {
  position:relative; min-height:100vh; width:100%%;
  display:flex; align-items:center; justify-content:center; padding:22px 14px;
  border:3px solid #17324a; box-sizing:border-box;
}
.stage::before {
  content:''; position:fixed; inset:0; z-index:0; pointer-events:none;
  background:
    radial-gradient(circle at 15%% 20%%, rgba(56,214,255,.10) 0%%, transparent 45%%),
    radial-gradient(circle at 85%% 80%%, rgba(155,92,255,.10) 0%%, transparent 45%%);
}
.subject-grid {
  position:fixed; inset:0; z-index:0; pointer-events:none;
  display:flex; flex-wrap:wrap; align-content:center; justify-content:center;
  gap:26px 34px; padding:40px; opacity:0.05; overflow:hidden;
}
.stile {
  font-family:'JetBrains Mono',monospace; font-weight:800; font-size:26px;
  letter-spacing:2px; color:#38d6ff; white-space:nowrap;
}

/* ---------- Boot / intro splash ---------- */
#boot {
  position:fixed; inset:0; z-index:999; background:#060810;
  display:flex; align-items:center; justify-content:center; flex-direction:column; gap:10px;
  font-family:'JetBrains Mono',monospace;
  animation: bootOut .5s ease .1s forwards; animation-play-state:paused;
}
#boot .brand {
  font-size:clamp(26px,7vw,44px); font-weight:800; letter-spacing:2px; color:#38d6ff;
  border-right:3px solid #38d6ff; white-space:nowrap; overflow:hidden; width:0;
  animation: typeBrand 1.1s steps(11,end) forwards, caretBlink .8s step-end infinite;
}
#boot .by {
  font-size:14px; letter-spacing:4px; color:#6b7bab; opacity:0;
  animation: fadeIn .5s ease 1.3s forwards;
}
#boot .by b { color:#9d7cf7; }
@keyframes typeBrand { from{width:0} to{width:11ch} }
@keyframes caretBlink { 50%%{border-color:transparent} }
@keyframes fadeIn { to{opacity:1} }
@keyframes bootOut { to{opacity:0;visibility:hidden;pointer-events:none;} }

.card {
  position:relative; z-index:1; width:min(400px,100%%);
  background:linear-gradient(180deg,#0b0f1a,#0a0d16);
  border:1px solid #182034; border-radius:18px; padding:26px 24px 22px;
  box-shadow:0 20px 60px rgba(0,0,0,.55), 0 0 0 1px rgba(56,214,255,.08);
  opacity:0; transform:translateY(10px); animation:cardIn .5s ease 1.7s forwards;
}
@keyframes cardIn { to{opacity:1;transform:translateY(0)} }

.badges { display:flex; gap:6px; flex-wrap:wrap; margin-bottom:16px; }
.badge {
  font-size:10.5px; font-weight:700; letter-spacing:.4px; padding:5px 9px; border-radius:20px;
  background:#0f1a2a; border:1px solid #1d3350; color:#6fe3ff;
  display:inline-flex; align-items:center; gap:5px;
}
.badge.free { color:#7dffb0; border-color:#1f4632; background:#0d1c15; }
.badge.https { color:#ffd166; border-color:#4a3a12; background:#1c1608; }

.lock-icon { font-size:30px; margin-bottom:6px; }
h2.title { font-size:17px; font-weight:700; margin-bottom:4px; }
.subtitle { font-size:12px; color:#6b7bab; margin-bottom:16px; line-height:1.5; }
.subtitle b { color:#9db3d9; }

input[type=password], input[type=text] {
  width:100%%; box-sizing:border-box; padding:13px 14px; border-radius:10px;
  border:1px solid #1c2537; background:#060810; color:#fff; font-size:15px;
  margin-bottom:12px; font-family:'Inter',sans-serif;
  transition:box-shadow .2s ease, color .2s ease;
}
input[type=password]:focus, input[type=text]:focus { outline:none; border-color:#38d6ff; box-shadow:0 0 6px rgba(56,214,255,.65),0 0 16px rgba(157,124,247,.35); }
/* Typed characters glow neon as you type */
input[type=password]:not(:placeholder-shown), input[type=text]:not(:placeholder-shown) { color:#7cf3ff; text-shadow:0 0 6px rgba(56,214,255,.7); }
button.unlock {
  width:100%%; padding:13px; border:none; border-radius:10px;
  background:linear-gradient(135deg,#38d6ff,#9d7cf7); color:#060810;
  font-weight:800; font-size:15px; cursor:pointer; letter-spacing:.3px;
}
button.unlock:active { transform:scale(.98); }

.stack-line {
  margin-top:16px; padding-top:14px; border-top:1px solid #131a2a;
  display:flex; flex-wrap:wrap; gap:6px; justify-content:center;
}
.stack-chip {
  font-size:10.5px; font-weight:700; color:#8ea3cf; background:#0d1322;
  border:1px solid #1a2436; border-radius:6px; padding:4px 8px; font-family:'JetBrains Mono',monospace;
}

/* ---------- Bottom collab / commission strip ---------- */
.collab {
  position:relative; z-index:1; width:min(400px,100%%); margin-top:16px;
  background:linear-gradient(135deg,#1c1608,#0b0f1a); border:1px solid #4a3a12;
  border-radius:14px; padding:16px 18px; text-align:center;
}
.collab .h { font-size:13px; font-weight:800; color:#ffd166; margin-bottom:5px; }
.collab .b { font-size:12px; color:#c9b98f; line-height:1.6; margin-bottom:10px; }
.collab a.cta {
  display:inline-flex; align-items:center; gap:6px; text-decoration:none;
  background:#ffd166; color:#1c1608; font-weight:800; font-size:12.5px;
  padding:8px 16px; border-radius:20px;
}

@media (prefers-reduced-motion: reduce) {
  #boot, #boot .brand, #boot .by, .card { animation:none !important; opacity:1 !important; width:auto !important; transform:none !important; }
}
</style></head>
<body>

<div id="boot">
  <div class="brand">ASTRATOONIX</div>
  <div class="by">BUILT BY <b>RAJ</b></div>
</div>

<div class="subject-grid">%s</div>

<div class="stage">
  <div style="display:flex;flex-direction:column;align-items:center;width:100%%;">
    <div class="card">
      <div class="badges">
        <span class="badge">🔒 PRIVATE</span>
        <span class="badge https">🔐 HTTPS</span>
        <span class="badge free">💯 FREE</span>
      </div>
      <div class="lock-icon">🔒</div>
      <h2 class="title">Password Protected Link</h2>
      <div class="subtitle">Ek <b>education platform</b> — Science, Math aur coding (Python, C++, Linux, JS, CSS) sab kuchh yahin milega.</div>
      %s
      %s
      %s
      <form method="GET" action="/watch/%s">
        <input type="text" name="name" placeholder="Your name" value="%s" autocomplete="off" autofocus required>
        <input type="password" name="pw" placeholder="Enter password" required>
        <button type="submit" class="unlock">Unlock</button>
      </form>
      %s
      <div class="stack-line">
        <span class="stack-chip">PYTHON</span>
        <span class="stack-chip">C++</span>
        <span class="stack-chip">LINUX</span>
        <span class="stack-chip">JS</span>
        <span class="stack-chip">CSS</span>
      </div>
      %s
    </div>

    <div class="collab">
      <div class="h">🤝 Devs wanted — earn commission</div>
      <div class="b">C++ ya web scraping aata hai? Website banane mein help karo — achcha commission milega, plus poora complete course (in English) free.</div>
      <a class="cta" href="https://t.me/%s" target="_blank">✈️ DM @%s</a>
    </div>
  </div>
</div>

<script>
document.getElementById('boot').style.animationPlayState='running';
</script>
</body></html>`, subjectTiles.String(), videoBlock, msg, accessIDBlock, slug, html.EscapeString(prefillName), imageBlock, contactBlock, url.QueryEscape(devContact), html.EscapeString(devContact))
}

// renderPendingApproval is shown when the password was correct but the
// admin hasn't run /approve <accessID> for this device yet. Auto-refreshes
// every 6s so access appears automatically the moment it's approved,
// without the visitor having to do anything. Shows Telegram + Instagram
// contact buttons (real brand icons, not generic camera/photo icons) so the
// visitor can send their Access ID straight to the admin, and a small
// EN/HI/BN switcher since text defaults to English.
func renderPendingApproval(w http.ResponseWriter, slug string, accessID int, contactTelegram, contactInstagram string) {
	tgUser := contactTelegram
	if tgUser == "" {
		tgUser = "raj_dev_01"
	}
	igUser := contactInstagram
	if igUser == "" {
		igUser = "raj_dev_01"
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!DOCTYPE html><html><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta http-equiv="refresh" content="6">
<title>Waiting for Approval</title>
<link href="https://fonts.googleapis.com/css2?family=JetBrains+Mono:wght@700;800&family=Inter:wght@400;600;700&display=swap" rel="stylesheet">
<link rel="stylesheet" href="https://cdnjs.cloudflare.com/ajax/libs/font-awesome/6.5.1/css/all.min.css">
<style>
* { margin:0; padding:0; box-sizing:border-box; }
body {
  background:#060810; color:#e7ecf7; font-family:'Inter',system-ui,sans-serif;
  min-height:100vh; display:flex; align-items:center; justify-content:center; padding:20px;
}
.card {
  width:min(380px,100%%); background:linear-gradient(180deg,#0b0f1a,#0a0d16);
  border:1px solid #182034; border-radius:18px; padding:30px 24px; text-align:center;
  box-shadow:0 20px 60px rgba(0,0,0,.55);
}
.spin {
  width:36px; height:36px; margin:0 auto 16px; border-radius:50%%;
  border:3px solid rgba(255,209,102,.2); border-top-color:#ffd166;
  animation:spin .9s linear infinite;
}
@keyframes spin { to { transform:rotate(360deg); } }
h2 { font-size:16px; margin-bottom:6px; }
p { font-size:12.5px; color:#6b7bab; line-height:1.6; margin-bottom:18px; }
.idbox {
  padding:14px; border-radius:10px; background:#0d1322; border:1px solid #1a2436; margin-bottom:18px;
}
.idbox .l { font-size:10.5px; letter-spacing:.5px; color:#6b7bab; margin-bottom:4px; }
.idbox .n { font-family:'JetBrains Mono',monospace; font-size:26px; font-weight:800; color:#ffd166; letter-spacing:3px; }
.ct-title { font-size:11.5px; color:#6b7bab; margin-bottom:10px; }
.ct-btn {
  display:flex; align-items:center; gap:10px; text-decoration:none; color:#fff;
  padding:12px 14px; border-radius:12px; margin-bottom:10px; font-size:13.5px; font-weight:600;
  cursor:pointer; border:none; width:100%%; font-family:'Inter',sans-serif;
}
.ct-btn.tg { background:#0088cc; }
.ct-btn.ig { background:linear-gradient(45deg,#f09433,#e6683c,#dc2743,#cc2366,#bc1888); }
.ct-btn i.brand { font-size:18px; flex-shrink:0; }
.ct-btn span.label { flex:1; text-align:left; }
.ct-btn i.arrow { opacity:.85; }
.lang-switch { display:flex; justify-content:center; gap:6px; margin-top:16px; }
.lang-btn {
  font-size:11px; font-weight:700; padding:5px 11px; border-radius:20px; border:1px solid #1a2436;
  background:#0d1322; color:#6b7bab; cursor:pointer; font-family:'Inter',sans-serif;
}
.lang-btn.active { background:#38d6ff; color:#060810; border-color:transparent; }
</style></head>
<body>
<div class="card">
  <div class="spin"></div>
  <h2 data-i18n="h2">Password correct ✅</h2>
  <p data-i18n="p">Admin hasn't approved yet. Send this Access ID to admin — the page will unlock automatically once approved.</p>
  <div class="idbox">
    <div class="l" data-i18n="idLabel">YOUR ACCESS ID</div>
    <div class="n">%05d</div>
  </div>
  <div class="ct-title" data-i18n="ctTitle">Message here and send your ID to get approved</div>
  <a class="ct-btn tg" href="https://t.me/%s" target="_blank" rel="noopener">
    <i class="fa-brands fa-telegram brand"></i>
    <span class="label" data-i18n="tgLabel">Message on Telegram</span>
    <i class="fas fa-arrow-right arrow"></i>
  </a>
  <button type="button" class="ct-btn ig" onclick="messageOnInstagram()">
    <i class="fa-brands fa-instagram brand"></i>
    <span class="label" data-i18n="igLabel">Message on Instagram</span>
    <i class="fas fa-arrow-right arrow"></i>
  </button>
  <div class="lang-switch">
    <button class="lang-btn active" data-lang="en">EN</button>
    <button class="lang-btn" data-lang="hi">हिं</button>
    <button class="lang-btn" data-lang="bn">বাং</button>
  </div>
</div>
<script>
var ACCESS_ID = "%05d";
var IG_USER = %q;
var I18N = {
  en: { h2:"Password correct ✅", p:"Admin hasn't approved yet. Send this Access ID to admin — the page will unlock automatically once approved.",
        idLabel:"YOUR ACCESS ID", ctTitle:"Message here and send your ID to get approved",
        tgLabel:"Message on Telegram", igLabel:"Message on Instagram" },
  hi: { h2:"Password sahi hai ✅", p:"Admin ne abhi tak approve nahi kiya. Yeh Access ID admin ko bhejo — approve hote hi yeh page apne aap khul jaayega.",
        idLabel:"AAPKA ACCESS ID", ctTitle:"Yahan message karke apna ID bhejo approval ke liye",
        tgLabel:"Telegram par message karo", igLabel:"Instagram par message karo" },
  bn: { h2:"পাসওয়ার্ড সঠিক ✅", p:"অ্যাডমিন এখনও অ্যাপ্রুভ করেননি। এই Access ID অ্যাডমিনকে পাঠান — অ্যাপ্রুভ হলেই পেজ নিজে থেকে খুলে যাবে।",
        idLabel:"আপনার অ্যাক্সেস আইডি", ctTitle:"এখানে মেসেজ করে আপনার আইডি পাঠান অ্যাপ্রুভালের জন্য",
        tgLabel:"টেলিগ্রামে মেসেজ করুন", igLabel:"ইনস্টাগ্রামে মেসেজ করুন" }
};
document.querySelectorAll('.lang-btn').forEach(function(btn){
  btn.addEventListener('click', function(){
    document.querySelectorAll('.lang-btn').forEach(function(b){ b.classList.remove('active'); });
    btn.classList.add('active');
    var dict = I18N[btn.dataset.lang];
    document.querySelectorAll('[data-i18n]').forEach(function(el){
      var key = el.getAttribute('data-i18n');
      if (dict[key]) el.textContent = dict[key];
    });
  });
});
function messageOnInstagram() {
  var msg = "My Access ID is " + ACCESS_ID + ", please approve it.";
  if (navigator.clipboard) { navigator.clipboard.writeText(msg).catch(function(){}); }
  window.open("https://instagram.com/" + IG_USER, "_blank");
}
</script>
</body></html>`, accessID, url.QueryEscape(tgUser), accessID, igUser)
}

// notifyNewAccessID pings the admin exactly once, the moment a brand-new
// device shows up on a password-protected link, with the 5-digit code to
// approve. Existing/repeat visitors don't spam this — isNew is only true
// the first time getOrCreateApproval ever sees that device.
//
// visitorName must be non-empty — bots/uptime checkers (which never fill in
// the name field) hit this repeatedly without a name, and used to spam the
// admin with a "new visitor" message every single ping since they never
// carry a cookie and look like a brand-new device each time. Requiring a
// name closes that off: no name typed in, no notification sent, period.
func (a *App) notifyNewAccessID(isNew bool, accessID int, fileName, visitorName string) {
	if !isNew || accessID == 0 || strings.TrimSpace(visitorName) == "" {
		return
	}
	a.pool.sendMD(a.cfg.AdminID, fmt.Sprintf(
		"🆕 New visitor on *%s*\n\nAccess ID: `%05d`\n\nApprove karne ke liye: `/approve %05d`",
		mdEscape(fileName), accessID, accessID,
	))
}

func (a *App) handleWatch(w http.ResponseWriter, r *http.Request, slug string) {
	ctx := r.Context()

	if slug == "" {
		http.Error(w, "Missing file ID", http.StatusBadRequest)
		return
	}

	// Resolve file metadata from cache or DB — same fallback pattern as handleStream
	var fileName, mimeType string
	var fileSize int64
	var expiresAt *time.Time
	var passwordHash *string

	cached := a.cache.getFile(ctx, slug)
	if cached != nil {
		fileName = cached.FileName
		fileSize = cached.FileSize
		mimeType = cached.MimeType
		expiresAt = cached.ExpiresAt
		passwordHash = cached.PasswordHash
	} else {
		rec, err := a.db.getFileByID(ctx, slug)
		if err != nil {
			http.Error(w, "File not found.", http.StatusNotFound)
			return
		}
		fileName = rec.FileName
		fileSize = rec.FileSize
		mimeType = rec.MimeType
		expiresAt = rec.ExpiresAt
		passwordHash = rec.PasswordHash
		a.cache.setFile(ctx, slug, &cachedFile{
			MessageID: rec.MessageID, ChannelID: rec.ChannelID,
			FileName: rec.FileName, FileSize: rec.FileSize, MimeType: rec.MimeType,
			ExpiresAt: expiresAt, PasswordHash: passwordHash,
		})
	}

	if isExpired(expiresAt) {
		http.Error(w, "⏳ This link has expired.", http.StatusGone)
		return
	}

	// Every visitor's device gets its own cookie-backed ID — needed here
	// (not just later for view counting) so we can tie a 5-digit Access ID
	// to this specific device for the approval gate below.
	deviceID := getOrSetDeviceID(w, r)

	// Blocked-device gate — checked before anything else (password or
	// approval), so a blocked visitor can't get back in even with the right
	// password or by re-triggering the approval flow.
	if blockedApproval, _, _ := a.db.getOrCreateApproval(ctx, deviceID, slug); blockedApproval != nil && blockedApproval.Blocked {
		http.Error(w, "🚫 Access blocked.", http.StatusForbidden)
		return
	}

	// Password gate — check cookie first (already unlocked earlier), then a
	// freshly submitted ?pw= from the prompt form below. Only relevant when
	// this file actually has a password set.
	if passwordHash != nil && *passwordHash != "" {
		visitorName := strings.TrimSpace(r.URL.Query().Get("name"))
		unlocked := hasValidPasswordCookie(r, slug, *passwordHash)
		if !unlocked {
			if pw := r.URL.Query().Get("pw"); pw != "" {
				if sha256Hex(pw) == *passwordHash {
					setPasswordCookie(w, slug, *passwordHash)
					unlocked = true
					// Make sure the approval row exists before saving the name —
					// this might be the very first request for this device.
					a.db.getOrCreateApproval(ctx, deviceID, slug) //nolint:errcheck
					if visitorName != "" {
						if err := a.db.setApprovalName(ctx, deviceID, visitorName); err != nil {
							a.logger.Warn("setApprovalName failed", zap.Error(err))
						}
					}
				} else {
					approval, isNewDevice, _ := a.db.getOrCreateApproval(ctx, deviceID, slug)
					accessID := 0
					if approval != nil {
						accessID = approval.AccessID
					}
					a.notifyNewAccessID(isNewDevice, accessID, fileName, visitorName)
					renderPasswordPrompt(w, slug, true, accessID, visitorName, a.cfg.PasswordPromptVideoURL, a.cfg.PasswordPromptImages, a.cfg.ContactTelegramUsername, a.cfg.ContactInstagramUsername)
					return
				}
			}
		}
		if !unlocked {
			approval, isNewDevice, _ := a.db.getOrCreateApproval(ctx, deviceID, slug)
			accessID := 0
			if approval != nil {
				accessID = approval.AccessID
			}
			a.notifyNewAccessID(isNewDevice, accessID, fileName, visitorName)
			renderPasswordPrompt(w, slug, false, accessID, visitorName, a.cfg.PasswordPromptVideoURL, a.cfg.PasswordPromptImages, a.cfg.ContactTelegramUsername, a.cfg.ContactInstagramUsername)
			return
		}
	}

	// Access-ID approval gate — applies to EVERY link, password-protected or
	// not. Same device always gets the same 5-digit code back (device_id is
	// UNIQUE + permanent), so this is a one-time approval per device: once
	// the admin runs /approve for that code, this device stays approved
	// forever, on any link. Until then, nobody gets through — password or no
	// password.
	approval, isNewDevice, err := a.db.getOrCreateApproval(ctx, deviceID, slug)
	if err != nil {
		a.logger.Warn("approval lookup failed", zap.Error(err))
	}
	if approval == nil || !approval.Approved {
		accessID := 0
		visitorName := ""
		if approval != nil {
			accessID = approval.AccessID
			visitorName = approval.VisitorName
		}
		a.notifyNewAccessID(isNewDevice, accessID, fileName, visitorName)
		renderPendingApproval(w, slug, accessID, a.cfg.ContactTelegramUsername, a.cfg.ContactInstagramUsername)
		return
	}

	if mimeType == "" {
		mimeType = "video/mp4"
	}

	// Unique-device view counting: one device (browser) only ever counts once
	// per file, permanently (rows are never deleted). Repeated visits from the
	// same device don't inflate the count; a new device always does.
	isNew, views, viewErr := a.db.recordUniqueView(ctx, slug, deviceID)
	if viewErr != nil {
		a.logger.Warn("recordUniqueView failed", zap.Error(viewErr))
	} else if isNew {
		for _, m := range viewMilestones {
			if views == m {
				a.pool.sendMD(a.cfg.AdminID, fmt.Sprintf(
					"👀 *%s*\n\nhit *%s* unique views on the watch page\\!",
					mdEscape(fileName), mdEscape(formatCount(views)),
				))
				break
			}
		}
	}

	base := a.cfg.baseURL()
	streamURL := fmt.Sprintf("%s/stream/%s", base, slug)
	dlURL := fmt.Sprintf("%s/dl/%s", base, slug)

	data := WatchData{
		FileName:    fileName,
		FileSize:    formatSize(fileSize),
		MimeType:    mimeType,
		StreamURL:   streamURL,
		DownloadURL: dlURL,
		ViewCount:   formatCount(views),
		LiveCount:   fmt.Sprintf("%d", a.cache.liveCount(ctx, slug)),
		DeviceID:    deviceID,
		Slug:        slug,
	}

	// index.html is baked into the container at /index.html (see Dockerfile)
	tmpl, err := template.ParseFiles("/index.html")
	if err != nil {
		a.logger.Error("watch template parse failed", zap.Error(err))
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.Execute(w, data); err != nil {
		a.logger.Error("watch template execute failed", zap.Error(err))
	}
}

// handleSearch lets a viewer type a topic name (e.g. "human behaviour") and
// find matching uploaded videos by filename, without needing a fresh link
// shared for every file. Returns JSON: [{id, name, size}, ...].
func (a *App) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	w.Header().Set("Content-Type", "application/json")
	if len(q) < 2 {
		fmt.Fprint(w, `{"results":[]}`)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	files, err := a.db.searchFiles(ctx, q, 10)
	if err != nil {
		a.logger.Error("search failed", zap.Error(err))
		http.Error(w, `{"error":"search failed"}`, http.StatusInternalServerError)
		return
	}
	type result struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Size   string `json:"size"`
		Locked bool   `json:"locked"`
	}
	out := make([]result, 0, len(files))
	for _, f := range files {
		out = append(out, result{
			ID:     f.ID,
			Name:   f.FileName,
			Size:   formatSize(f.FileSize),
			Locked: f.PasswordHash != nil && *f.PasswordHash != "",
		})
	}
	b, _ := json.Marshal(map[string]any{"results": out})
	w.Write(b)
}

// handleAdmin renders a simple, auto-refreshing admin dashboard: total
// files/users/bots/views, how many people are watching right now (site-wide),
// and a per-file breakdown with live viewer counts. Gated by a token so it
// isn't publicly browsable — get the link via the /dashboard bot command.
func (a *App) handleAdmin(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if r.URL.Query().Get("token") != a.cfg.DashboardToken {
		http.Error(w, "❌ Invalid or missing token.", http.StatusForbidden)
		return
	}

	files, _ := a.db.countFiles(ctx)
	users, _ := a.db.countUsers(ctx)
	totalViews, _ := a.db.sumViews(ctx)
	liveNow := a.cache.liveCountAll(ctx)
	top, _ := a.db.topFilesByViews(ctx, 25)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!DOCTYPE html><html><head><meta charset="utf-8">
<meta http-equiv="refresh" content="10">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>RAJ Admin Dashboard</title>
<style>
body{background:#0f1117;color:#e6e6e6;font-family:system-ui,sans-serif;margin:0;padding:24px;}
h1{font-size:20px;margin-bottom:4px;} .sub{color:#8b949e;font-size:12px;margin-bottom:24px;}
.cards{display:flex;gap:14px;flex-wrap:wrap;margin-bottom:28px;}
.card{background:#1b1c26;border-radius:12px;padding:16px 20px;min-width:120px;}
.card .n{font-size:26px;font-weight:700;color:#7aa2f7;} .card .l{font-size:12px;color:#8b949e;}
.live{color:#f87171;}
table{width:100%%;border-collapse:collapse;font-size:13px;}
th,td{padding:8px 10px;border-bottom:1px solid #262837;text-align:left;}
th{color:#8b949e;font-weight:600;} .locked{color:#f0b429;} .pw{color:#7ee787;font-family:monospace;}
</style></head><body>
<h1>🖥️ RAJ Admin Dashboard</h1>
<div class="sub">Auto-refreshes every 10s</div>
<div class="cards">
<div class="card"><div class="n">%d</div><div class="l">📁 Files</div></div>
<div class="card"><div class="n">%d</div><div class="l">👥 Users</div></div>
<div class="card"><div class="n">%d</div><div class="l">🤖 Bots</div></div>
<div class="card"><div class="n">%s</div><div class="l">👁 Total unique views</div></div>
<div class="card"><div class="n live">%d</div><div class="l">🔴 Watching right now</div></div>
</div>
<table><thead><tr><th>File</th><th>Size</th><th>Views</th><th>Live now</th><th>🔒 Password</th><th>Uploaded</th></tr></thead><tbody>`,
		files, users, a.pool.count(), formatCount(totalViews), liveNow)

	for _, f := range top {
		lock := `<span style="color:#4b5263;">—</span>`
		if f.PasswordHash != nil && *f.PasswordHash != "" {
			pw := "?"
			if f.PasswordPlain != nil && *f.PasswordPlain != "" {
				pw = *f.PasswordPlain
			}
			lock = fmt.Sprintf(`<span class="locked">🔒</span> <span class="pw">%s</span>`, template.HTMLEscapeString(pw))
		}
		fmt.Fprintf(w, `<tr><td>%s</td><td>%s</td><td>%s</td><td>%d</td><td>%s</td><td>%s</td></tr>`,
			template.HTMLEscapeString(f.FileName), formatSize(f.FileSize), formatCount(f.ViewCount),
			a.cache.liveCount(ctx, f.ID), lock, f.CreatedAt.Format("02 Jan 15:04"))
	}

	fmt.Fprint(w, `</tbody></table></body></html>`)
}

// ============================================================
// HELPERS
// ============================================================

func makeShortHash(fileName string, fileSize int64, messageID int) string {
	key := fmt.Sprintf("%s-%d-%d", fileName, fileSize, messageID)
	h := 0
	for _, c := range key {
		h = h*31 + int(c)
	}
	if h < 0 {
		h = -h
	}
	return fmt.Sprintf("%06x", h%0xffffff)[:6]
}

// toInternalChannelID converts -1001234567890 → 1234567890
func toInternalChannelID(id int64) int64 {
	if id < -1000000000000 {
		return -(id + 1000000000000)
	}
	if id < 0 {
		return -id
	}
	return id
}

func mdEscape(s string) string {
	r := strings.NewReplacer(
		"_", "\\_", "*", "\\*", "[", "\\[", "]", "\\]",
		"(", "\\(", ")", "\\)", "~", "\\~", "`", "\\`",
		">", "\\>", "#", "\\#", "+", "\\+", "-", "\\-",
		"=", "\\=", "|", "\\|", "{", "\\{", "}", "\\}",
		".", "\\.", "!", "\\!",
	)
	return r.Replace(s)
}

// viewMilestones are the view counts at which the uploader gets a Telegram notification.
var viewMilestones = []int64{10, 50, 100, 250, 500, 1000, 2500, 5000, 10000, 25000, 50000, 100000, 250000, 500000, 1000000}

func formatSize(b int64) string {
	const u = 1024
	if b < u {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(u), 0
	for n := b / u; n >= u; n /= u {
		div *= u
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

// isExpired reports whether a link's expiry timestamp has passed.
// A nil timestamp means the link is permanent/unlimited (the default).
func isExpired(expiresAt *time.Time) bool {
	return expiresAt != nil && time.Now().After(*expiresAt)
}

// formatCount renders view counts the "1.2K" / "3.4M" way for compact display.
func formatCount(n int64) string {
	switch {
	case n < 1000:
		return fmt.Sprintf("%d", n)
	case n < 1000000:
		return fmt.Sprintf("%.1fK", float64(n)/1000)
	default:
		return fmt.Sprintf("%.1fM", float64(n)/1000000)
	}
}

// parseExpiryDuration turns admin-friendly strings like "30m", "12h", "7d", "1y"
// into a time.Duration. Supports "off"/"never"/"remove" to signal "clear expiry".
func parseExpiryDuration(s string) (dur time.Duration, clear bool, err error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "off" || s == "never" || s == "remove" || s == "none" {
		return 0, true, nil
	}
	if s == "" {
		return 0, false, fmt.Errorf("empty duration")
	}
	// Custom suffixes not supported by time.ParseDuration: d (days), y (years)
	if strings.HasSuffix(s, "d") {
		n, e := strconv.ParseFloat(strings.TrimSuffix(s, "d"), 64)
		if e != nil {
			return 0, false, fmt.Errorf("invalid days value")
		}
		return time.Duration(n * 24 * float64(time.Hour)), false, nil
	}
	if strings.HasSuffix(s, "y") {
		n, e := strconv.ParseFloat(strings.TrimSuffix(s, "y"), 64)
		if e != nil {
			return 0, false, fmt.Errorf("invalid years value")
		}
		return time.Duration(n * 365 * 24 * float64(time.Hour)), false, nil
	}
	d, e := time.ParseDuration(s)
	if e != nil {
		return 0, false, fmt.Errorf("invalid duration (use e.g. 30m, 12h, 7d, 1y, or 'off')")
	}
	return d, false, nil
}

// sha256Hex hashes a password (or generates a token) into a hex string.
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// randomToken generates a random hex token of n random bytes (n*2 hex chars).
func randomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// Extremely unlikely; fall back to a time-based token rather than crash.
		return sha256Hex(fmt.Sprintf("fallback-%d", time.Now().UnixNano()))
	}
	return hex.EncodeToString(b)
}

// deviceCookieName / passwordCookieName — cookies used for unique-device
// view counting and for remembering a correctly-entered link password.
const deviceCookieName = "rdid"

func passwordCookieName(slug string) string { return "fpw_" + slug }

// getOrSetDeviceID reads the long-lived device-id cookie, or creates one.
// One cookie = one "device" for the lifetime of the browser/profile — this is
// what makes the view counter count unique viewers instead of page reloads.
func getOrSetDeviceID(w http.ResponseWriter, r *http.Request) string {
	if ck, err := r.Cookie(deviceCookieName); err == nil && ck.Value != "" {
		return ck.Value
	}
	id := uuid.New().String()
	http.SetCookie(w, &http.Cookie{
		Name:     deviceCookieName,
		Value:    id,
		Path:     "/",
		MaxAge:   10 * 365 * 24 * 60 * 60, // ~10 years — permanent per browser
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	return id
}

// hasValidPasswordCookie checks whether the browser already proved it knows
// the current password for this link (set once, right after correct entry).
func hasValidPasswordCookie(r *http.Request, slug, hash string) bool {
	ck, err := r.Cookie(passwordCookieName(slug))
	return err == nil && ck.Value == hash
}

func setPasswordCookie(w http.ResponseWriter, slug, hash string) {
	http.SetCookie(w, &http.Cookie{
		Name:     passwordCookieName(slug),
		Value:    hash,
		Path:     "/",
		MaxAge:   60, // 1 minute — password expire hoke dobara lock ho jaata hai
		HttpOnly: false,
		SameSite: http.SameSiteLaxMode,
	})
}

func sanitizeName(name string) string {
	name = filepath.Base(name)
	name = strings.ReplaceAll(name, `"`, "")
	if name == "" || name == "." {
		return "file"
	}
	return name
}

// ============================================================
// MAIN
// ============================================================

func main() {
	encCfg := zap.NewProductionEncoderConfig()
	encCfg.EncodeTime = zapcore.ISO8601TimeEncoder
	encCfg.EncodeLevel = zapcore.CapitalColorLevelEncoder
	logger, _ := zap.Config{
		Level:            zap.NewAtomicLevelAt(zap.InfoLevel),
		Encoding:         "console",
		EncoderConfig:    encCfg,
		OutputPaths:      []string{"stdout"},
		ErrorOutputPaths: []string{"stderr"},
	}.Build()
	defer logger.Sync() //nolint:errcheck

	fmt.Print("\033[36m\n██████╗  █████╗      ██╗\n██╔══██╗██╔══██╗     ██║\n██████╔╝███████║     ██║\n██╔══██╗██╔══██║██   ██║\n██║  ██║██║  ██║╚█████╔╝\n╚═╝  ╚═╝╚═╝  ╚═╝ ╚════╝\n\033[0m  ⚡ RAJ File Stream Bot\n  Built by Raj Dev J\n\n")
	// RAJ Logo
	logger.Info("⚡ RAJ File Stream Bot starting...")

	ctx, cancel := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer cancel()

	cfg, err := loadConfig()
	if err != nil {
		logger.Fatal("config error", zap.Error(err))
	}
	logger.Info("config loaded",
		zap.Int("bots", len(cfg.BotTokens)),
		zap.String("fqdn", cfg.FQDN),
	)

	db, err := newDB(ctx, cfg.DBURI)
	if err != nil {
		logger.Fatal("database error", zap.Error(err))
	}
	defer db.close()
	logger.Info("✅ database connected")

	cache, err := newCache(ctx, cfg.RedisURI)
	if err != nil {
		logger.Fatal("redis error", zap.Error(err))
	}
	defer cache.close() //nolint:errcheck
	logger.Info("✅ redis connected")

	// BotPool mein SAARE tokens — main + workers
	// Forwarding ke liye round-robin use hoga (fast!)
	// Lekin polling sirf primary() = MainBotToken se hogi
	pool, err := newBotPool(cfg.BotTokens, logger)
	if err != nil {
		logger.Fatal("bot pool error", zap.Error(err))
	}

	pool.primary().Request(tgbotapi.NewSetMyCommands( //nolint:errcheck
		tgbotapi.BotCommand{Command: "start", Description: "Start"},
		tgbotapi.BotCommand{Command: "help", Description: "Help"},
		tgbotapi.BotCommand{Command: "stats", Description: "Stats (admin)"},
		tgbotapi.BotCommand{Command: "dashboard", Description: "Admin dashboard link (admin)"},
		tgbotapi.BotCommand{Command: "setpass", Description: "Password-protect a link"},
	))

	// Start MTProto pool — sirf MAIN BOT se streaming
	// Worker bots ka MTProto nahi — unhe channel ka access nahi hoga!
	logger.Info("starting MTProto connections...")
	mtPool := newMTProtoPool(cfg.APIID, cfg.APIHash, []string{cfg.MainBotToken}, logger)

	// Wait for at least one MTProto connection
	for i := 0; i < 15; i++ {
		if mtPool.isAnyReady() {
			logger.Info("✅ MTProto ready — any file size supported!")
			break
		}
		time.Sleep(1 * time.Second)
	}
	if !mtPool.isAnyReady() {
		logger.Warn("⚠️ MTProto still connecting — will retry in background")
	}

	app := &App{
		cfg:    cfg,
		db:     db,
		cache:  cache,
		pool:   pool,
		mtPool: mtPool,
		logger: logger,
	}

	// Let the admin know the dashboard link at startup (also always available via /dashboard)
	dashboardLink := fmt.Sprintf("%s/admin?token=%s", cfg.baseURL(), cfg.DashboardToken)
	app.pool.send(cfg.AdminID, fmt.Sprintf(
		"🚀 Bot started!\n\n🖥️ Dashboard: %s", dashboardLink,
	))

	// HTTP server
	mux := http.NewServeMux()
	mux.HandleFunc("/search", app.handleSearch)
	mux.HandleFunc("/", app.serveHTTP)
	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           mux,
		ReadHeaderTimeout: 15 * time.Second,
		WriteTimeout:      0, // Must be 0 for streaming
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		logger.Info("✅ HTTP server started", zap.String("port", cfg.Port))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("http error", zap.Error(err))
		}
	}()

	// Pending-visitor cleanup — every 5 minutes, delete approval rows that
	// are still pending (not approved, not blocked) and older than 30
	// minutes. Keeps the /user list and admin's DMs from filling up with
	// stale entries from bots/uptime checkers that never get approved.
	// Approved and blocked rows are never touched by this.
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		staleAfter := 30 * time.Minute
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				n, err := app.db.deletePendingApprovals(context.Background(), &staleAfter)
				if err != nil {
					logger.Warn("pending approval cleanup failed", zap.Error(err))
				} else if n > 0 {
					logger.Info("cleaned up stale pending visitors", zap.Int64("count", n))
				}
			}
		}
	}()

	// Telegram long-polling
	// SIRF main bot (BOT_TOKEN) polling karta hai
	// Worker bots (MULTI_TOKEN1,2...) sirf streaming ke liye hain!
	go func() {
		logger.Info("✅ bot polling started", zap.String("bot", "@"+pool.primary().Self.UserName))

		for {
			select {
			case <-ctx.Done():
				pool.stopUpdates()
				return
			default:
			}

			u := tgbotapi.NewUpdate(0)
			u.Timeout = 60

			// DeleteWebhook + DropPendingUpdates — conflict clear karta hai
			pool.primary().Request(tgbotapi.DeleteWebhookConfig{DropPendingUpdates: true}) //nolint:errcheck

			updates := pool.primary().GetUpdatesChan(u)

			pollLoop:
			for {
				select {
				case <-ctx.Done():
					pool.stopUpdates()
					return
				case update, ok := <-updates:
					if !ok {
						break pollLoop
					}
					go func(upd tgbotapi.Update) {
						defer func() {
							if r := recover(); r != nil {
								logger.Error("panic", zap.Any("r", r))
							}
						}()
						app.dispatch(ctx, upd)
					}(update)
				}
			}

			// Agar yahan aaye toh channel band hua — thoda ruk ke retry karo
			logger.Warn("polling stopped, retrying in 5s...")
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down...")
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutCancel()
	srv.Shutdown(shutCtx) //nolint:errcheck
	logger.Info("bye! ✓")
}
