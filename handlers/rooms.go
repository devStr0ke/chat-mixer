package handlers

import (
	"database/sql"
	"net/http"
	"time"

	"github.com/devstr0ke/chat-mixer/db"
	"github.com/gin-gonic/gin"
)

type roomInfoResponse struct {
	ID        string    `json:"id"`
	CountryA  string    `json:"country_a"`
	CountryB  string    `json:"country_b"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	IsActive  bool      `json:"is_active"`
}

type messageResponse struct {
	ID       string    `json:"id"`
	SenderID string    `json:"sender_id"`
	Content  string    `json:"content"`
	SentAt   time.Time `json:"sent_at"`
}

func GetMyRooms(c *gin.Context) {
	userID := c.GetString("userID")

	rows, err := db.DB.Query(
		`SELECT r.id, ua.country, ub.country, r.created_at, r.expires_at, r.is_active
		 FROM rooms r
		 JOIN users ua ON ua.id = r.user_a_id
		 JOIN users ub ON ub.id = r.user_b_id
		 WHERE (r.user_a_id = $1 OR r.user_b_id = $1) AND r.is_active = true
		 ORDER BY r.created_at DESC`,
		userID,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch rooms"})
		return
	}
	defer rows.Close()

	rooms := make([]roomInfoResponse, 0)
	for rows.Next() {
		var r roomInfoResponse
		if err := rows.Scan(&r.ID, &r.CountryA, &r.CountryB, &r.CreatedAt, &r.ExpiresAt, &r.IsActive); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to scan room"})
			return
		}
		rooms = append(rooms, r)
	}

	c.JSON(http.StatusOK, rooms)
}

func GetRoom(c *gin.Context) {
	userID := c.GetString("userID")
	roomID := c.Param("room_id")

	var resp roomInfoResponse
	err := db.DB.QueryRow(
		`SELECT r.id, ua.country, ub.country, r.created_at, r.expires_at, r.is_active
		 FROM rooms r
		 JOIN users ua ON ua.id = r.user_a_id
		 JOIN users ub ON ub.id = r.user_b_id
		 WHERE r.id = $1 AND (r.user_a_id = $2 OR r.user_b_id = $2)`,
		roomID, userID,
	).Scan(&resp.ID, &resp.CountryA, &resp.CountryB, &resp.CreatedAt, &resp.ExpiresAt, &resp.IsActive)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "room not found or access denied"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch room"})
		return
	}

	c.JSON(http.StatusOK, resp)
}

func GetMessages(c *gin.Context) {
	userID := c.GetString("userID")
	roomID := c.Param("room_id")

	var exists bool
	err := db.DB.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM rooms WHERE id = $1 AND (user_a_id = $2 OR user_b_id = $2))`,
		roomID, userID,
	).Scan(&exists)
	if err != nil || !exists {
		c.JSON(http.StatusNotFound, gin.H{"error": "room not found or access denied"})
		return
	}

	rows, err := db.DB.Query(
		`SELECT id, sender_id, content, sent_at
		 FROM messages WHERE room_id = $1
		 ORDER BY sent_at ASC`,
		roomID,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch messages"})
		return
	}
	defer rows.Close()

	messages := make([]messageResponse, 0)
	for rows.Next() {
		var msg messageResponse
		if err := rows.Scan(&msg.ID, &msg.SenderID, &msg.Content, &msg.SentAt); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to scan message"})
			return
		}
		messages = append(messages, msg)
	}

	c.JSON(http.StatusOK, messages)
}
