package handlers

import (
	"context"
	"database/sql"
	"log"
	"net/http"
	"time"

	"github.com/devstr0ke/chat-mixer/db"
	"github.com/devstr0ke/chat-mixer/storage"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// Custom stickers: images users cut out of their own photos in the app. Each
// user has a collection; a sticker someone sent is visible to the members of
// the rooms it was sent in. Removing one from your collection doesn't break
// messages that already used it: the file is deleted once none do
// (workers.PurgeRemovedStickers).

const (
	maxStickerBytes    = 1 << 20
	maxStickerSide     = 1024
	maxStickersPerUser = 200
)

// messageSticker is the custom sticker a message carries.
type messageSticker struct {
	ID     string `json:"id"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

// ListMyStickers — GET /stickers: your collection, newest first.
func ListMyStickers(c *gin.Context) {
	rows, err := db.DB.Query(
		`SELECT id, width, height FROM stickers
		 WHERE owner_id = $1 AND deleted_at IS NULL
		 ORDER BY created_at DESC`,
		c.GetString("userID"),
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch stickers"})
		return
	}
	defer rows.Close()
	stickers := make([]messageSticker, 0)
	for rows.Next() {
		var s messageSticker
		if err := rows.Scan(&s.ID, &s.Width, &s.Height); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch stickers"})
			return
		}
		stickers = append(stickers, s)
	}
	c.JSON(http.StatusOK, stickers)
}

// CreateSticker — POST /stickers (multipart field "file"): a PNG or WebP
// (transparent) of at most 1024px a side, made by the app's sticker maker.
func CreateSticker(c *gin.Context) {
	userID := c.GetString("userID")
	if !storage.Enabled() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "uploads are disabled"})
		return
	}
	data, contentType, cfg, ok := readImageUpload(c, maxStickerBytes)
	if !ok {
		return
	}
	if contentType != "image/png" && contentType != "image/webp" {
		c.JSON(http.StatusUnsupportedMediaType, gin.H{"error": "stickers must be PNG or WebP"})
		return
	}
	if cfg.Width > maxStickerSide || cfg.Height > maxStickerSide {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "stickers can be at most 1024px a side"})
		return
	}

	var count int
	if err := db.DB.QueryRow(`SELECT COUNT(*) FROM stickers WHERE owner_id = $1 AND deleted_at IS NULL`, userID).Scan(&count); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save sticker"})
		return
	}
	if count >= maxStickersPerUser {
		c.JSON(http.StatusConflict, gin.H{"error": "you have too many stickers, remove some first"})
		return
	}

	id := uuid.NewString()
	key := "stickers/" + userID + "/" + id + "." + allowedImageTypes[contentType]
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	if err := storage.Put(ctx, key, contentType, data); err != nil {
		log.Printf("storage: put %s failed: %v", key, err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to store sticker"})
		return
	}
	if _, err := db.DB.Exec(
		`INSERT INTO stickers (id, owner_id, object_key, content_type, width, height) VALUES ($1, $2, $3, $4, $5, $6)`,
		id, userID, key, contentType, cfg.Width, cfg.Height,
	); err != nil {
		storage.Delete(context.Background(), key)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save sticker"})
		return
	}
	c.JSON(http.StatusCreated, messageSticker{ID: id, Width: cfg.Width, Height: cfg.Height})
}

// DeleteSticker — DELETE /stickers/:sticker_id: removes it from your collection.
func DeleteSticker(c *gin.Context) {
	id, ok := uuidParam(c, "sticker_id", "sticker not found")
	if !ok {
		return
	}
	res, err := db.DB.Exec(
		`UPDATE stickers SET deleted_at = NOW() WHERE id = $1 AND owner_id = $2 AND deleted_at IS NULL`,
		id, c.GetString("userID"),
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to remove sticker"})
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "sticker not found"})
		return
	}
	c.Status(http.StatusNoContent)
}

// GetStickerImage — GET /stickers/:sticker_id/image: for its owner, and for
// members of the rooms it was sent in. Accepts the token cookie (<img>).
func GetStickerImage(c *gin.Context) {
	userID := c.GetString("userID")
	id, ok := uuidParam(c, "sticker_id", "sticker not found")
	if !ok {
		return
	}
	if !storage.Enabled() {
		c.JSON(http.StatusNotFound, gin.H{"error": "sticker not found"})
		return
	}
	var key string
	err := db.DB.QueryRow(
		`SELECT s.object_key FROM stickers s
		 WHERE s.id = $1 AND (
		     s.owner_id = $2
		     OR EXISTS (
		         SELECT 1 FROM messages m
		         JOIN room_members rm ON rm.room_id = m.room_id AND rm.user_id = $2
		         WHERE m.sticker_id = s.id
		     )
		 )`,
		id, userID,
	).Scan(&key)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "sticker not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch sticker"})
		return
	}
	obj, info, err := storage.Get(c.Request.Context(), key)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "sticker not found"})
		return
	}
	defer obj.Close()
	setImageHeaders(c)
	c.Header("Content-Type", info.ContentType)
	http.ServeContent(c.Writer, c.Request, "", time.Time{}, obj)
}

// loadOwnSticker returns a sticker from the sender's collection, for sending.
func loadOwnSticker(userID, stickerID string) (*messageSticker, error) {
	if !isUUID(stickerID) {
		return nil, sql.ErrNoRows
	}
	var s messageSticker
	err := db.DB.QueryRow(
		`SELECT id, width, height FROM stickers WHERE id = $1 AND owner_id = $2 AND deleted_at IS NULL`,
		stickerID, userID,
	).Scan(&s.ID, &s.Width, &s.Height)
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// scanSticker turns a message's nullable sticker columns into its sticker.
func scanSticker(id sql.NullString, width, height sql.NullInt64) *messageSticker {
	if !id.Valid {
		return nil
	}
	return &messageSticker{ID: id.String, Width: int(width.Int64), Height: int(height.Int64)}
}
