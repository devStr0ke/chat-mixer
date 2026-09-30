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
- Members can send images and GIFs; files live in an S3-compatible bucket (Garage) and are only served to room members
- A GIF library (GIPHY) searchable from the chat, proxied by the API so the key stays server-side
- Replies: a message can quote another message of the same room
- Senders can edit their messages; every version is kept and visible to the room
- Per-room appearance (background color or image, bubble colors): any member can change it, everyone sees it
- Message history is paginated
- A global notification channel pushes new-message previews, invitations and room removals

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
handlers/
  auth.go             — POST /auth/register, POST /auth/login
  users.go            — GET /users/search
  rooms.go            — rooms, members, message history
  invitations.go      — room invitations
  reactions.go        — message reactions
  attachments.go      — image upload/download
  profile.go          — profile, countries and avatars
  gifs.go             — GIPHY search proxy (cached) and GIF lookup
  replies.go          — quoted-message previews for replies
  edits.go            — message editing and edit history
  appearance.go       — room theme and background image
  websocket.go        — per-room WS + global notification WS + hub
  helpers.go          — membership checks and shared helpers
storage/s3.go         — S3 client for the attachments bucket
workers/attachments.go — hourly cleanup of uploads never sent
models/               — User, Room, RoomMember, RoomInvitation, Message
docker-compose.yml    — postgres + pgAdmin + Garage + API
```

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
| POST | `/rooms/:room_id/attachments` | Upload an image (multipart field `file`, max 10 MB). JPEG, PNG, GIF or WebP, checked from the file content. Returns `{ id, content_type, width, height, size }` |
| GET | `/attachments/:attachment_id` | The image, for room members once it's been sent (and for its uploader before). Also accepts the `token` cookie so `<img>` tags work |

Uploads are private until a message references them; ones never sent are deleted after 24 hours. Deleting a room deletes its files.

### Editing
| Method | Route | Description |
|---|---|---|
| PATCH | `/messages/:message_id` | Edit the text of your own message. Body: `content` (may be empty only if the message has an image or GIF). Returns `{ id, content, edited_at }` |
| GET | `/messages/:message_id/edits` | Every version, oldest first: `{ versions: [{ content, at }] }` — the last one is the current text. Any room member |

Messages carry `edited_at` (null until edited). Edits are announced with `message_edited` on both WebSockets.

### GIFs
| Method | Route | Description |
|---|---|---|
| GET | `/gifs/search?q=&offset=` | Search GIPHY. Returns `{ gifs: [{ id, title, url, width, height, preview_url, preview_width, preview_height }], next_offset }` |
| GET | `/gifs/trending?offset=` | Trending GIFs, same shape |

Results are cached (10 min for searches, 15 min for trending) because starter GIPHY keys allow 100 calls per hour. GIFs are hotlinked from GIPHY's CDN, not stored. To send one, a client passes its `gif_id` over the WebSocket and the server resolves the URL itself, so clients can't inject arbitrary image URLs.

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

`attachment_ids` (up to 10) must be your own unsent uploads in this room; images are shown in that order. `gif_id` is a GIF id from `/gifs/*`; the delivered message then carries `"gif": { "id", "url", "width", "height" }`. `reply_to_id` must be a message of the same room; the delivered message (and history) then carries `"reply_to": { "id", "sender_id", "sender_pseudo", "content", "attachment_count", "has_gif" }`, with `content` cut to 140 characters. Theme changes are announced with `room_updated`. `read` moves your read marker up to that message. Sending a message marks the room read up to it.

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
