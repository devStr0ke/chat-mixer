package handlers

import (
	"database/sql"
	"encoding/json"
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

func ReactToMessage(c *gin.Context) {
	userID := c.GetString("userID")
	messageID := c.Param("message_id")

	var req reactionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "emoji is required"})
		return
	}

	// verify the message belongs to a room this user is in
	var roomID string
	err := db.DB.QueryRow(
		`SELECT m.room_id FROM messages m
		 JOIN rooms r ON r.id = m.room_id
		 WHERE m.id = $1 AND (r.user_a_id = $2 OR r.user_b_id = $2)`,
		messageID, userID,
	).Scan(&roomID)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "message not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to verify message"})
		return
	}

	// upsert: one reaction per user per message — patch emoji if already exists
	_, err = db.DB.Exec(
		`INSERT INTO message_reactions (message_id, user_id, emoji)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (message_id, user_id) DO UPDATE SET emoji = $3, created_at = NOW()`,
		messageID, userID, req.Emoji,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save reaction"})
		return
	}

	out, _ := json.Marshal(reactionEvent{
		Type:      "reaction",
		MessageID: messageID,
		UserID:    userID,
		Emoji:     req.Emoji,
		Action:    "add",
	})
	WSHub.Broadcast(roomID, userID, out)

	c.JSON(http.StatusOK, gin.H{"message": "reaction saved"})
}

func RemoveReaction(c *gin.Context) {
	userID := c.GetString("userID")
	messageID := c.Param("message_id")

	// verify the message belongs to a room this user is in
	var roomID string
	err := db.DB.QueryRow(
		`SELECT m.room_id FROM messages m
		 JOIN rooms r ON r.id = m.room_id
		 WHERE m.id = $1 AND (r.user_a_id = $2 OR r.user_b_id = $2)`,
		messageID, userID,
	).Scan(&roomID)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "message not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to verify message"})
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

	out, _ := json.Marshal(reactionEvent{
		Type:      "reaction",
		MessageID: messageID,
		UserID:    userID,
		Action:    "remove",
	})
	WSHub.Broadcast(roomID, userID, out)

	c.JSON(http.StatusOK, gin.H{"message": "reaction removed"})
}
