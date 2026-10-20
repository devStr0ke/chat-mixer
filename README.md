# chat-mixer

Real-time group chat backend. Users create rooms, invite people by pseudo, and chat over WebSocket. Rooms are persistent — no expiry. Built with Go, Gin, PostgreSQL, and Gorilla WebSocket.

---

## What it does

- Users register and authenticate with JWT
- Users have a profile picture and a country flag, plus an optional second country
- Anyone can create a room and invite other users by pseudo
- Invitees accept or decline; invitations arrive live over the notification WebSocket
- Any member can invite; the owner can rename the room, remove members, or delete it
- Members can leave; if the owner leaves, ownership passes to the longest-standing member, and a room with no members left is deleted
- Chat happens over WebSocket with typing indicators, per-member read receipts ("seen by") and emoji reactions
- Members can send photos, videos and GIFs; files live in an S3-compatible bucket (Garage) and are only served to room members
- A GIF library (GIPHY) searchable from the chat, proxied by the API so the key stays server-side
- Replies: a message can quote another message of the same room
- Senders can edit their messages; every version is kept and visible to the room
- Custom stickers: users cut stickers out of their photos in the app and keep a collection; a sent sticker is visible to that room's members
- Drawings: drawn over the conversation and sent like a sticker (not added to the collection)
- Senders can delete their messages for everyone: text, files, reactions and edit history are erased and the room sees "This message was deleted"
- Link previews: the server fetches a link's title, description and image (with SSRF protection) for a card under the message
- Per-room appearance (background color or image, bubble colors): any member can change it, everyone sees it
- Message history is paginated
- A global notification channel pushes new-message previews, invitations and room removals
- View-once photos and videos: each member can open one once, the sender sees who opened it, and the file is deleted once everyone has (or after 14 days)
- Encryption at rest: message text, edit history, cached link previews and every stored file are encrypted, so a leaked database or bucket can't be read on its own (see [Encryption](#encryption))

---

## Requirements

- [Docker Desktop](https://www.docker.com/products/docker-desktop/) — runs PostgreSQL, pgAdmin, Garage and the API
- Go 1.25+ — only if you want to run the API outside Docker

---

## Setup

### 1. Configure the environment

```bash
cp .env.example .env
```

Fill in `POSTGRES_PASSWORD`, `PGADMIN_PASSWORD`, `JWT_SECRET`, `GARAGE_RPC_SECRET`, `GARAGE_ADMIN_TOKEN` and `S3_SECRET_KEY` (each `openssl rand -hex 32`), set `S3_ACCESS_KEY` to `GK` followed by `openssl rand -hex 16`, and put the same Postgres password in `DB_URL`. Docker Compose reads `.env` for the stack, and the API reads it when run with `go run .`.

| Variable | Used by | Default |
|---|---|---|
| `POSTGRES_USER` / `POSTGRES_PASSWORD` / `POSTGRES_DB` | postgres, pgAdmin, API | `chatmixer` / — / `chat-mixer` |
| `POSTGRES_PORT` | host port for Postgres | `5434` |
| `PGADMIN_EMAIL` / `PGADMIN_PASSWORD` | pgAdmin (required by the image) | — |
| `PGADMIN_PORT` | host port for pgAdmin | `5051` |
| `API_PORT` | host port for the dockerized API | `8090` |
| `GARAGE_RPC_SECRET` / `GARAGE_ADMIN_TOKEN` | Garage | — |
| `GARAGE_PORT` | host port for Garage's S3 API | `3900` |
| `S3_BUCKET` / `S3_ACCESS_KEY` / `S3_SECRET_KEY` | Garage creates them on first start; the API uses them | `chat-mixer` / — / — |
| `S3_ENDPOINT` | `go run .` only (the dockerized API uses `http://garage:3900`). Empty disables uploads | — |
| `S3_REGION` | API | `garage` |
| `GIPHY_API_KEY` | API — [GIPHY](https://developers.giphy.com) key for the GIF picker. Empty disables it | — |
| `ENCRYPTION_KEY` | API — 32 random bytes in hex (`openssl rand -hex 32`) that everything stored is encrypted with. **Required**; keep a copy in a password manager, since losing it makes messages and files unreadable | — |
| `ENCRYPTION_OLD_KEYS` | API — previous keys (comma-separated) while rotating; see [Encryption](#encryption) | — |
| `CORS_ORIGINS` | API — web app origins (comma-separated) allowed to upload straight to the API; see [Attachments](#attachments) | `http://localhost:3000,http://localhost:3001` in Compose |
| `DB_URL` | `go run .` only | — |
| `JWT_SECRET` | API | — |
| `PORT` | `go run .` only | `8080` |

> The API refuses to start if `DB_URL` or `JWT_SECRET` is missing.

### 2. Start the stack

```bash
docker compose up -d --build
```

| Service | URL |
|---|---|
| API | http://localhost:8090 |
| pgAdmin | http://localhost:5051 (no login; the `chat-mixer` server is pre-registered) |
| PostgreSQL | `localhost:5434` |
| Garage S3 API | http://localhost:3900 (bucket `chat-mixer`) |

To run the API on the host instead, start only the database and use Go:

```bash
docker compose up -d postgres pgadmin garage
go run .
```

Migrations run automatically on startup. A database created by the old 1:1 ephemeral version is upgraded in place: closed rooms are dropped, and active ones become two-member groups that keep their history.

### Local test accounts

The local database used during development has test users `alice`, `bob`, `carol` and `dave`, all with the password `password123` (local only; they don't exist in production). Register more from the app's sign-up page.

### 3. Health check

```bash
curl http://localhost:8090/health
# {"status":"ok"}
```

---

## Project structure

```
main.go               — server bootstrap and route registration
db/postgres.go        — DB connection and migrations
middleware/auth.go    — JWT generation and auth middleware
middleware/cors.go    — CORS for direct uploads from the web app
encryption/           — key derivation, AES-256-GCM sealing, keyed hashes (with tests)
handlers/
  auth.go             — POST /auth/register, POST /auth/login
  users.go            — GET /users/search
  rooms.go            — rooms, members, message history
  invitations.go      — room invitations
  reactions.go        — message reactions
  attachments.go      — photo/video upload and download (with Range)
  videos.go           — video type detection, streamed storage, poster frames
  viewonce.go         — opening view-once messages, who opened what
  stickers.go         — custom sticker collection, upload and serving
  deletes.go          — deleting a message for everyone
  profile.go          — profile, countries and avatars
  gifs.go             — GIPHY search proxy (cached) and GIF lookup
  replies.go          — quoted-message previews for replies
  edits.go            — message editing and edit history
  linkpreviews.go     — link preview endpoint, cache and image copies
linkpreview/          — SSRF-safe page fetcher and Open Graph parser (with tests)
  appearance.go       — room theme and background image
  websocket.go        — per-room WS + global notification WS + hub
  helpers.go          — membership checks and shared helpers
  content.go          — sealing and opening message text
storage/s3.go         — S3 client for the attachments bucket (SSE-C encrypted)
workers/attachments.go — hourly cleanup of unsent uploads and stale link previews
workers/viewonce.go   — deletes view-once files once everyone opened them (or after 14 days)
workers/encryption.go — startup sweep: encrypts older rows and files, re-encrypts after a key change
models/               — User, Room, RoomMember, RoomInvitation, Message
docker-compose.yml    — postgres + pgAdmin + Garage + API
```

---

## Encryption

Everything users write is encrypted before it's stored, with keys derived from `ENCRYPTION_KEY`. That key lives only in the API's configuration, so a copy of the database (a dump, a backup, a leaked password) or of Garage's volume is unreadable on its own. It doesn't protect against someone who controls the API server itself, since the API holds the key; only end-to-end encryption would.

| What | How |
|---|---|
| Message text and every earlier version of edited messages | AES-256-GCM, bound to the message's room and id: a ciphertext copied onto another message or into another room won't decrypt |
| Link-preview cache (URL, title, description, site) | AES-256-GCM; looked up by a keyed hash (HMAC-SHA256), so the cache can't be checked for a guessed URL |
| Photos, videos, posters, video overlays, avatars, backgrounds, preview images | SSE-C: the API hands Garage the key with each request; Garage encrypts what it stores and never keeps the key. Range reads still work |
| Request logs | query strings (shared URLs, GIF searches) are left out |

Still readable in the database: accounts (pseudo, email, countries; passwords are bcrypt hashes), room names and themes, who is in which room, when messages were sent, reactions, and file sizes and types.

Each encrypted row stores the id of the key that sealed it (`key_id`). On startup the API encrypts anything stored before encryption was enabled, then compacts those tables (`VACUUM FULL`) so the old plaintext row versions leave the live database files; files are re-encrypted in place by Garage in the background, and both sweeps run once more five minutes later to catch writes from the previous version during a rolling deploy. **Backups taken before encryption was enabled still contain plaintext: delete them.**

**Rotating the key:** generate a new key, set it as `ENCRYPTION_KEY` and move the old one to `ENCRYPTION_OLD_KEYS`, then restart. The sweep re-encrypts every row and file with the new key (cached link previews are dropped and fetched again). Once the logs show the file sweep has finished, remove the old key and restart.

The API refuses to start without `ENCRYPTION_KEY`, so nothing is ever stored unencrypted by mistake.

---

## API overview

All protected routes require an `Authorization: Bearer <token>` header.

### Auth
| Method | Route | Description |
|---|---|---|
| POST | `/auth/register` | Create account. Body: `pseudo`, `email`, `country` (2-letter), optional `country2`, `password` |
| POST | `/auth/login` | Login. Body: `identifier` (email or pseudo), `password` |

### Users
| Method | Route | Description |
|---|---|---|
| GET | `/users/search?q=` | Up to 10 users whose pseudo starts with `q` (excludes you) |
| GET | `/users/me` | Your profile |
| PATCH | `/users/me` | Change your countries. Body: `country`, `country2` (`null` clears it) |
| PUT | `/users/me/avatar` | Set your profile picture (multipart field `file`, max 2 MB, JPEG/PNG/GIF/WebP) |
| DELETE | `/users/me/avatar` | Remove your profile picture |
| GET | `/avatars/:avatar_id` | A profile picture, for any signed-in user (accepts the `token` cookie). Each upload gets a new id, so URLs are cached forever |

Users everywhere in the API (members, search, invitations, auth) include `country2` and `avatar_id` (both nullable).

### Rooms
| Method | Route | Who | Description |
|---|---|---|---|
| POST | `/rooms` | anyone | Create a room. Body: `name`, optional `pseudos` to invite |
| GET | `/rooms/me` | — | Your rooms with `member_count`, `unread_count` and `last_message`, most recent first |
| GET | `/rooms/:room_id` | member | Room details, `theme` and members (with `last_read_at`) |
| PATCH | `/rooms/:room_id` | owner | Rename. Body: `name` |
| DELETE | `/rooms/:room_id` | owner | Delete the room and its history for everyone |
| PATCH | `/rooms/:room_id/theme` | member | Set the room's colors. Body: `background_color`, `bubble_own_color`, `bubble_other_color` (`#rrggbb`, or `null` for the default) |
| PUT | `/rooms/:room_id/background` | member | Set the background image (multipart field `file`, max 10 MB) |
| DELETE | `/rooms/:room_id/background` | member | Remove the background image |
| GET | `/rooms/:room_id/background/:image_id` | member | The current background image (accepts the `token` cookie, so CSS can load it) |
| GET | `/rooms/:room_id/messages` | member | Latest 50 messages, oldest first: `{ messages, has_more }`. Page back with `?before=<message_id>`; `?limit=` up to 100 |
| GET | `/rooms/:room_id/invitations` | member | Pending invitations for the room |
| POST | `/rooms/:room_id/invitations` | member | Invite a user. Body: `pseudo` |
| DELETE | `/rooms/:room_id/members/:user_id` | self / owner | Leave (your own id) or remove a member (owner) |

### Attachments
| Method | Route | Description |
|---|---|---|
| POST | `/rooms/:room_id/attachments` | Upload a photo or video (multipart field `file`). Photos: JPEG, PNG, GIF or WebP, max 10 MB. Videos: MP4, MOV or WebM, max 100 MB, with optional fields `poster` (a JPEG/PNG/WebP frame, max 2 MB, which also gives the video's `width`/`height`), `duration_ms` and `overlay` (a transparent PNG/WebP the video's size, max 2 MB: text and drawings from the in-app camera, shown on top during playback). Types are checked from the file content. Returns `{ id, content_type, width, height, size }`, plus `duration_ms`, `has_poster` and `has_overlay` for videos |
| GET | `/attachments/:attachment_id` | The file, for room members once it's been sent (and for its uploader before). Supports `Range` requests, so videos stream and seek. Also accepts the `token` cookie so `<img>`/`<video>` tags work |
| GET | `/attachments/:attachment_id/poster` | A video's poster frame (same access rules) |
| GET | `/attachments/:attachment_id/overlay` | The text and drawings laid over a camera video (same access rules as the video, view-once window included) |

Uploads are private until a message references them; ones never sent are deleted after 24 hours. Deleting a room deletes its files.

Videos are stored as uploaded, not re-encoded: they're streamed to the bucket (never held in memory) and play in the browser's own player. MOV files are served as `video/mp4`, which every browser accepts for the H.264 that iPhones export; an HEVC video plays only where the browser and device can decode HEVC.

The web app uploads straight to the API rather than through its `/api` rewrite, because Next.js caps proxied request bodies at 10 MB. That route alone answers CORS preflights, for the origins in `CORS_ORIGINS`; it authenticates with the `Authorization` header only, never cookies.

### Custom stickers
| Method | Route | Description |
|---|---|---|
| GET | `/stickers` | Your sticker collection, newest first: `[{ id, width, height }]` |
| POST | `/stickers?kind=sticker\|drawing` | Add a sticker (multipart field `file`): PNG or WebP, at most 1 MB and 1024px a side; up to 200 per user. With `kind=drawing` it's a drawing to send: not listed in the collection, up to 120 an hour, and deleted after a day if it's never sent. Returns `{ id, width, height, kind }` |
| DELETE | `/stickers/:sticker_id` | Remove one from your collection. Messages that already used it keep showing it; the file is deleted once none do (hourly cleanup) |
| GET | `/stickers/:sticker_id/image` | The image, for its owner and for members of rooms it was sent in (accepts the `token` cookie) |

To send one, pass `"sticker_id"` over the WebSocket (on its own: no text, files or GIF; only from your own collection). Messages then carry `"sticker": { id, width, height, kind }` (`kind: "drawing"` for drawings), and `last_message` / reply quotes have `sticker: true` (and `drawing: true` for a drawing).

### Deleting
| Method | Route | Description |
|---|---|---|
| DELETE | `/messages/:message_id` | Delete your own message for everyone (`204`, also when already deleted). Its text, GIF, files (from the bucket too), reactions, edit history and view-once opens are erased; the message stays in history with `"deleted": true` and empty fields. Every member gets `{ "type": "message_deleted", "id", "room_id" }`. A deleted message can't be edited, reacted to (`410`) or replied to; quotes of it carry `"deleted": true` |

### View once
| Method | Route | Description |
|---|---|---|
| POST | `/messages/:message_id/open` | Open a view-once message (members other than the sender, once each). Returns `{ attachment, expires_in }`: the file can be fetched for `expires_in` seconds (2 minutes for a photo, a video's length + 2 minutes, at most 15). A second call returns `410`; the sender gets `403` |

A view-once message is sent over the WebSocket with `"view_once": true` and exactly one attachment (no text or GIF). In history it carries `view_once`, `opens` (`[{ user_id, opened_at }]`, missing until someone opens it) and `expired`; its attachment never advertises a poster, and `GET /attachments/:id` only serves the file to someone inside their viewing window (with `Cache-Control: no-store`). Every member gets `{ "type": "message_opened", "id", "user_id", "opened_at" }` when someone opens it. It can't be edited. The file is deleted once every other member has opened it and the last window has closed, or 14 days after sending; the message then shows as expired. A web page can't prevent screenshots: view-once means it can't be reopened, not that it can't be captured.

### Editing
| Method | Route | Description |
|---|---|---|
| PATCH | `/messages/:message_id` | Edit the text of your own message. Body: `content` (may be empty only if the message has an image or GIF). Returns `{ id, content, edited_at }` |
| GET | `/messages/:message_id/edits` | Every version, oldest first: `{ versions: [{ content, at }] }` — the last one is the current text. Any room member |

Messages carry `edited_at` (null until edited). Edits are announced with `message_edited` on both WebSockets.

### Link previews
| Method | Route | Description |
|---|---|---|
| GET | `/link-preview?url=` | Card data for a web page: `{ url, title, description, site_name, image_id, image_width, image_height }`, or `404` when it has no usable preview |
| GET | `/link-previews/images/:image_id` | Our stored copy of the page's preview image (accepts the `token` cookie) |

The server fetches pages itself, so viewers never contact the linked site. Because the URL comes from users, the fetcher (`linkpreview/`) only connects to public IP addresses on ports 80/443 — checked on the resolved address, for every redirect — and caps sizes and time. Results are cached in the database (7 days, 1 hour for failures), uncached lookups are limited to 20 per user per minute, and previews unused for 30 days are purged.

```bash
go test ./linkpreview/
```

### GIFs
| Method | Route | Description |
|---|---|---|
| GET | `/gifs/search?q=&offset=&kind=` | Search GIPHY. `kind=stickers` searches stickers instead of GIFs. Returns `{ gifs: [{ id, title, url, width, height, sticker, preview_url, preview_width, preview_height }], next_offset }` |
| GET | `/gifs/trending?offset=&kind=` | Trending GIFs (or stickers), same shape |

Results are cached (10 min for searches, 15 min for trending) because starter GIPHY keys allow 100 calls per hour. GIFs are hotlinked from GIPHY's CDN, not stored. To send one, a client passes its `gif_id` over the WebSocket and the server resolves the URL itself, so clients can't inject arbitrary image URLs; whether it's a sticker (transparent, shown without a bubble) also comes from GIPHY's data, as `gif.sticker`.

### Invitations
| Method | Route | Description |
|---|---|---|
| GET | `/invitations` | Invitations you've received |
| POST | `/invitations/:invitation_id/accept` | Join the room. Returns `{ room_id }` |
| DELETE | `/invitations/:invitation_id` | Decline (invitee) or cancel (inviter or room owner) |

### Reactions
| Method | Route | Description |
|---|---|---|
| POST | `/messages/:message_id/reactions` | React (one per user per message). Body: `emoji` |
| DELETE | `/messages/:message_id/reactions` | Remove your reaction |

### WebSocket
| Endpoint | Description |
|---|---|
| `GET /ws/:room_id?token=<jwt>` | Per-room chat (members only) |
| `GET /ws/notifications?token=<jwt>` | Global notification channel |

WebSocket connections pass the JWT as `?token=` instead of a header.

---

## WebSocket protocol

### Per-room `/ws/:room_id`

Send:
```json
{ "type": "message", "content": "hello", "client_id": "local-1" }
{ "type": "message", "content": "", "client_id": "local-2", "attachment_ids": ["uuid", "uuid"] }
{ "type": "message", "content": "", "client_id": "local-3", "gif_id": "giphy-id" }
{ "type": "message", "content": "agreed", "client_id": "local-4", "reply_to_id": "message-uuid" }
{ "type": "typing" }
{ "type": "read", "id": "message-uuid" }
```

`attachment_ids` (up to 10) must be your own unsent uploads in this room; photos and videos are shown in that order. `gif_id` is a GIF id from `/gifs/*`; the delivered message then carries `"gif": { "id", "url", "width", "height" }`. `reply_to_id` must be a message of the same room; the delivered message (and history) then carries `"reply_to": { "id", "sender_id", "sender_pseudo", "content", "attachment_count", "video_count", "has_gif" }`, with `content` cut to 140 characters (`video_count` says how many of the attachments are videos, as in `last_message` from `/rooms/me`). Theme changes are announced with `room_updated`. `read` moves your read marker up to that message. Sending a message marks the room read up to it.

Receive:
```json
{ "type": "message", "id": "uuid", "room_id": "uuid", "sender_id": "uuid", "sender_pseudo": "bob", "content": "hello", "sent_at": "…", "attachments": [{ "id": "uuid", "content_type": "image/webp", "width": 2048, "height": 1365, "size": 23810 }] }
{ "type": "message_edited", "id": "uuid", "room_id": "uuid", "content": "new text", "edited_at": "…" }
{ "type": "message_ack", "id": "uuid", "client_id": "local-1", "sent_at": "…" }
{ "type": "message_error", "client_id": "local-1" }
{ "type": "typing", "user_id": "uuid", "pseudo": "bob" }
{ "type": "read", "user_id": "uuid", "read_at": "…" }
{ "type": "reaction", "message_id": "uuid", "user_id": "uuid", "emoji": "👍", "action": "add" }
{ "type": "reaction", "message_id": "uuid", "user_id": "uuid", "action": "remove" }
{ "type": "member_joined", "room_id": "uuid", "user_id": "uuid" }
{ "type": "member_left", "room_id": "uuid", "user_id": "uuid" }
{ "type": "room_updated", "room_id": "uuid" }
```

A message is broadcast to every other connection in the room; the sending connection gets `message_ack` instead. The server closes the socket when the room is deleted or you're removed from it.

### Global `/ws/notifications`

Receive only:
```json
{ "type": "new_message", "id": "uuid", "room_id": "uuid", "sender_id": "uuid", "sender_pseudo": "bob", "content": "hello", "sent_at": "…" }
{ "type": "invitation", "room_id": "uuid" }
{ "type": "room_removed", "room_id": "uuid" }
{ "type": "room_read", "room_id": "uuid" }
{ "type": "online_count", "count": 3 }
```

`new_message` goes to every member except the sender, whether or not they have the room open: a message is only read once a client sends `read`, which clients should do only while the conversation is actually on screen (tab visible, window focused, scrolled to the latest messages). `room_read` tells your other tabs and devices that you've read a room, so they can clear its unread badge. `invitation` means your invitation list changed (new, cancelled, or the room was deleted). `room_removed` means you lost access (room deleted, removed, or you left).
