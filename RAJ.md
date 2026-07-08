# ⚡ RAJ File Stream Bot
### Built by **Raj Dev J**

---

## 🤖 Bot Kya Hai?

Yeh ek **Telegram File Streaming Bot** hai jo Go language mein likha gaya hai.

Aap bot ko koi bhi file bhejo — video, audio, document — bot ek **permanent link** deta hai.
Woh link browser mein seedha play hota hai ya download ho sakta hai.

**Koi bhi size ka file kaam karta hai — 1MB se lekar 4GB tak!**

---

## 📁 Files — Kaun Kya Karta Hai

```
Raj/
├── main.go       ← Poora bot ka code (sab kuch ek hi file mein!)
├── go.mod        ← Dependencies ki list
├── Dockerfile    ← Render/Docker deploy ke liye
└── RAJ.md        ← Yeh documentation file
```

### `main.go` — Poora Bot

Yeh ek hi file hai jisme **sab kuch** hai:

| Section | Kya Karta Hai |
|---------|---------------|
| **Config** | Environment variables padhta hai |
| **Database** | Neon PostgreSQL se baat karta hai |
| **Cache** | Upstash Redis se baat karta hai |
| **BotPool** | Telegram Bot API manage karta hai |
| **MTProtoPool** | Bade files stream karta hai |
| **TgFileReader** | Telegram se bytes read karta hai |
| **HTTP Server** | Browser ke requests handle karta hai |
| **Handlers** | Bot ke commands handle karta hai |
| **main()** | Sab kuch start karta hai |

### `go.mod` — Dependencies

```
github.com/go-telegram-bot-api  ← Telegram Bot API
github.com/gotd/td              ← MTProto (bade files ke liye)
github.com/jackc/pgx/v5         ← Neon PostgreSQL
github.com/redis/go-redis/v9    ← Upstash Redis
github.com/google/uuid          ← Unique link ID banane ke liye
go.uber.org/zap                 ← Logging
```

### `Dockerfile` — Deploy ke liye

```
Stage 1: Go code build karta hai
Stage 2: Chota sa final image banata hai
Result: ~10MB ka binary — bahut fast start hota hai!
```

---

## 🔧 Environment Variables — Poori List

### ✅ REQUIRED (Zaroori — bina inke bot nahi chalega)

| Variable | Kya Hai | Example |
|----------|---------|---------|
| `BOT_TOKENS` | Bot tokens comma se alag | `123:ABC,456:DEF` |
| `MULTI_TOKEN1` | Ya alag alag tokens | `123456:ABCdef` |
| `MULTI_TOKEN2` | Doosra token | `789012:GHIjkl` |
| `MULTI_TOKEN3` | Teesra token | `345678:MNOpqr` |
| `API_ID` | Telegram API ID | `12345678` |
| `API_HASH` | Telegram API Hash | `abcdef1234567890` |
| `ADMIN_ID` | Aapka Telegram User ID | `987654321` |
| `DB_URI` | Neon PostgreSQL URL | `postgresql://user:pass@host/db?sslmode=require` |
| `REDIS_URI` | Upstash Redis URL | `rediss://default:pass@host:6380` |
| `FQDN` | Aapki website ka URL | `raj.onrender.com` |
| `DB_CHANNEL_ID` | Storage channel ID | `-1001234567890` |
| `LOG_CHANNEL_ID` | Log channel ID | `-1009876543210` |
| `PASSWORD_PROMPT_VIDEO_URL` | your thumbnail video | `https://your video mp4` |


### ⚙️ OPTIONAL (Default values hain)

| Variable | Default | Kya Karta Hai |
|----------|---------|---------------|
| `MAIN_CHANNEL_ID` | Off | Force Subscribe channel |
| `PORT` | `8080` | HTTP server port |

### 📝 Token Format — Dono Tarike Chalte Hain

```bash
# Tarika 1 — Sab ek saath
BOT_TOKENS=token1,token2,token3

# Tarika 2 — Alag alag (Raj Dev J ka pasandida!)
MULTI_TOKEN1=123456:token1
MULTI_TOKEN2=789012:token2
MULTI_TOKEN3=345678:token3
```

---

## ⚙️ Kaise Kaam Karta Hai — Poora Logic

### File Upload Flow

```
Aap file bhejte ho bot ko
        ↓
Bot file forward karta hai Storage Channel mein
        ↓
Message ID + Channel ID save hota hai Neon DB mein
        ↓
UUID slug generate hota hai (jaise: a1b2c3d4-...)
        ↓
Redis mein cache hota hai (fast access ke liye)
        ↓
Aapko 2 links milte hain:
   ▶️  https://yoursite.com/stream/{uuid}
   ⬇️  https://yoursite.com/dl/{uuid}
```

### File Stream Flow (Jab koi link khole)

```
Browser GET /stream/{uuid}
        ↓
Redis check karo (cache mein hai?)
   Haan → Fast! Seedha aage jao
   Nahi → Neon DB se lo, Redis mein save karo
        ↓
MTProto se Telegram ka message lo
(message_id + channel_id use hota hai)
        ↓
InputDocumentFileLocation milta hai
        ↓
TgFileReader — 1MB chunks mein padhta hai
        ↓
io.CopyBuffer — seedha browser ko deta hai
        ↓
Browser mein video play hoti hai! ✅
```

### Range Request (Seek/Skip) Flow

```
User video mein 50% pe click karta hai
        ↓
Browser bhejta hai: Range: bytes=524288000-
        ↓
Bot calculate karta hai: start=524288000
        ↓
TgFileReader usi jagah se start karta hai
        ↓
User seedha 50% se dekh sakta hai ✅
```

---

## 🗄️ Database (Neon PostgreSQL) — Kya Save Hota Hai

```sql
-- Files table
id            → UUID slug (link mein use hota hai)
message_id    → Telegram message ID in storage channel
channel_id    → Storage channel ID
file_name     → File ka naam
file_size     → File ki size (bytes mein)
mime_type     → File type (video/mp4, audio/mpeg, etc.)
hash          → Short verification hash
uploader_id   → Kisne upload kiya
uploader_name → Username
created_at    → Kab upload hua

-- Users table
id            → Telegram User ID
username      → @username
first_name    → Pehla naam
is_banned     → Banned hai ya nahi
joined_at     → Kab aaya
```

**Important: File ka actual data DB mein KABHI save nahi hota!**
Sirf metadata (naam, size, ID) save hota hai.

---

## ⚡ Cache (Upstash Redis) — Kya Cache Hota Hai

| Key | Value | TTL |
|-----|-------|-----|
| `file:{uuid}` | File metadata | 1 ghanta |
| `fsub:{userID}` | Member hai ya nahi | 5 minute |

**Fayda:** DB pe baar baar request nahi jaati → Bot fast rehta hai!

---

## 🤖 Bot Commands

| Command | Kaun Use Kar Sakta Hai | Kya Karta Hai |
|---------|----------------------|---------------|
| `/start` | Sab | Welcome message |
| `/help` | Sab | Help message |
| `/stats` | Sirf Admin | Files, Users, Bots count |
| `/ban <id>` | Sirf Admin | User ko ban karo |
| `/unban <id>` | Sirf Admin | User ka ban hatao |
| `/delete <id>` | Sirf Admin | File record delete karo |

---

## 🔗 URL Formats

```
Stream (inline play):   https://yoursite.com/stream/{uuid}
Download (force save):  https://yoursite.com/dl/{uuid}
Health check:           https://yoursite.com/health
```

---

## 📊 Multi-Bot Load Balancing

```
Ek token  → 10-15 users tak smooth
3 tokens  → 50-100 users tak smooth
5 tokens  → 200+ users tak smooth
10 tokens → 500+ users tak smooth
```

**Kaise kaam karta hai:**
```
User 1 request → Bot 1 handle karta hai
User 2 request → Bot 2 handle karta hai
User 3 request → Bot 3 handle karta hai
User 4 request → Bot 1 (round robin!)
...
```

---

## 🚀 Render Pe Deploy Karne Ke Steps

```
1. GitHub pe "Raj" repo banao
2. main.go, go.mod, Dockerfile upload karo
3. render.com → New Web Service → GitHub connect
4. Environment Variables section mein sab daalo
5. Deploy!
6. go.sum Render khud banayega ✅
```

---

## ❓ Common Issues

| Problem | Solution |
|---------|----------|
| `Database error` | DB_URI mein `?sslmode=require` check karo |
| `Cannot retrieve file` | API_ID aur API_HASH check karo |
| `MTProto not ready` | Bot restart karo |
| `File not found` | Storage channel mein bot admin hai? |
| `FSub not working` | Bot MAIN_CHANNEL mein admin hai? |
| `Flood wait` | Aur tokens add karo |

---

## 🏗️ Technical Architecture

```
┌─────────────────────────────────────────────┐
│              User / Browser                  │
└─────────────────────────────────────────────┘
              │ GET /stream/{id}
              ▼
┌─────────────────────────────────────────────┐
│         Go HTTP Server (Render)              │
│  ┌──────────────┐  ┌─────────────────────┐  │
│  │  serveHTTP   │  │   handleStream      │  │
│  │  (routing)   │  │   (range requests)  │  │
│  └──────────────┘  └─────────────────────┘  │
└──────────┬──────────────────┬───────────────┘
           │                  │
     ┌─────▼──────┐    ┌──────▼───────┐
     │   Redis    │    │   Neon DB    │
     │  (cache)   │    │ (file info)  │
     └────────────┘    └──────────────┘
                              │
                    ┌─────────▼──────────┐
                    │   MTProto Pool     │
                    │  (Bot 1, 2, 3...)  │
                    └─────────┬──────────┘
                              │
                    ┌─────────▼──────────┐
                    │   Telegram CDN     │
                    │  (actual file)     │
                    └────────────────────┘
```

---

## 📦 File Size Limits

| Method | Limit |
|--------|-------|
| Bot API (purana) | 20MB ❌ |
| MTProto (abhi wala) | **Koi limit nahi** ✅ |

---

*Built with ❤️ by **Raj Dev J***
*Powered by Go, Telegram MTProto, Neon PostgreSQL & Upstash Redis*

---

## 🆕 New Features Added (Password, Live Viewers, Admin Dashboard)

### 🔒 Password-Protected Links
- `/setpass <file_id> <password>` — sirf uploader ya admin set kar sakta hai
- `/setpass <file_id> off` — password hata do
- Password ek `/watch` prompt page pe maangi jaati hai; sahi password dalte hi
  30-din wali cookie set ho jaati hai, dobara nahi maangega usi browser mein
- `/stream` aur `/dl` bhi isi cookie se protect hote hain (video tag automatically
  cookie bhejta hai — password URL mein kabhi expose nahi hoti)
- Password kabhi plaintext mein DB mein save nahi hoti — sirf SHA-256 hash

### 👁 Unique-Device View Counter (Permanent)
- Har browser/device ko ek permanent cookie (`rdid`) milta hai
- Ek naya `file_views` table (file_id + device_id) rakhta hai kis device ne
  kaunsi file dekhi — same device dobara dekhe toh count nahi badhta
- Naya device dekhe toh hi permanent `view_count` +1 hota hai — kabhi delete/reset nahi hota

### 🔴 Live "Watching Now" Counter
- Watch page har 15 second mein `/heartbeat/{id}` ko call karta hai
- Redis mein har file ke liye ek sorted-set rakha jaata hai (30-second window)
- `/livecount/{id}` real-time count deta hai — watch page har 10s mein isse refresh karta hai
- Dashboard pe site-wide "live right now" bhi dikhta hai

### 🖥️ Admin Dashboard (`/admin?token=...`)
- Token-protected web page — link `/dashboard` command se milta hai (sirf admin)
- Total files, users, bots, total unique views, live viewers dikhata hai
- Per-file table: size, views, live-now count, 🔒 (password hai ya nahi), upload date
- Har 10 second mein auto-refresh hota hai
