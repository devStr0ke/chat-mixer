package handlers

import (
	"net/http"
	"sync"
	"time"

	"github.com/devstr0ke/chat-mixer/db"
	"github.com/devstr0ke/chat-mixer/models"
	"github.com/gin-gonic/gin"
)

// poolEntry represents a user waiting to be matched.
type poolEntry struct {
	UserID      string
	Country     string
	SameCountry bool
}

// matchPool is the in-memory waiting queue protected by a mutex.
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

// JoinPool adds the authenticated user to the matching pool.
// If a compatible partner is already waiting, both are dequeued and a room is created.
func JoinPool(c *gin.Context) {
	userID := c.GetString("userID")

	var req joinRequest
	// Body is optional — default same_country = false
	_ = c.ShouldBindJSON(&req)

	// Look up the user's country for matching
	var country string
	err := db.DB.QueryRow(`SELECT country FROM users WHERE id = $1`, userID).Scan(&country)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch user"})
		return
	}

	poolMu.Lock()
	defer poolMu.Unlock()

	// Check if user is already in the pool
	for _, entry := range pool {
		if entry.UserID == userID {
			c.JSON(http.StatusConflict, gin.H{"error": "already in the pool"})
			return
		}
	}

	// Try to find a match
	matchIdx := -1
	for i, entry := range pool {
		if entry.UserID == userID {
			continue
		}
		// If either side wants same_country, both must share it
		if req.SameCountry || entry.SameCountry {
			if entry.Country == country {
				matchIdx = i
				break
			}
		} else {
			matchIdx = i
			break
		}
	}

	// No match found — enqueue and wait
	if matchIdx == -1 {
		pool = append(pool, poolEntry{
			UserID:      userID,
			Country:     country,
			SameCountry: req.SameCountry,
		})
		c.JSON(http.StatusAccepted, gin.H{"message": "waiting for a match"})
		return
	}

	// Match found — dequeue partner, create room
	partner := pool[matchIdx]
	pool = append(pool[:matchIdx], pool[matchIdx+1:]...)

	now := time.Now().UTC()
	expiresAt := now.Add(24 * time.Hour)

	var room models.Room
	err = db.DB.QueryRow(
		`INSERT INTO rooms (user_a_id, user_b_id, created_at, expires_at)
		 VALUES ($1, $2, $3, $4)
		 RETURNING id, user_a_id, user_b_id, created_at, expires_at, is_active`,
		partner.UserID, userID, now, expiresAt,
	).Scan(&room.ID, &room.UserAID, &room.UserBID, &room.CreatedAt, &room.ExpiresAt, &room.IsActive)
	if err != nil {
		// Put the partner back if room creation fails
		pool = append(pool, partner)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create room"})
		return
	}

	c.JSON(http.StatusCreated, matchResponse{RoomID: room.ID})
}

// LeavePool removes the authenticated user from the waiting pool.
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
