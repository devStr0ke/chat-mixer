package handlers

import (
	"net/http"
	"sync"
	"time"

	"github.com/devstr0ke/chat-mixer/db"
	"github.com/devstr0ke/chat-mixer/models"
	"github.com/gin-gonic/gin"
)

type poolEntry struct {
	UserID      string
	Country     string
	SameCountry bool
}

var (
	pool   []poolEntry
	poolMu sync.Mutex
)

type joinRequest struct {
	SameCountry bool `json:"same_country"`
}

type matchResponse struct {
	RoomID string `json:"room_id"`
}

func JoinPool(c *gin.Context) {
	userID := c.GetString("userID")

	var req joinRequest
	_ = c.ShouldBindJSON(&req)

	var country string
	if err := db.DB.QueryRow(`SELECT country FROM users WHERE id = $1`, userID).Scan(&country); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch user"})
		return
	}

	poolMu.Lock()
	defer poolMu.Unlock()

	for _, entry := range pool {
		if entry.UserID == userID {
			c.JSON(http.StatusConflict, gin.H{"error": "already in the pool"})
			return
		}
	}

	matchIdx := findMatch(userID, country, req.SameCountry)
	if matchIdx == -1 {
		pool = append(pool, poolEntry{
			UserID:      userID,
			Country:     country,
			SameCountry: req.SameCountry,
		})
		c.JSON(http.StatusAccepted, gin.H{"message": "waiting for a match"})
		return
	}

	partner := pool[matchIdx]
	pool = append(pool[:matchIdx], pool[matchIdx+1:]...)

	now := time.Now().UTC()
	var room models.Room
	err := db.DB.QueryRow(
		`INSERT INTO rooms (user_a_id, user_b_id, created_at, expires_at)
		 VALUES ($1, $2, $3, $4)
		 RETURNING id, user_a_id, user_b_id, created_at, expires_at, is_active`,
		partner.UserID, userID, now, now.Add(24*time.Hour),
	).Scan(&room.ID, &room.UserAID, &room.UserBID, &room.CreatedAt, &room.ExpiresAt, &room.IsActive)
	if err != nil {
		pool = append(pool, partner)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create room"})
		return
	}

	c.JSON(http.StatusCreated, matchResponse{RoomID: room.ID})
}

func LeavePool(c *gin.Context) {
	userID := c.GetString("userID")

	poolMu.Lock()
	defer poolMu.Unlock()

	for i, entry := range pool {
		if entry.UserID == userID {
			pool = append(pool[:i], pool[i+1:]...)
			c.JSON(http.StatusOK, gin.H{"message": "left the pool"})
			return
		}
	}

	c.JSON(http.StatusNotFound, gin.H{"error": "not in the pool"})
}

func findMatch(userID, country string, sameCountry bool) int {
	for i, entry := range pool {
		if entry.UserID == userID {
			continue
		}
		if sameCountry || entry.SameCountry {
			if entry.Country == country {
				return i
			}
		} else {
			return i
		}
	}
	return -1
}
