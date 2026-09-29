# chat-mixer

Real-time group chat backend. Users create rooms, invite people by pseudo, and chat over WebSocket. Rooms are persistent — no expiry. Built with Go, Gin, PostgreSQL, and Gorilla WebSocket.

---

## What it does

- Users register and authenticate with JWT
- Anyone can create a room and invite other users by pseudo
- Invitees accept or decline; invitations arrive live over the notification WebSocket
- Any member can invite; the owner can rename the room, remove members, or delete it
- Members can leave; if the owner leaves, ownership passes to the longest-standing member, and a room with no members left is deleted
- Chat happens over WebSocket with typing indicators, per-member read receipts ("seen by") and emoji reactions
- Message history is paginated
- A global notification channel pushes new-message previews, invitations and room removals

---

## Requirements

- [Docker Desktop](https://www.docker.com/products/docker-desktop/) — runs PostgreSQL, pgAdmin and the API
- Go 1.25+ — only if you want to run the API outside Docker

---

## Setup

### 1. Configure the environment

```bash
cp .env.example .env
```

Fill in `POSTGRES_PASSWORD`, `PGADMIN_PASSWORD` and `JWT_SECRET` (`openssl rand -hex 32`), and put the same Postgres password in `DB_URL`. Docker Compose reads `.env` for the stack, and the API reads it when run with `go run .`.

| Variable | Used by | Default |
|---|---|---|
| `POSTGRES_USER` / `POSTGRES_PASSWORD` / `POSTGRES_DB` | postgres, pgAdmin, API | `chatmixer` / — / `chat-mixer` |
| `POSTGRES_PORT` | host port for Postgres | `5434` |
| `PGADMIN_EMAIL` / `PGADMIN_PASSWORD` | pgAdmin (required by the image) | — |
| `PGADMIN_PORT` | host port for pgAdmin | `5051` |
| `API_PORT` | host port for the dockerized API | `8090` |
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

To run the API on the host instead, start only the database and use Go:

```bash
docker compose up -d postgres pgadmin
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
  websocket.go        — per-room WS + global notification WS + hub
  helpers.go          — membership checks and shared helpers
models/               — User, Room, RoomMember, RoomInvitation, Message
docker-compose.yml    — postgres + pgAdmin + API
```

---

## API overview

All protected routes require an `Authorization: Bearer <token>` header.

### Auth
| Method | Route | Description |
|---|---|---|
| POST | `/auth/register` | Create account. Body: `pseudo`, `email`, `country` (2-letter), `password` |
| POST | `/auth/login` | Login. Body: `identifier` (email or pseudo), `password` |

### Users
| Method | Route | Description |
|---|---|---|
| GET | `/users/search?q=` | Up to 10 users whose pseudo starts with `q` (excludes you) |

### Rooms
| Method | Route | Who | Description |
|---|---|---|---|
| POST | `/rooms` | anyone | Create a room. Body: `name`, optional `pseudos` to invite |
| GET | `/rooms/me` | — | Your rooms with `member_count`, `unread_count` and `last_message`, most recent first |
| GET | `/rooms/:room_id` | member | Room details and members (with `last_read_at`) |
| PATCH | `/rooms/:room_id` | owner | Rename. Body: `name` |
| DELETE | `/rooms/:room_id` | owner | Delete the room and its history for everyone |
| GET | `/rooms/:room_id/messages` | member | Latest 50 messages, oldest first: `{ messages, has_more }`. Page back with `?before=<message_id>`; `?limit=` up to 100 |
| GET | `/rooms/:room_id/invitations` | member | Pending invitations for the room |
| POST | `/rooms/:room_id/invitations` | member | Invite a user. Body: `pseudo` |
| DELETE | `/rooms/:room_id/members/:user_id` | self / owner | Leave (your own id) or remove a member (owner) |

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
{ "type": "typing" }
{ "type": "read", "id": "message-uuid" }
```

`read` moves your read marker up to that message. Sending a message marks the room read up to it.

Receive:
```json
{ "type": "message", "id": "uuid", "room_id": "uuid", "sender_id": "uuid", "sender_pseudo": "bob", "content": "hello", "sent_at": "…" }
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
{ "type": "online_count", "count": 3 }
```

`new_message` goes to members who don't have the room open. `invitation` means your invitation list changed (new, cancelled, or the room was deleted). `room_removed` means you lost access (room deleted, removed, or you left).
