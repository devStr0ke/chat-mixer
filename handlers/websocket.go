package handlers

import (
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/devstr0ke/chat-mixer/db"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/lib/pq"
)

// --- Protocol ---

type WSMessage struct {
	Type         string     `json:"type"`
	Content      string     `json:"content,omitempty"`
	ID           string     `json:"id,omitempty"`
	ClientID     string     `json:"client_id,omitempty"`
	RoomID       string     `json:"room_id,omitempty"`
	UserID       string     `json:"user_id,omitempty"`
	Pseudo       string     `json:"pseudo,omitempty"`
	SenderID     string     `json:"sender_id,omitempty"`
	SenderPseudo string     `json:"sender_pseudo,omitempty"`
	SentAt       *time.Time `json:"sent_at,omitempty"`
	ReadAt       *time.Time `json:"read_at,omitempty"`
	Count        int        `json:"count,omitempty"`

	AttachmentIDs []string             `json:"attachment_ids,omitempty"` // client → server
	Attachments   []attachmentResponse `json:"attachments,omitempty"`    // server → client
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

// Broadcast sends msg to every connection in the room except the sending one.
func (h *Hub) Broadcast(roomID string, except *Client, msg []byte) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	for _, client := range h.rooms[roomID] {
		if client != except {
			client.send(msg)
		}
	}
}

func (h *Hub) BroadcastToAll(roomID string, msg []byte) {
	h.Broadcast(roomID, nil, msg)
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

func (h *Hub) NotifyUsers(msg []byte, userIDs ...string) {
	for _, uid := range userIDs {
		h.NotifyUser(uid, msg)
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

// CloseRoom disconnects every connection to a room (the room was deleted).
func (h *Hub) CloseRoom(roomID string) {
	h.mu.Lock()
	clients := h.rooms[roomID]
	delete(h.rooms, roomID)
	h.mu.Unlock()

	for _, c := range clients {
		c.close()
	}
}

// DisconnectUser drops a user's connections to a room (they left or were removed).
func (h *Hub) DisconnectUser(roomID, userID string) {
	h.mu.Lock()
	var kept, dropped []*Client
	for _, c := range h.rooms[roomID] {
		if c.UserID == userID {
			dropped = append(dropped, c)
		} else {
			kept = append(kept, c)
		}
	}
	if len(kept) == 0 {
		delete(h.rooms, roomID)
	} else {
		h.rooms[roomID] = kept
	}
	h.mu.Unlock()

	for _, c := range dropped {
		c.close()
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
	Pseudo string
	RoomID string
	Conn   *websocket.Conn
	Send   chan []byte

	// done is closed to make writePump send a close frame and exit.
	// Send is never closed, so late writers can't panic.
	done      chan struct{}
	closeOnce sync.Once
}

func (c *Client) close() {
	c.closeOnce.Do(func() { close(c.done) })
}

// send queues msg without blocking; it's dropped if the buffer is full or the client is closed.
func (c *Client) send(msg []byte) {
	select {
	case <-c.done:
	case c.Send <- msg:
	default:
	}
}

func (c *Client) readPump() {
	defer func() {
		WSHub.Unregister(c)
		c.close()
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
	content := strings.TrimSpace(msg.Content)
	if content == "" && len(msg.AttachmentIDs) == 0 {
		return
	}

	id, sentAt, attachments, err := c.saveMessage(content, msg.AttachmentIDs)
	if err != nil {
		log.Printf("ws: failed to save message: %v", err)
		c.send(mustJSON(WSMessage{Type: "message_error", ClientID: msg.ClientID}))
		return
	}

	out := WSMessage{
		Type:         "message",
		ID:           id,
		RoomID:       c.RoomID,
		SenderID:     c.UserID,
		SenderPseudo: c.Pseudo,
		Content:      content,
		SentAt:       &sentAt,
		Attachments:  attachments,
	}
	WSHub.Broadcast(c.RoomID, c, mustJSON(out))
	c.send(mustJSON(WSMessage{Type: "message_ack", ID: id, ClientID: msg.ClientID, SentAt: &sentAt}))

	// sending a message means the sender has caught up on the room
	c.markRead(id)

	memberIDs, err := roomMemberIDs(db.DB, c.RoomID)
	if err != nil {
		log.Printf("ws: failed to list members: %v", err)
		return
	}
	out.Type = "new_message"
	notif := mustJSON(out)
	for _, uid := range memberIDs {
		if uid != c.UserID && !WSHub.IsUserInRoom(uid, c.RoomID) {
			WSHub.NotifyUser(uid, notif)
		}
	}
}

var errInvalidAttachments = errors.New("invalid attachments")

// saveMessage stores a message and claims the sender's uploaded attachments
// for it, all or nothing.
func (c *Client) saveMessage(content string, attachmentIDs []string) (id string, sentAt time.Time, attachments []attachmentResponse, err error) {
	if len(attachmentIDs) > maxAttachmentsPerMsg {
		return "", time.Time{}, nil, errInvalidAttachments
	}
	seen := make(map[string]bool, len(attachmentIDs))
	for _, a := range attachmentIDs {
		if !isUUID(a) || seen[a] {
			return "", time.Time{}, nil, errInvalidAttachments
		}
		seen[a] = true
	}

	tx, err := db.DB.Begin()
	if err != nil {
		return "", time.Time{}, nil, err
	}
	defer tx.Rollback()

	if err = tx.QueryRow(
		`INSERT INTO messages (room_id, sender_id, content)
		 VALUES ($1, $2, $3)
		 RETURNING id, sent_at`,
		c.RoomID, c.UserID, content,
	).Scan(&id, &sentAt); err != nil {
		return "", time.Time{}, nil, err
	}

	if len(attachmentIDs) > 0 {
		rows, err := tx.Query(
			`UPDATE attachments SET message_id = $1, position = array_position($2::uuid[], id)
			 WHERE id = ANY($2::uuid[]) AND room_id = $3 AND uploader_id = $4 AND message_id IS NULL
			 RETURNING id, content_type, width, height, size_bytes`,
			id, pq.Array(attachmentIDs), c.RoomID, c.UserID,
		)
		if err != nil {
			return "", time.Time{}, nil, err
		}
		byID := make(map[string]attachmentResponse, len(attachmentIDs))
		for rows.Next() {
			var a attachmentResponse
			if err := rows.Scan(&a.ID, &a.ContentType, &a.Width, &a.Height, &a.Size); err != nil {
				rows.Close()
				return "", time.Time{}, nil, err
			}
			byID[a.ID] = a
		}
		rows.Close()
		if len(byID) != len(attachmentIDs) {
			return "", time.Time{}, nil, errInvalidAttachments
		}
		// keep the order the sender picked them in
		for _, attachmentID := range attachmentIDs {
			attachments = append(attachments, byID[attachmentID])
		}
	}

	if err = tx.Commit(); err != nil {
		return "", time.Time{}, nil, err
	}
	return id, sentAt, attachments, nil
}

func (c *Client) handleTyping() {
	WSHub.Broadcast(c.RoomID, c, mustJSON(WSMessage{Type: "typing", UserID: c.UserID, Pseudo: c.Pseudo}))
}

func (c *Client) handleRead(msg WSMessage) {
	if !isUUID(msg.ID) {
		return
	}
	c.markRead(msg.ID)
}

// markRead advances the member's read marker to the given message (never
// backwards) and tells the room so "seen by" indicators update.
func (c *Client) markRead(messageID string) {
	var readAt time.Time
	err := db.DB.QueryRow(
		`UPDATE room_members rm SET last_read_at = m.sent_at
		 FROM messages m
		 WHERE m.id = $3 AND m.room_id = $1
		   AND rm.room_id = $1 AND rm.user_id = $2
		   AND rm.last_read_at < m.sent_at
		 RETURNING rm.last_read_at`,
		c.RoomID, c.UserID, messageID,
	).Scan(&readAt)
	if err == sql.ErrNoRows {
		return
	}
	if err != nil {
		log.Printf("ws: failed to mark read: %v", err)
		return
	}

	WSHub.BroadcastToAll(c.RoomID, mustJSON(WSMessage{Type: "read", UserID: c.UserID, ReadAt: &readAt}))
}

func (c *Client) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		c.Conn.Close()
	}()

	for {
		select {
		case <-c.done:
			c.Conn.SetWriteDeadline(time.Now().Add(writeWait))
			c.Conn.WriteMessage(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
			return
		case msg := <-c.Send:
			c.Conn.SetWriteDeadline(time.Now().Add(writeWait))
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
	roomID, ok := uuidParam(c, "room_id", "room not found or access denied")
	if !ok {
		return
	}

	var pseudo string
	err := db.DB.QueryRow(
		`SELECT u.pseudo FROM room_members rm
		 JOIN users u ON u.id = rm.user_id
		 WHERE rm.room_id = $1 AND rm.user_id = $2`,
		roomID, userID,
	).Scan(&pseudo)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "room not found or access denied"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch room"})
		return
	}

	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Printf("ws: upgrade failed: %v", err)
		return
	}

	client := &Client{
		UserID: userID,
		Pseudo: pseudo,
		RoomID: roomID,
		Conn:   conn,
		Send:   make(chan []byte, 256),
		done:   make(chan struct{}),
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
