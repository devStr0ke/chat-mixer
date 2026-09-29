package handlers

import (
	"database/sql"
	"net/http"

	"github.com/devstr0ke/chat-mixer/db"
	"github.com/gin-gonic/gin"
)

type reactionRequest struct {
	Emoji string `json:"emoji" binding:"required,max=32"`
}

type reactionEvent struct {
	Type      string `json:"type"`
	MessageID string `json:"message_id"`
	UserID    string `json:"user_id"`
	Emoji     string `json:"emoji,omitempty"`
	Action    string `json:"action"` // "add" or "remove"
}

// messageRoomForMember returns the room of a message the user can see,
// answering 404 when the message doesn't exist or isn't in one of their rooms.
func messageRoomForMember(c *gin.Context) (messageID, roomID string, ok bool) {
	messageID, ok = uuidParam(c, "message_id", "message not found")
	if !ok {
		return "", "", false
	}

	err := db.DB.QueryRow(
		`SELECT m.room_id FROM messages m
		 JOIN room_members rm ON rm.room_id = m.room_id AND rm.user_id = $2
		 WHERE m.id = $1`,
		messageID, c.GetString("userID"),
	).Scan(&roomID)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "message not found"})
		return "", "", false
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to verify message"})
		return "", "", false
	}
	return messageID, roomID, true
}

func ReactToMessage(c *gin.Context) {
	userID := c.GetString("userID")

	var req reactionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "emoji is required"})
		return
	}

	messageID, roomID, ok := messageRoomForMember(c)
	if !ok {
		return
	}

	// upsert: one reaction per user per message — patch emoji if already exists
	_, err := db.DB.Exec(
		`INSERT INTO message_reactions (message_id, user_id, emoji)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (message_id, user_id) DO UPDATE SET emoji = $3, created_at = NOW()`,
		messageID, userID, req.Emoji,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save reaction"})
		return
	}

	// everyone in the room, including the reactor's other tabs
	WSHub.BroadcastToAll(roomID, mustJSON(reactionEvent{
		Type:      "reaction",
		MessageID: messageID,
		UserID:    userID,
		Emoji:     req.Emoji,
		Action:    "add",
	}))

	c.JSON(http.StatusOK, gin.H{"message": "reaction saved"})
}

func RemoveReaction(c *gin.Context) {
	userID := c.GetString("userID")

	messageID, roomID, ok := messageRoomForMember(c)
	if !ok {
		return
	}

	result, err := db.DB.Exec(
		`DELETE FROM message_reactions WHERE message_id = $1 AND user_id = $2`,
		messageID, userID,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to remove reaction"})
		return
	}

	rows, _ := result.RowsAffected()
	if rows == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "no reaction to remove"})
		return
	}

	WSHub.BroadcastToAll(roomID, mustJSON(reactionEvent{
		Type:      "reaction",
		MessageID: messageID,
		UserID:    userID,
		Action:    "remove",
	}))

	c.JSON(http.StatusOK, gin.H{"message": "reaction removed"})
}
