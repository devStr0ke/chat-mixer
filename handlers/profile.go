package handlers

import (
	"context"
	"database/sql"
	"log"
	"net/http"
	"regexp"
	"strings"

	"github.com/devstr0ke/chat-mixer/db"
	"github.com/devstr0ke/chat-mixer/models"
	"github.com/devstr0ke/chat-mixer/storage"
	"github.com/gin-gonic/gin"
)

const (
	maxAvatarBytes     = 2 << 20
	maxAvatarDimension = 4096
)

var countryCode = regexp.MustCompile(`^[A-Z]{2}$`)

func avatarKey(avatarID string) string {
	return "avatars/" + avatarID
}

func loadUser(userID string) (models.User, error) {
	var u models.User
	err := db.DB.QueryRow(
		`SELECT id, pseudo, email, country, country2, avatar_id, created_at FROM users WHERE id = $1`,
		userID,
	).Scan(&u.ID, &u.Pseudo, &u.Email, &u.Country, &u.Country2, &u.AvatarID, &u.CreatedAt)
	return u, err
}

// respondWithUser answers with the caller's up-to-date profile.
func respondWithUser(c *gin.Context, userID string) {
	user, err := loadUser(userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load profile"})
		return
	}
	c.JSON(http.StatusOK, user)
}

// notifyProfileChange makes open rooms the user belongs to refetch their
// members, so new avatars and flags show up without a reload.
func notifyProfileChange(userID string) {
	roomIDs, err := userRoomIDs(userID)
	if err != nil {
		log.Printf("profile: failed to list rooms of %s: %v", userID, err)
		return
	}
	for _, roomID := range roomIDs {
		WSHub.BroadcastToAll(roomID, mustJSON(WSMessage{Type: "room_updated", RoomID: roomID}))
	}
}

func userRoomIDs(userID string) ([]string, error) {
	rows, err := db.DB.Query(`SELECT room_id FROM room_members WHERE user_id = $1`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func GetMe(c *gin.Context) {
	respondWithUser(c, c.GetString("userID"))
}

type updateProfileRequest struct {
	Country  string  `json:"country"`
	Country2 *string `json:"country2"`
}

// UpdateMe changes the caller's countries. A null or empty country2 clears it.
func UpdateMe(c *gin.Context) {
	userID := c.GetString("userID")

	var req updateProfileRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	country := strings.ToUpper(strings.TrimSpace(req.Country))
	if !countryCode.MatchString(country) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "country must be a 2-letter code"})
		return
	}
	country2 := ""
	if req.Country2 != nil {
		country2 = strings.ToUpper(strings.TrimSpace(*req.Country2))
	}
	if country2 != "" && !countryCode.MatchString(country2) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "second country must be a 2-letter code"})
		return
	}
	if country2 == country {
		c.JSON(http.StatusBadRequest, gin.H{"error": "second country must be different from the first"})
		return
	}

	if _, err := db.DB.Exec(
		`UPDATE users SET country = $1, country2 = NULLIF($2, '') WHERE id = $3`,
		country, country2, userID,
	); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update profile"})
		return
	}

	notifyProfileChange(userID)
	respondWithUser(c, userID)
}

// UploadAvatar replaces the caller's profile picture.
func UploadAvatar(c *gin.Context) {
	userID := c.GetString("userID")
	if !storage.Enabled() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "image uploads are not available"})
		return
	}

	data, contentType, cfg, ok := readImageUpload(c, maxAvatarBytes)
	if !ok {
		return
	}
	if cfg.Width > maxAvatarDimension || cfg.Height > maxAvatarDimension {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "image dimensions are too large"})
		return
	}

	// a fresh id per upload keeps avatar URLs cacheable forever
	var avatarID string
	if err := db.DB.QueryRow(`SELECT uuid_generate_v4()`).Scan(&avatarID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to store avatar"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), attachmentStoreTimeout)
	defer cancel()
	if err := storage.Put(ctx, avatarKey(avatarID), contentType, data); err != nil {
		log.Printf("storage: put avatar failed: %v", err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to store avatar"})
		return
	}

	previous, err := swapAvatar(userID, sql.NullString{String: avatarID, Valid: true})
	if err != nil {
		storage.Delete(context.Background(), avatarKey(avatarID))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to store avatar"})
		return
	}
	deleteAvatarObject(previous)

	notifyProfileChange(userID)
	respondWithUser(c, userID)
}

func DeleteAvatar(c *gin.Context) {
	userID := c.GetString("userID")

	previous, err := swapAvatar(userID, sql.NullString{})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to remove avatar"})
		return
	}
	deleteAvatarObject(previous)

	notifyProfileChange(userID)
	respondWithUser(c, userID)
}

// swapAvatar sets the user's avatar id and returns the one it replaced.
func swapAvatar(userID string, avatarID sql.NullString) (previous sql.NullString, err error) {
	err = db.DB.QueryRow(
		`WITH old AS (SELECT avatar_id FROM users WHERE id = $2)
		 UPDATE users SET avatar_id = $1 WHERE id = $2
		 RETURNING (SELECT avatar_id FROM old)`,
		avatarID, userID,
	).Scan(&previous)
	return previous, err
}

func deleteAvatarObject(avatarID sql.NullString) {
	if !avatarID.Valid || !storage.Enabled() {
		return
	}
	go func() {
		if err := storage.Delete(context.Background(), avatarKey(avatarID.String)); err != nil {
			log.Printf("storage: failed to delete avatar %s: %v", avatarID.String, err)
		}
	}()
}

// GetAvatar streams a current profile picture to any signed-in user. Accepts
// the token cookie since <img> tags can't send headers.
func GetAvatar(c *gin.Context) {
	avatarID, ok := uuidParam(c, "avatar_id", "avatar not found")
	if !ok {
		return
	}
	if !storage.Enabled() {
		c.JSON(http.StatusNotFound, gin.H{"error": "avatar not found"})
		return
	}

	var inUse bool
	if err := db.DB.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM users WHERE avatar_id = $1)`, avatarID,
	).Scan(&inUse); err != nil || !inUse {
		c.JSON(http.StatusNotFound, gin.H{"error": "avatar not found"})
		return
	}

	obj, info, err := storage.Get(c.Request.Context(), avatarKey(avatarID))
	if err != nil {
		log.Printf("storage: get avatar %s failed: %v", avatarID, err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to fetch avatar"})
		return
	}
	defer obj.Close()

	if _, allowed := allowedImageTypes[info.ContentType]; !allowed {
		info.ContentType = "application/octet-stream"
	}
	setImageHeaders(c)
	c.DataFromReader(http.StatusOK, info.Size, info.ContentType, obj, nil)
}
