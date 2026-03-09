# chat-mixer

Real-time anonymous ephemeral chat backend. Two users join a matching pool and get paired into a private room that auto-expires after 24 hours. All chat is over WebSocket. Built with Go, Gin, PostgreSQL, and Gorilla WebSocket.

---

## What it does

- Users register and authenticate with JWT
- Users join a pool and get matched with another user (optionally filtered by country)
- A private room is created on match, valid for 24 hours
- Chat happens over WebSocket with typing indicators and read receipts
- A global notification WebSocket channel pushes unread badges and room-closed events
- Either user can close the room early
- Each user can give the room a custom name on their side

---

## Requirements

- Go 1.21+
- PostgreSQL (local or remote)

---

## Setup

### 1. Clone and install dependencies

```bash
git clone https://github.com/devStr0ke/chat-mixer.git
cd chat-mixer
go mod tidy
```

### 2. Create the database

In pgAdmin or psql, create a database. Example:

```sql
CREATE DATABASE "chat-mixer-local";
```

### 3. Configure the environment

Create a `.env` file at the root of the project:

```env
# PostgreSQL connection string
# Format: postgres://<user>:<password>@<host>:<port>/<dbname>?sslmode=disable
DB_URL=postgres://postgres:yourpassword@localhost:5432/chat-mixer-local?sslmode=disable

# JWT signing secret — use any long random string
JWT_SECRET=your_super_secret_key_here

# Server port (optional, defaults to 8080)
PORT=8080
```

> The app will refuse to start if `DB_URL` or `JWT_SECRET` are missing.

### 4. Run

```bash
go run main.go
```

The server will automatically run all database migrations on startup.
No manual SQL setup required.

### 5. Health check

```bash
curl http://localhost:8080/health
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
  pool.go             — POST /pool/join, POST /pool/leave
  rooms.go            — GET/PATCH/DELETE /rooms/*
  websocket.go        — per-room WS + global notification WS + hub
models/               — User, Room, Message structs
workers/
  expiration.go       — background worker that expires rooms every 5 min
```

---

## API overview

### Auth
| Method | Route | Description |
|---|---|---|
| POST | `/auth/register` | Create account. Body: `pseudo`, `email`, `country` (2-letter), `password` |
| POST | `/auth/login` | Login. Body: `identifier` (email or pseudo), `password` |

### Pool
| Method | Route | Description |
|---|---|---|
| POST | `/pool/join` | Join the matching pool. Optional body: `same_country: true` |
| POST | `/pool/leave` | Leave the pool |

### Rooms
| Method | Route | Description |
|---|---|---|
| GET | `/rooms/me` | List your active rooms |
| GET | `/rooms/:room_id` | Get room details |
| GET | `/rooms/:room_id/messages` | Get message history |
| PATCH | `/rooms/:room_id/name` | Rename your side of the room. Body: `name` |
| DELETE | `/rooms/:room_id` | Close the room for both users immediately |

### WebSocket
| Endpoint | Description |
|---|---|
| `GET /ws/:room_id?token=<jwt>` | Per-room chat WebSocket |
| `GET /ws/notifications?token=<jwt>` | Global notification channel (unread badges, room closed) |

All protected routes require `Authorization: Bearer <token>` header.
WebSocket connections use `?token=<jwt>` query param instead.

---

## WebSocket protocol

### Per-room `/ws/:room_id`

Send:
```json
{ "type": "message", "content": "hello" }
{ "type": "typing" }
{ "type": "read", "id": "message-uuid" }
```

Receive:
```json
{ "type": "message", "id": "uuid", "content": "hello" }
{ "type": "message_ack", "id": "uuid" }
{ "type": "typing" }
{ "type": "read", "id": "uuid" }
```

### Global `/ws/notifications`

Receive only:
```json
{ "type": "new_message", "room_id": "uuid", "id": "sender_user_id" }
{ "type": "room_closed", "room_id": "uuid" }
```
