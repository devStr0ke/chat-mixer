package handlers

import (
	"database/sql"
	"net/http"
	"time"

	"github.com/devstr0ke/chat-mixer/db"
	"github.com/gin-gonic/gin"
)

type roomInfoResponse struct {
	ID            string    `json:"id"`
	CountryA      string    `json:"country_a"`
	CountryB      string    `json:"country_b"`
	UserARoomName string    `json:"user_a_room_name"`
	UserBRoomName string    `json:"user_b_room_name"`
	CreatedAt     time.Time `json:"created_at"`
	ExpiresAt     time.Time `json:"expires_at"`
	IsActive      bool      `json:"is_active"`
}

type messageResponse struct {
	ID       string    `json:"id"`
	SenderID string    `json:"sender_id"`
	Content  string    `json:"content"`
	IsRead   bool      `json:"is_read"`
	SentAt   time.Time `json:"sent_at"`
}

func GetMyRooms(c *gin.Context) {
	userID := c.GetString("userID")

	rows, err := db.DB.Query(
		`SELECT r.id, ua.country, ub.country, r.user_a_room_name, r.user_b_room_name,
		        r.created_at, r.expires_at, r.is_active
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
		if err := rows.Scan(&r.ID, &r.CountryA, &r.CountryB, &r.UserARoomName, &r.UserBRoomName,
			&r.CreatedAt, &r.ExpiresAt, &r.IsActive); err != nil {
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
		`SELECT r.id, ua.country, ub.country, r.user_a_room_name, r.user_b_room_name,
		        r.created_at, r.expires_at, r.is_active
		 FROM rooms r
		 JOIN users ua ON ua.id = r.user_a_id
		 JOIN users ub ON ub.id = r.user_b_id
		 WHERE r.id = $1 AND (r.user_a_id = $2 OR r.user_b_id = $2)`,
		roomID, userID,
	).Scan(&resp.ID, &resp.CountryA, &resp.CountryB, &resp.UserARoomName, &resp.UserBRoomName,
		&resp.CreatedAt, &resp.ExpiresAt, &resp.IsActive)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "room not found"})
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
		c.JSON(http.StatusNotFound, gin.H{"error": "room not found"})
		return
	}

	rows, err := db.DB.Query(
		`SELECT id, sender_id, content, is_read, sent_at
		 FROM messages WHERE room_id = $1 ORDER BY sent_at ASC`,
		roomID,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch messages"})
		return
	}
	defer rows.Close()

	messages := make([]messageResponse, 0)
	for rows.Next() {
		var m messageResponse
		if err := rows.Scan(&m.ID, &m.SenderID, &m.Content, &m.IsRead, &m.SentAt); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to scan message"})
			return
		}
		messages = append(messages, m)
	}

	c.JSON(http.StatusOK, messages)
}

type renameRequest struct {
	Name string `json:"name" binding:"required,min=1,max=64"`
}

func RenameRoom(c *gin.Context) {
	userID := c.GetString("userID")
	roomID := c.Param("room_id")

	var req renameRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required (1-64 chars)"})
		return
	}

	var userAID string
	err := db.DB.QueryRow(
		`SELECT user_a_id FROM rooms
		 WHERE id = $1 AND (user_a_id = $2 OR user_b_id = $2) AND is_active = true`,
		roomID, userID,
	).Scan(&userAID)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "room not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch room"})
		return
	}

	col := "user_b_room_name"
	if userID == userAID {
		col = "user_a_room_name"
	}

	if _, err := db.DB.Exec(`UPDATE rooms SET `+col+` = $1 WHERE id = $2`, req.Name, roomID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to rename room"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "room renamed"})
}

func DeleteRoom(c *gin.Context) {
	userID := c.GetString("userID")
	roomID := c.Param("room_id")

	result, err := db.DB.Exec(
		`UPDATE rooms SET is_active = false
		 WHERE id = $1 AND (user_a_id = $2 OR user_b_id = $2) AND is_active = true`,
		roomID, userID,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete room"})
		return
	}

	rows, _ := result.RowsAffected()
	if rows == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "room not found"})
		return
	}

	db.DB.Exec(`DELETE FROM messages WHERE room_id = $1`, roomID)
	WSHub.CloseRoom(roomID)

	c.JSON(http.StatusOK, gin.H{"message": "room deleted"})
}
