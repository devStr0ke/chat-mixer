package handlers

import (
	"database/sql"
	"net/http"
	"time"

	"github.com/devstr0ke/chat-mixer/db"
	"github.com/devstr0ke/chat-mixer/workers"
	"github.com/gin-gonic/gin"
	"github.com/lib/pq"
)

// View-once photos and videos: every member except the sender can open one
// once. Opening starts a short viewing window (workers.ViewWindowSQL) during
// which the file is served; outside it, and to the sender, it's refused. The
// file is deleted once everyone has opened it, or after 14 days.

// messageOpen is someone opening a view-once message.
type messageOpen struct {
	UserID   string    `json:"user_id"`
	OpenedAt time.Time `json:"opened_at"`
}

// OpenViewOnce — POST /messages/:message_id/open: opens a view-once photo or
// video, once. Returns the attachment and how many seconds it stays readable.
func OpenViewOnce(c *gin.Context) {
	userID := c.GetString("userID")
	messageID, roomID, ok := messageRoomForMember(c)
	if !ok {
		return
	}

	var (
		senderID string
		viewOnce bool
		purged   bool
		window   int
		a        attachmentResponse
	)
	err := db.DB.QueryRow(
		`SELECT m.sender_id, m.view_once, a.id, a.content_type, a.width, a.height, a.size_bytes,
		        COALESCE(a.duration_ms, 0), a.overlay_key IS NOT NULL, a.purged_at IS NOT NULL, `+workers.ViewWindowSQL+`
		 FROM messages m
		 JOIN attachments a ON a.message_id = m.id
		 WHERE m.id = $1`,
		messageID,
	).Scan(&senderID, &viewOnce, &a.ID, &a.ContentType, &a.Width, &a.Height, &a.Size, &a.DurationMs, &a.HasOverlay, &purged, &window)
	if err == sql.ErrNoRows || (err == nil && !viewOnce) {
		c.JSON(http.StatusNotFound, gin.H{"error": "not a view-once message"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to open"})
		return
	}
	if senderID == userID {
		c.JSON(http.StatusForbidden, gin.H{"error": "you can't open your own view-once message"})
		return
	}
	if purged {
		c.JSON(http.StatusGone, gin.H{"error": "this has expired"})
		return
	}

	var openedAt time.Time
	err = db.DB.QueryRow(
		`INSERT INTO message_opens (message_id, user_id) VALUES ($1, $2)
		 ON CONFLICT DO NOTHING
		 RETURNING opened_at`,
		messageID, userID,
	).Scan(&openedAt)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusGone, gin.H{"error": "you've already opened this"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to open"})
		return
	}

	WSHub.BroadcastToAll(roomID, mustJSON(gin.H{
		"type":      "message_opened",
		"id":        messageID,
		"user_id":   userID,
		"opened_at": openedAt,
	}))

	// the last one to open it: the file goes as soon as their window closes
	var everyone bool
	if err := db.DB.QueryRow(
		`SELECT NOT EXISTS (
		     SELECT 1 FROM room_members rm
		     WHERE rm.room_id = $1 AND rm.user_id <> $2
		       AND NOT EXISTS (SELECT 1 FROM message_opens o WHERE o.message_id = $3 AND o.user_id = rm.user_id)
		 )`,
		roomID, senderID, messageID,
	).Scan(&everyone); err == nil && everyone {
		time.AfterFunc(time.Duration(window+5)*time.Second, func() { workers.PurgeViewOnce(db.DB) })
	}

	c.JSON(http.StatusOK, gin.H{"attachment": a, "expires_in": window})
}

// opensByMessage loads who opened the given view-once messages, in order.
func opensByMessage(messageIDs []string) (map[string][]messageOpen, error) {
	result := make(map[string][]messageOpen)
	if len(messageIDs) == 0 {
		return result, nil
	}
	rows, err := db.DB.Query(
		`SELECT message_id, user_id, opened_at FROM message_opens
		 WHERE message_id = ANY($1::uuid[])
		 ORDER BY opened_at ASC`,
		pq.Array(messageIDs),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var messageID string
		var o messageOpen
		if err := rows.Scan(&messageID, &o.UserID, &o.OpenedAt); err != nil {
			return nil, err
		}
		result[messageID] = append(result[messageID], o)
	}
	return result, rows.Err()
}
