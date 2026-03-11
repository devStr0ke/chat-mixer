package handlers

import (
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/devstr0ke/chat-mixer/db"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

// --- Protocol ---

type WSMessage struct {
	Type    string `json:"type"`
	Content string `json:"content,omitempty"`
	ID      string `json:"id,omitempty"`
	RoomID  string `json:"room_id,omitempty"`
	Count   int    `json:"count,omitempty"`
}

// --- Hub ---

type NotifClient struct {
	UserID string
	Conn   *websocket.Conn
	Send   chan []byte
}

type Hub struct {
	mu     sync.RWMutex
	rooms  map[string][]*Client
	notifs map[string][]*NotifClient // userID -> connections
}

var WSHub *Hub

func NewHub() *Hub {
	return &Hub{
		rooms:  make(map[string][]*Client),
		notifs: make(map[string][]*NotifClient),
	}
}

func (h *Hub) Register(client *Client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.rooms[client.RoomID] = append(h.rooms[client.RoomID], client)
}

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

func (h *Hub) Broadcast(roomID, senderID string, msg []byte) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	for _, client := range h.rooms[roomID] {
		if client.UserID == senderID {
			continue
		}
		select {
		case client.Send <- msg:
		default:
		}
	}
}

func (h *Hub) BroadcastToAll(roomID string, msg []byte) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	for _, client := range h.rooms[roomID] {
		select {
		case client.Send <- msg:
		default:
		}
	}
}

func (h *Hub) RegisterNotif(nc *NotifClient) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.notifs[nc.UserID] = append(h.notifs[nc.UserID], nc)
}

func (h *Hub) UnregisterNotif(nc *NotifClient) {
	h.mu.Lock()
	defer h.mu.Unlock()

	clients := h.notifs[nc.UserID]
	for i, c := range clients {
		if c == nc {
			h.notifs[nc.UserID] = append(clients[:i], clients[i+1:]...)
			break
		}
	}
	if len(h.notifs[nc.UserID]) == 0 {
		delete(h.notifs, nc.UserID)
	}
}

func (h *Hub) NotifyUser(userID string, msg []byte) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	for _, nc := range h.notifs[userID] {
		select {
		case nc.Send <- msg:
		default:
		}
	}
}

func (h *Hub) IsUserInRoom(userID, roomID string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()

	for _, client := range h.rooms[roomID] {
		if client.UserID == userID {
			return true
		}
	}
	return false
}

func (h *Hub) NotifyRoomClosed(roomID string, userIDs ...string) {
	out, _ := json.Marshal(WSMessage{Type: "room_closed", RoomID: roomID})
	for _, uid := range userIDs {
		h.NotifyUser(uid, out)
	}
}

func (h *Hub) BroadcastOnlineCount() {
	h.mu.RLock()
	count := len(h.notifs)
	out, _ := json.Marshal(WSMessage{Type: "online_count", Count: count})
	for _, clients := range h.notifs {
		for _, nc := range clients {
			select {
			case nc.Send <- out:
			default:
			}
		}
	}
	h.mu.RUnlock()
}

func (h *Hub) CloseRoom(roomID string) {
	h.mu.Lock()
	clients := h.rooms[roomID]
	delete(h.rooms, roomID)
	h.mu.Unlock()

	for _, c := range clients {
		close(c.Send)
	}
}

// --- Client ---

const (
	writeWait  = 10 * time.Second
	pongWait   = 60 * time.Second
	pingPeriod = (pongWait * 9) / 10
	maxMsgSize = 4096
)

type Client struct {
	UserID string
	RoomID string
	Conn   *websocket.Conn
	Send   chan []byte
}

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
		_, raw, err := c.Conn.ReadMessage()
		if err != nil {
			break
		}
		if len(raw) == 0 {
			continue
		}

		var msg WSMessage
		if err := json.Unmarshal(raw, &msg); err != nil {
			continue
		}

		switch msg.Type {
		case "message":
			c.handleMessage(msg)
		case "typing":
			c.handleTyping()
		case "read":
			c.handleRead(msg)
		}
	}
}

func (c *Client) handleMessage(msg WSMessage) {
	if msg.Content == "" {
		return
	}

	var id string
	var sentAt time.Time
	err := db.DB.QueryRow(
		`INSERT INTO messages (room_id, sender_id, content)
		 VALUES ($1, $2, $3)
		 RETURNING id, sent_at`,
		c.RoomID, c.UserID, msg.Content,
	).Scan(&id, &sentAt)
	if err != nil {
		log.Printf("ws: failed to save message: %v", err)
		return
	}

	out, _ := json.Marshal(WSMessage{Type: "message", ID: id, Content: msg.Content})
	WSHub.Broadcast(c.RoomID, c.UserID, out)

	ack, _ := json.Marshal(WSMessage{Type: "message_ack", ID: id})
	select {
	case c.Send <- ack:
	default:
	}

	var otherUserID string
	_ = db.DB.QueryRow(
		`SELECT CASE WHEN user_a_id = $1 THEN user_b_id ELSE user_a_id END
		 FROM rooms WHERE id = $2`,
		c.UserID, c.RoomID,
	).Scan(&otherUserID)

	if otherUserID != "" && !WSHub.IsUserInRoom(otherUserID, c.RoomID) {
		notif, _ := json.Marshal(WSMessage{Type: "new_message", RoomID: c.RoomID, ID: c.UserID})
		WSHub.NotifyUser(otherUserID, notif)
	}
}

func (c *Client) handleTyping() {
	out, _ := json.Marshal(WSMessage{Type: "typing"})
	WSHub.Broadcast(c.RoomID, c.UserID, out)
}

func (c *Client) handleRead(msg WSMessage) {
	if msg.ID == "" {
		return
	}

	_, err := db.DB.Exec(
		`UPDATE messages SET is_read = true
		 WHERE id = $1 AND room_id = $2 AND sender_id != $3 AND is_read = false`,
		msg.ID, c.RoomID, c.UserID,
	)
	if err != nil {
		log.Printf("ws: failed to mark read: %v", err)
		return
	}

	out, _ := json.Marshal(WSMessage{Type: "read", ID: msg.ID})
	WSHub.Broadcast(c.RoomID, c.UserID, out)
}

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

// --- Upgrade Handler ---

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

func HandleWebSocket(c *gin.Context) {
	userID := c.GetString("userID")
	roomID := c.Param("room_id")

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

	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Printf("ws: upgrade failed: %v", err)
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

// --- Notification WebSocket ---

func (nc *NotifClient) readPump() {
	defer func() {
		WSHub.UnregisterNotif(nc)
		WSHub.BroadcastOnlineCount()
		nc.Conn.Close()
	}()

	nc.Conn.SetReadLimit(512)
	nc.Conn.SetReadDeadline(time.Now().Add(pongWait))
	nc.Conn.SetPongHandler(func(string) error {
		nc.Conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})

	for {
		if _, _, err := nc.Conn.ReadMessage(); err != nil {
			break
		}
	}
}

func (nc *NotifClient) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		nc.Conn.Close()
	}()

	for {
		select {
		case msg, ok := <-nc.Send:
			nc.Conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				nc.Conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			if err := nc.Conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		case <-ticker.C:
			nc.Conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := nc.Conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

func HandleNotificationWS(c *gin.Context) {
	userID := c.GetString("userID")

	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Printf("ws: notification upgrade failed: %v", err)
		return
	}

	nc := &NotifClient{
		UserID: userID,
		Conn:   conn,
		Send:   make(chan []byte, 256),
	}

	WSHub.RegisterNotif(nc)
	WSHub.BroadcastOnlineCount()
	go nc.writePump()
	go nc.readPump()
}
