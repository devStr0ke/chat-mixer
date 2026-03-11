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

type reactionResponse struct {
	UserID string `json:"user_id"`
	Emoji  string `json:"emoji"`
}

type messageResponse struct {
	ID        string             `json:"id"`
	SenderID  string             `json:"sender_id"`
	Content   string             `json:"content"`
	IsRead    bool               `json:"is_read"`
	SentAt    time.Time          `json:"sent_at"`
	Reactions []reactionResponse `json:"reactions"`
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
		`SELECT m.id, m.sender_id, m.content, m.is_read, m.sent_at,
		        r.user_id, r.emoji
		 FROM messages m
		 LEFT JOIN message_reactions r ON r.message_id = m.id
		 WHERE m.room_id = $1
		 ORDER BY m.sent_at ASC, r.created_at ASC`,
		roomID,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch messages"})
		return
	}
	defer rows.Close()

	index := make(map[string]int)
	messages := make([]messageResponse, 0)

	for rows.Next() {
		var (
			id, senderID, content string
			isRead                bool
			sentAt                time.Time
			reactionUserID        sql.NullString
			reactionEmoji         sql.NullString
		)
		if err := rows.Scan(&id, &senderID, &content, &isRead, &sentAt, &reactionUserID, &reactionEmoji); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to scan message"})
			return
		}

		i, seen := index[id]
		if !seen {
			messages = append(messages, messageResponse{
				ID:        id,
				SenderID:  senderID,
				Content:   content,
				IsRead:    isRead,
				SentAt:    sentAt,
				Reactions: make([]reactionResponse, 0),
			})
			i = len(messages) - 1
			index[id] = i
		}

		if reactionUserID.Valid && reactionEmoji.Valid {
			messages[i].Reactions = append(messages[i].Reactions, reactionResponse{
				UserID: reactionUserID.String,
				Emoji:  reactionEmoji.String,
			})
		}
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

	var userAID, userBID string
	err := db.DB.QueryRow(
		`SELECT user_a_id, user_b_id FROM rooms
		 WHERE id = $1 AND (user_a_id = $2 OR user_b_id = $2) AND is_active = true`,
		roomID, userID,
	).Scan(&userAID, &userBID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "room not found"})
		return
	}

	db.DB.Exec(`DELETE FROM messages WHERE room_id = $1`, roomID)
	db.DB.Exec(`UPDATE rooms SET is_active = false WHERE id = $1`, roomID)

	WSHub.CloseRoom(roomID)
	WSHub.NotifyRoomClosed(roomID, userAID, userBID)

	c.JSON(http.StatusOK, gin.H{"message": "room deleted"})
}
