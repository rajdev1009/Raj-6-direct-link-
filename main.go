package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
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

	// Streaming config (from env vars)
	StreamConcurrency int // STREAM_CONCURRENCY (default: 4)
	StreamBufferCount int // STREAM_BUFFER_COUNT (default: 8)
	StreamTimeoutSec  int // STREAM_TIMEOUT_SEC  (default: 30)
	StreamMaxRetries  int // STREAM_MAX_RETRIES  (default: 3)
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

	// Stream config with defaults
	cfg.StreamConcurrency = envInt("STREAM_CONCURRENCY", 4)
	cfg.StreamBufferCount = envInt("STREAM_BUFFER_COUNT", 8)
	cfg.StreamTimeoutSec  = envInt("STREAM_TIMEOUT_SEC", 30)
	cfg.StreamMaxRetries  = envInt("STREAM_MAX_RETRIES", 3)

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
}

type UserRecord struct {
	ID        int64
	Username  string
	FirstName string
	IsBanned  bool
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
		// Remove tg_file_id column if it exists (old schema cleanup)
		`DO $$ BEGIN
			IF EXISTS (SELECT 1 FROM information_schema.columns
				WHERE table_name='files' AND column_name='tg_file_id') THEN
				ALTER TABLE files DROP COLUMN tg_file_id;
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
		SELECT id,message_id,channel_id,file_name,file_size,mime_type,hash,uploader_id,uploader_name,created_at
		FROM files WHERE id=$1`, id).Scan(
		&f.ID, &f.MessageID, &f.ChannelID, &f.FileName,
		&f.FileSize, &f.MimeType, &f.Hash,
		&f.UploaderID, &f.UploaderName, &f.CreatedAt,
	)
	return f, err
}

func (db *DB) deleteFileByMsgID(ctx context.Context, msgID int) (bool, error) {
	tag, err := db.pool.Exec(ctx, `DELETE FROM files WHERE message_id=$1`, msgID)
	return tag.RowsAffected() > 0, err
}

func (db *DB) countFiles(ctx context.Context) (int64, error) {
	var n int64
	return n, db.pool.QueryRow(ctx, `SELECT COUNT(*) FROM files`).Scan(&n)
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

// ============================================================
// CACHE
// ============================================================

type Cache struct{ client *redis.Client }

type cachedFile struct {
	MessageID int    `json:"message_id"`
	ChannelID int64  `json:"channel_id"`
	FileName  string `json:"file_name"`
	FileSize  int64  `json:"file_size"`
	MimeType  string `json:"mime_type"`
	Hash      string `json:"hash"`
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
			"*Commands:*\n/start \\- Welcome\n/help \\- Help\n/stats \\- Stats \\(admin\\)\n\nSend any file to get a link\\!")
	case "stats":
		if msg.From.ID != a.cfg.AdminID {
			a.pool.send(msg.Chat.ID, "❌ Admin only.")
			return
		}
		files, _ := a.db.countFiles(ctx)
		users, _ := a.db.countUsers(ctx)
		a.pool.sendMD(msg.Chat.ID, fmt.Sprintf(
			"📊 *Stats*\n\n📁 Files: `%d`\n👥 Users: `%d`\n🤖 Bots: `%d`",
			files, users, a.pool.count(),
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

	text := fmt.Sprintf(
		"✅ *File Stored\\!*\n\n📄 `%s`\n📦 `%s`\n\n▶️ [Stream](%s)\n⬇️ [Download](%s)\n\n🔒 Permanent link\\!",
		mdEscape(fi.fileName), formatSize(fi.fileSize), streamLink, dlLink,
	)
	kb := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonURL("▶️ Stream", streamLink),
			tgbotapi.NewInlineKeyboardButtonURL("⬇️ Download", dlLink),
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

	default:
		http.NotFound(w, r)
	}
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

	cached := a.cache.getFile(ctx, slug)
	if cached != nil {
		messageID = cached.MessageID
		channelID = cached.ChannelID
		fileName = cached.FileName
		fileSize = cached.FileSize
		mimeType = cached.MimeType
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
		a.cache.setFile(ctx, slug, &cachedFile{
			MessageID: messageID, ChannelID: channelID,
			FileName: fileName, FileSize: fileSize, MimeType: mimeType,
		})
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

	// HTTP server
	mux := http.NewServeMux()
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
