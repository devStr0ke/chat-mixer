package handlers

import (
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/devstr0ke/chat-mixer/db"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

// ─── Hub ────────────────────────────────────────────────────────────────────────

// Hub maintains the set of active clients grouped by room.
type Hub struct {
	mu    sync.RWMutex
	rooms map[string][]*Client
}

// NewHub creates an empty hub.
func NewHub() *Hub {
	return &Hub{rooms: make(map[string][]*Client)}
}

// Register adds a client to its room.
func (h *Hub) Register(client *Client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.rooms[client.RoomID] = append(h.rooms[client.RoomID], client)
}

// Unregister removes a client. If the room is empty, it deletes the entry.
func (h *Hub) Unregister(client *Client) {
	h.mu.Lock()
	defer h.mu.Unlock()

	clients := h.rooms[client.RoomID]
	for i, c := range clients {
		if c == client {
			h.rooms[client.RoomID] = append(clients[:i], clients[i+1:]...)
			break
		}
	}
	if len(h.rooms[client.RoomID]) == 0 {
		delete(h.rooms, client.RoomID)
	}
}

// Broadcast sends a message to every client in the room except the sender.
func (h *Hub) Broadcast(roomID string, senderID string, msg []byte) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	for _, client := range h.rooms[roomID] {
		if client.UserID == senderID {
			continue
		}
		select {
		case client.Send <- msg:
		default:
			// Client's buffer is full — skip to avoid blocking
		}
	}
}

// CloseRoom forcefully closes all connections in a room.
func (h *Hub) CloseRoom(roomID string) {
	h.mu.Lock()
	clients := h.rooms[roomID]
	delete(h.rooms, roomID)
	h.mu.Unlock()

	for _, c := range clients {
		close(c.Send)
	}
}

// WSHub is the global hub instance, initialised in main.go.
var WSHub *Hub

// ─── Client ─────────────────────────────────────────────────────────────────────

const (
	writeWait  = 10 * time.Second
	pongWait   = 60 * time.Second
	pingPeriod = (pongWait * 9) / 10
	maxMsgSize = 4096
)

// Client represents a single WebSocket connection in a room.
type Client struct {
	UserID string
	RoomID string
	Conn   *websocket.Conn
	Send   chan []byte
}

// readPump reads messages from the WebSocket, saves them to Postgres,
// and broadcasts to the room.
func (c *Client) readPump() {
	defer func() {
		WSHub.Unregister(c)
		c.Conn.Close()
	}()

	c.Conn.SetReadLimit(maxMsgSize)
	c.Conn.SetReadDeadline(time.Now().Add(pongWait))
	c.Conn.SetPongHandler(func(string) error {
		c.Conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})

	for {
		_, msg, err := c.Conn.ReadMessage()
		if err != nil {
			break
		}

		content := string(msg)
		if content == "" {
			continue
		}

		// Persist message
		_, err = db.DB.Exec(
			`INSERT INTO messages (room_id, sender_id, content) VALUES ($1, $2, $3)`,
			c.RoomID, c.UserID, content,
		)
		if err != nil {
			log.Printf("failed to save message: %v", err)
			continue
		}

		WSHub.Broadcast(c.RoomID, c.UserID, msg)
	}
}

// writePump pumps messages from the Send channel to the WebSocket connection.
func (c *Client) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		c.Conn.Close()
	}()

	for {
		select {
		case msg, ok := <-c.Send:
			c.Conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				c.Conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			if err := c.Conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		case <-ticker.C:
			c.Conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.Conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// ─── Upgrade Handler ────────────────────────────────────────────────────────────

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin:     func(r *http.Request) bool { return true }, // TODO: restrict in prod
}

// HandleWebSocket upgrades the connection, validates the user belongs to the room,
// and starts the read/write pumps.
func HandleWebSocket(c *gin.Context) {
	userID := c.GetString("userID")
	roomID := c.Param("room_id")

	// Verify the room exists, is active, not expired, and the user belongs to it
	var isActive bool
	var expiresAt time.Time
	err := db.DB.QueryRow(
		`SELECT is_active, expires_at FROM rooms
		 WHERE id = $1 AND (user_a_id = $2 OR user_b_id = $2)`,
		roomID, userID,
	).Scan(&isActive, &expiresAt)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "room not found or access denied"})
		return
	}
	if !isActive {
		c.JSON(http.StatusGone, gin.H{"error": "room is no longer active"})
		return
	}
	if time.Now().UTC().After(expiresAt) {
		c.JSON(http.StatusGone, gin.H{"error": "room has expired"})
		return
	}

	// Upgrade to WebSocket
	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Printf("websocket upgrade failed: %v", err)
		return
	}

	client := &Client{
		UserID: userID,
		RoomID: roomID,
		Conn:   conn,
		Send:   make(chan []byte, 256),
	}

	WSHub.Register(client)

	go client.writePump()
	go client.readPump()
}
