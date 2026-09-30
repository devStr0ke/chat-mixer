package handlers

import (
	"database/sql"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/devstr0ke/chat-mixer/db"
	"github.com/gin-gonic/gin"
)

const (
	maxMessageRunes    = 4000
	maxEditsPerMessage = 100
)

type editMessageRequest struct {
	Content string `json:"content"`
}

type editedMessageResponse struct {
	ID       string     `json:"id"`
	Content  string     `json:"content"`
	EditedAt *time.Time `json:"edited_at"`
}

// EditMessage changes the text of one of the caller's own messages. The
// previous text is kept as a version any room member can look up.
func EditMessage(c *gin.Context) {
	userID := c.GetString("userID")
	messageID, ok := uuidParam(c, "message_id", "message not found")
	if !ok {
		return
	}

	var req editMessageRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	content := strings.TrimSpace(req.Content)
	if utf8.RuneCountInString(content) > maxMessageRunes {
		c.JSON(http.StatusBadRequest, gin.H{"error": "message is too long"})
		return
	}

	tx, err := db.DB.Begin()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to edit message"})
		return
	}
	defer tx.Rollback()

	var (
		roomID, senderID, current string
		editedAt                  *time.Time
		hasMedia                  bool
		edits                     int
	)
	// the row lock keeps concurrent edits of the same message in order
	err = tx.QueryRow(
		`SELECT m.room_id, m.sender_id, m.content, m.edited_at,
		        m.gif_id IS NOT NULL OR EXISTS(SELECT 1 FROM attachments a WHERE a.message_id = m.id),
		        (SELECT COUNT(*) FROM message_edits e WHERE e.message_id = m.id)
		 FROM messages m
		 JOIN room_members rm ON rm.room_id = m.room_id AND rm.user_id = $2
		 WHERE m.id = $1
		 FOR UPDATE OF m`,
		messageID, userID,
	).Scan(&roomID, &senderID, &current, &editedAt, &hasMedia, &edits)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "message not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to edit message"})
		return
	}
	if senderID != userID {
		c.JSON(http.StatusForbidden, gin.H{"error": "you can only edit your own messages"})
		return
	}
	// text can only be emptied when an image or GIF remains
	if content == "" && !hasMedia {
		c.JSON(http.StatusBadRequest, gin.H{"error": "message can't be empty"})
		return
	}
	if content == current {
		c.JSON(http.StatusOK, editedMessageResponse{ID: messageID, Content: current, EditedAt: editedAt})
		return
	}
	if edits >= maxEditsPerMessage {
		c.JSON(http.StatusConflict, gin.H{"error": "this message has been edited too many times"})
		return
	}

	if _, err := tx.Exec(
		`INSERT INTO message_edits (message_id, content, written_at)
		 SELECT id, content, COALESCE(edited_at, sent_at) FROM messages WHERE id = $1`,
		messageID,
	); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to edit message"})
		return
	}
	var newEditedAt time.Time
	if err := tx.QueryRow(
		`UPDATE messages SET content = $1, edited_at = NOW() WHERE id = $2 RETURNING edited_at`,
		content, messageID,
	).Scan(&newEditedAt); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to edit message"})
		return
	}
	if err := tx.Commit(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to edit message"})
		return
	}

	// content is sent even when empty (caption removed), hence not WSMessage
	event := mustJSON(gin.H{
		"type":      "message_edited",
		"id":        messageID,
		"room_id":   roomID,
		"content":   content,
		"edited_at": newEditedAt,
	})
	WSHub.BroadcastToAll(roomID, event)
	// lets room lists refresh their last-message preview
	if memberIDs, err := roomMemberIDs(db.DB, roomID); err == nil {
		WSHub.NotifyUsers(event, memberIDs...)
	}

	c.JSON(http.StatusOK, editedMessageResponse{ID: messageID, Content: content, EditedAt: &newEditedAt})
}

type messageVersion struct {
	Content string    `json:"content"`
	At      time.Time `json:"at"`
}

// GetMessageEdits returns every version of a message, oldest first; the last
// one is the current text. Available to all members of the room.
func GetMessageEdits(c *gin.Context) {
	messageID, _, ok := messageRoomForMember(c)
	if !ok {
		return
	}

	rows, err := db.DB.Query(
		`SELECT content, written_at FROM (
		     SELECT content, written_at, created_at AS ord FROM message_edits WHERE message_id = $1
		     UNION ALL
		     SELECT content, COALESCE(edited_at, sent_at), 'infinity'::timestamp FROM messages WHERE id = $1
		 ) v
		 ORDER BY ord ASC, written_at ASC`,
		messageID,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch edit history"})
		return
	}
	defer rows.Close()

	versions := make([]messageVersion, 0)
	for rows.Next() {
		var v messageVersion
		if err := rows.Scan(&v.Content, &v.At); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch edit history"})
			return
		}
		versions = append(versions, v)
	}

	c.JSON(http.StatusOK, gin.H{"versions": versions})
}
