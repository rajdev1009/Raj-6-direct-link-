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





# ⚡ RAJ File Stream Bot — Poori Feature List
### Built by **Raj Dev J**

Yeh document batata hai ki abhi ke `main.go` + `index.html` mein **kya-kya features hain** aur **konse commands** kaam karte hain — normal aur admin-only (hidden) dono.

---

## 🌐 Website Routes (URLs)

| Route | Kya Karta Hai |
|---|---|
| `/` | Simple homepage — bot ka naam dikhata hai |
| `/health` ya `/ping` | Health check (JSON status) |
| `/stream/{id}` | Video/file ka **raw stream link** — seedha player ya app mein khulta hai, koi HTML wrapper nahi |
| `/dl/{id}` | Force **Download** link |
| `/watch/{id}` | **Watch Page** — poora advanced player wala HTML page |

Teeno links (`stream`, `dl`, `watch`) **expiry check** karte hain — agar file expire ho chuki hai toh `⏳ This link has expired.` dikhata hai.

---

## 🎬 Watch Page (`/watch/{id}`) — Poori Feature List

### Player
- **Native HTML5 video player** (fullscreen mein koi black gap nahi — old reliable player)
- **⏩ Skip 10s** button — ek tap mein 10 second aage
- **Resume Playback** — localStorage mein last position save hoti hai, dobara khologe toh wahi se shuru
- **💬 Subtitle Upload/Toggle** — apni `.srt`/`.vtt` file upload karke CC on/off kar sakte ho (client-side, koi server upload nahi hota)
- **📸 Screenshot/Capture** — current frame ko PNG image ke roop mein download
- **Double-Tap Seek (Mobile)** — video ke left/right side pe double-tap = -10s/+10s (YouTube jaisa), single tap = play/pause
- **Speed control** — 1x → 1.25x → 1.5x → 2x → 0.5x cycle button
- **Picture-in-Picture (PiP)** button
- **Loading spinner** — buffering ke time dikhta hai
- **Multi-Audio-Track Switcher** — agar file mein multiple dubbed audio tracks hain (Hindi/English/Tamil), toh dropdown se switch kar sakte ho (best-effort — sirf kuch desktop browsers support karte hain, mobile pe automatically chhup jaata hai)

### Info & Actions
- File name, file size, auto-detected quality badge (jaise 1080p — filename se guess hota hai)
- **👁 View Counter** — DB mein permanent count, page pe "1.2K views" jaisa dikhta hai
- **Bookmark/Watchlist** — localStorage-based, koi backend nahi chahiye
- **Action buttons**: Watchlist, Copy Link, PlayIt (app), VLC (app), Download, Share
- **Social Share Grid** — WhatsApp, Telegram, Facebook, Twitter, Instagram, Messenger, Email (sab auto-filled links ke saath)

### Branding & Extras
- Header mein aapka **logo image**, "RAJ Stream" naam, animated 🔴 LIVE badge, scrolling filename marquee
- **3-dot (⋮) Projects Menu** — aapke 2 projects (RAJ AI + Explore & Follow/Instagram) ke links
- Sidebar mein **RGB glowing "Raj Dev"** naam credit card
- Footer mein Telegram username **@raj_dev_01** (clickable)
- **VPN + Secure Note** — video ke neeche VPN suggestion (Proton/Turbo VPN, Singapore server) + "HTTPS secure" note, **🌐 Translate button** se English → Hindi → Bengali cycle
- **Theme Switcher** — 🌈 Default / 🌙 True Dark / ☀️ Light
- Toast notifications, keyboard shortcuts (`Ctrl+C` copy link, `Ctrl+B` bookmark, `Ctrl+W` open watchlist)

---

## 🤖 Telegram Bot Commands

### 👤 Normal (Sabke liye — but poora bot hi private/admin-only hai)
| Command | Kya Karta Hai |
|---|---|
| `/start` | Welcome message |
| `/help` | Sab commands ki list + expiry ka short explanation |
| *(File bhejna)* | Koi bhi file (video/audio/document/photo) bhejo → bot permanent stream/download/watch links deta hai, saath mein Telegram message mein "📺 Watch Online" button bhi milta hai |

### 🔒 Hidden / Admin-Only Commands
*(Sirf `ADMIN_ID` wala user use kar sakta hai — koi aur try kare toh silently ignore ya "Admin only" milta hai)*

| Command | Usage | Kya Karta Hai |
|---|---|---|
| `/stats` | `/stats` | Total files, total users, aur active bots ki count dikhata hai |
| `/ban` | `/ban <user_id>` | User ko bot use karne se ban karta hai |
| `/unban` | `/unban <user_id>` | Ban hataata hai |
| `/delete` | `/delete <file_id>` | File record delete karta hai (link kaam karna band ho jaata hai) |
| `/expire` | `/expire <file_id> <duration>` | **Link Expiry System** — neeche detail mein |

---

## ⏳ Link Expiry System (`/expire`)

**Default behavior: sab links PERMANENT (unlimited) hote hain** — jab tak aap khud expiry set na karo.

```
/expire abc123 30m     → 30 minute mein expire
/expire abc123 12h     → 12 ghante mein expire
/expire abc123 7d      → 7 din mein expire
/expire abc123 1y      → 1 saal mein expire (koi bhi custom duration chal sakti hai)
/expire abc123 off     → expiry hata do, link wapas permanent
```

- Expiry set karte hi cache turant clear hota hai (turant effect)
- Expire ho jaane ke baad `/stream`, `/dl`, `/watch` teeno **"⏳ This link has expired."** dikhate hain
- Har `/expire` set karne pe aapko confirmation message milta hai (exact expiry date/time ke saath)

---

## 👁 View Counter + Telegram Notifications

- Har baar koi `/watch` page kholta hai, uska count **Neon PostgreSQL DB mein permanently** save hota hai (bot restart ho, count safe rehta hai)
- Watch page pe dikh raha hai (stats row + sidebar dono jagah)
- **Milestone Notification**: jaise hi koi file 10, 50, 100, 500, 1K, 5K, 10K, 50K, 100K... views cross karti hai, aapko (Admin) seedha Telegram message aata hai:
  > 👀 *filename.mp4* hit **1.2K** views on the watch page!
- Har view pe spam nahi hota — sirf milestones pe

---

## 🗄️ Database Schema (Neon PostgreSQL)

**`files` table** columns:
`id`, `message_id`, `channel_id`, `file_name`, `file_size`, `mime_type`, `hash`, `uploader_id`, `uploader_name`, `created_at`, **`expires_at`** *(nullable — NULL = permanent)*, **`view_count`** *(default 0)*

**`users` table**: `id`, `username`, `first_name`, `is_banned`, `joined_at`

---

## 📁 Files — Kaun Kya Karta Hai

```
Raj/
├── main.go       ← Poora bot ka code + HTTP server + DB + expiry + views
├── index.html    ← Advanced Watch Page (Techify-style UI, sab features)
├── go.mod        ← Dependencies
├── Dockerfile    ← Deploy config (index.html bhi binary ke saath bundle hoti hai)
```

---

## 🔧 Environment Variables (Poori List, unchanged)

### ✅ Required
`BOT_TOKENS` / `MULTI_TOKEN1-3`, `API_ID`, `API_HASH`, `ADMIN_ID`, `DB_URI`, `REDIS_URI`, `FQDN`, `DB_CHANNEL_ID`, `LOG_CHANNEL_ID`

### ⚙️ Optional
`MAIN_CHANNEL_ID` (Force Subscribe), `PORT` (default `8080`)

---

*Yeh document is baat ko cover karta hai ki poore conversation mein jo bhi features step-by-step add hue (Watch Page, RGB branding, Projects menu, Audio tracks, VPN note, Player upgrades, Expiry system, View counter) — sab abhi ke code mein zinda hain, kuch bhi miss nahi hua.*
