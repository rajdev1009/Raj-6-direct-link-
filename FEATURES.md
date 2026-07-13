# ASTRATOONIX — Complete Feature List

Yeh poore platform ka ek jagah pe complete reference hai — jo bhi pehle se tha aur jo abhi is session mein add hua, sab kuchh.

---

## 1. File Streaming (Core)

- Koi bhi file size Telegram bot ke through upload karke stream/download link milta hai
- Range-request based video streaming (seek/scrub karne pe pura file load nahi hota)
- Har file ka apna unique short ID/slug, permanent link by default
- `/expire <file_id> <duration>` — link ko expire karne ka option (e.g. `7d`, `12h`, `1y`, ya `off` karke hata do)
- `/delete <file_id>` — ek file delete
- `/dminem` — **saari** files delete (confirmation ke saath, accidental delete se bachne ke liye)
- View counter — per-file unique-device views count hoti hai, kabhi reset nahi hoti
- Live viewer count — abhi kitne log woh specific video dekh rahe hain (heartbeat system se)

---

## 2. Password Protection

- `/setpass <file_id> <password>` — kisi bhi link ko password se protect karo
- Site-wide sliding-window cookie — ek baar password dalne ke baad, poori site pe (sab password-protected files pe) dobara nahi maangega jab tak cookie valid hai
- **[Is session mein fix hua]** Password form ab `POST` se submit hota hai, `GET` se nahi — pehle password address bar mein `?pw=...` ke roop mein dikh jaata tha, jisse copy-paste karke share kiya gaya link auto-unlock ho jaata tha. Ab yeh possible nahi.
- Admin dashboard pe har password-protected file ka plaintext password bhi dikhta hai (taaki bhoolne pe dobara dekh sako)

---

## 3. Visitor Approval System (Access ID)

- Har naya device jo kisi link pe aata hai, uska ek unique 5-digit **Access ID** banta hai
- Jab tak admin approve na kare, video stream nahi hota — pehle password page, uske baad "pending approval" page
- `/approve <access_id>` — admin manually approve kare
- `/block <access_id>` / `/unblock <access_id>` — **[naya]** kisi bhi device ka access permanently block/unblock karo, chahe woh pehle approved ho ya na ho
- `/user` — recent visitors ki list, Access ID + Name + Status (⏳pending / ✅approved / 🚫blocked) ke saath
- `/clearpending` — **[naya]** ek command se saare pending (non-approved) visitors clear — approved aur blocked wale safe rehte hain
- **[naya]** 30-minute auto-cleanup — background job har 5 min mein chalta hai, 30 min se purane pending visitors (jo kabhi approve nahi hue, jaise UptimeRobot ke fake pings) khud-ba-khud delete kar deta hai
- **[naya]** Naam zaroori — jab tak visitor apna naam na bhare, admin ko koi "New Visitor" notification nahi jaata. Isse bots/UptimeRobot pings se spam band ho gaya
- **[naya]** Telegram pe "New Visitor" notification ab sirf text nahi — seedha ✅ Approve aur 🚫 Block button attached hote hain, tap karte hi ho jaata hai. Tap karne pe wahi message update ho jaata hai (Approve ke baad Block button reh jaata hai, taaki baad mein revoke kar sako)
- **[naya]** Pending-approval page pe ab ek "📨 Notify Admin" button hai — visitor apna naam bharke button dabaye to seedha tumhare Telegram pe automatic message chala jaata hai (2-minute cooldown ke saath, spam-proof, chahe koi JS bypass kar ke bhi try kare)
- **[naya]** Pending page ab background mein chupchaap check karta hai ki approve hua ya nahi (har 6 sec), aur sirf approve/block hone par hi reload karta hai — pehle poora page hi reload ho jaata tha har 6 sec mein

---

## 4. Subject/Chapter Organization — **naya**

- `/tag <file_id> <Subject>` ya `/tag <file_id> <Subject> / <Chapter>` — file ko subject/chapter se tag karo
  - Example: `/tag abc123 Physics / Chapter 3 - Motion`
- `/untag <file_id>` — tag hataana
- Jaise-jaise tag karte jao, watch page ke search bar ke bagal wali "Subjects" row apne aap us subject ka icon dikhane lagti hai — kisi bhi coding ke bina
- Click karne pe us subject ke saare tagged chapters dikhte hain (chapter naam ke hisaab se grouped)
- Untagged files affect nahi hoti — normal search se abhi bhi mil jaati hain
- Yeh row utni hi jagah leti hai jitni pehle 5 fixed icons (Science/Math/English/Coding/History) le rahe the — koi extra jagah nahi

---

## 5. Visitor Profiles — **naya**

- ⚙️ icon (search ke bagal mein) — har visitor apna profile bhar sakta hai: **Name, About, Email, Phone, Instagram, Facebook**
- Ek hi device ID se judi rehti hai (dobara aane pe wahi profile dikhta hai)
- Sirf admin ko dikhta hai, kisi doosre visitor ko nahi
- `/profile <access_id>` — Telegram se seedha kisi bhi visitor ka poora profile dekho
- 🆔 icon (search+settings ke bagal mein) — **admin-only**, sirf tabhi dikhta hai jab link mein `?admin=TUMHARA_DASHBOARD_TOKEN` laga ho. Isme koi bhi Access ID daal ke us visitor ka poora profile dekh sakte ho
  - ⚠️ Security note: yeh backend pe bhi token-checked hai, sirf icon chhupana hi security nahi hai — koi bhi random visitor doosre ka phone/email nahi dekh sakta, chahe ID guess bhi kar le

---

## 6. Admin Dashboard (Web) — `/dashboard` command se link milta hai

- Total files, users, bots, total views, abhi kitne log live watch kar rahe hain
- **[naya]** Recent Visitors list — Access ID, Name, Status, aur seedhe **Approve / Block / Unblock** buttons (bina Telegram khole)
- **[naya]** Har file ke aage 🗑️ **Delete** button
- **[naya]** Har file ka Subject/Chapter tag bhi column mein dikhta hai (untagged files bhi saaf dikhti hain)
- Auto-refreshes har 10 second mein

---

## 7. Telegram Bot — Poori Command List

| Command | Kaam |
|---|---|
| `/start`, `/help` | Welcome, help |
| `/stats` | Overall stats (admin) |
| `/dashboard` | Admin dashboard link (admin) |
| `/setpass <id> <password>` | Link password-protect karo |
| `/tag <id> <subject> [/ chapter]` | Subject/Chapter tag karo (admin) |
| `/untag <id>` | Tag hataao (admin) |
| `/approve <access_id>` | Visitor approve (admin) |
| `/block <access_id>` | Visitor block (admin) |
| `/unblock <access_id>` | Visitor unblock (admin) |
| `/user` | Recent visitors list (admin) |
| `/profile <access_id>` | Visitor ka poora profile dekho (admin) |
| `/clearpending` | Saare pending visitors clear (admin) |
| `/ban` / `/unban <user_id>` | Telegram user ban/unban (admin) |
| `/expire <id> <duration>` | Link expiry set/remove (admin) |
| `/delete <id>` | Ek file delete (admin) |
| `/dminem` | Saari files delete, confirmation ke saath (admin) |

---

## 8. Watch Page (Video Player) — index.html

**Player controls:** speed control, Picture-in-Picture, skip ±10s, resume from last position (localStorage), double-tap seek zones (mobile), subtitle upload (.srt → .vtt), screenshot/frame capture, multi-audio-track switcher

**Search & Browse:**
- Live search bar — dusri file pe switch karo bina page reload kiye
- **[naya]** Subjects row — tagged subjects ke through browse karo
- **[fix hua]** File switch karne par ab title, page-tab-title, quality badge, aur watchlist — sab naye file ka naam sahi se follow karte hain (pehle sirf top ki marquee update hoti thi, neeche ka title purana hi reh jaata tha)

**Share/Copy:**
- **[fix hua]** Copy Link, WhatsApp, Telegram, Facebook, Twitter, Email, native Share — sab ab **currently playing** video ka sahi link use karte hain (pehle hamesha original/purana link hi share ho raha tha, chahe tumne koi bhi naya video select kiya ho)

**Watchlist:**
- Bookmark button — video save karo baad mein dekhne ke liye
- **[fix hua]** File switch karne par bookmark icon bhi sahi se update hota hai (naya video already saved hai ya nahi)

**Naye icons (search ke bagal mein):**
- ⚙️ Profile — apna naam/about/contact details bharo
- 🆔 Admin Lookup — sirf admin ke liye, kisi ka bhi profile Access ID se dekho

---

## 9. Gate Pages (Password + Pending-Approval) — **is session mein poori tarah unify aur redesign hua**

- Dono pages ab bilkul same design share karte hain (ek hi shared code se aata hai, taaki kabhi mismatch na ho)
- **"ASTRATOONIX" boot animation** — pehle sirf 1.1 second mein type ho raha tha aur poora dikhne se pehle hi gayab ho jaata tha (bug tha — do alag timers aapas mein match nahi kar rahe the). Ab poore **10 second** tak rukta hai: "ASTRATOONIX" type hota hai → "🎓 EDUCATIONAL PLATFORM" tag aata hai → "BUILT BY RAJ" → phir card dikhta hai
- Background mein halka sa Subject names ka pattern (MATH, PHYSICS, PYTHON, CHEMISTRY, etc.) — turant "yeh education site hai" jaisa lagta hai
- Trust badges: 🔒 PRIVATE, 🔐 HTTPS, 💯 FREE (password page), 🎓 EDUCATION / ✅ VERIFIED ACCESS (pending page)
- Pending page pe ek green note: *"Astratoonix ek education platform hai... Yeh spam nahi hai."*
- Galat password daalne par 10-second animation **skip** ho jaata hai — dobara wait nahi karna padta, seedha error dikhta hai
- Dono pages pe Name field hai

---

## 10. Database (Postgres) — Tables

- `files` — file metadata, password, expiry, view count, **subject/chapter** (naya)
- `users` — Telegram bot users, ban status
- `file_views` — per-view tracking
- `approvals` — device-wise approval status, visitor name, **blocked flag** (naya), **last_notified_at** (naya, cooldown ke liye)
- `visitor_profiles` — **poori naya table** — name, about, email, phone, instagram, facebook, device ke hisaab se

Saare migrations automatic chalte hain deploy pe — koi manual DB step nahi chahiye.

---

*Yeh doc poori codebase se directly generate kiya gaya hai (guess nahi), taaki accurate rahe.*
