package handlers

import (
	"context"
	"database/sql"
	"log"
	"net/http"
	"strconv"
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
//
// Drawings made in the chat are stored the same way (kind "drawing"), but
// they aren't part of the collection, and one that's never sent is deleted
// after a day.

const (
	maxStickerBytes    = 1 << 20
	maxStickerSide     = 1024
	maxDrawingSide     = 2048 // drawings can span a wide screen
	maxStickersPerUser = 200
	maxDrawingsPerHour = 120
	stickerKindSticker = "sticker"
	stickerKindDrawing = "drawing"
)

// messageSticker is the custom sticker (or drawing) a message carries.
type messageSticker struct {
	ID     string `json:"id"`
	Width  int    `json:"width"`          // a drawing: the size it was drawn at on screen
	Height int    `json:"height"`         // (otherwise the image's own size)
	Kind   string `json:"kind,omitempty"` // "drawing", or empty for a sticker
}

// publicKind is how a sticker's kind appears in responses.
func publicKind(kind string) string {
	if kind == stickerKindDrawing {
		return stickerKindDrawing
	}
	return ""
}

// ListMyStickers — GET /stickers: your collection, newest first.
func ListMyStickers(c *gin.Context) {
	rows, err := db.DB.Query(
		`SELECT id, width, height FROM stickers
		 WHERE owner_id = $1 AND deleted_at IS NULL AND kind = 'sticker'
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

// CreateSticker — POST /stickers?kind=sticker|drawing (multipart field
// "file"): a PNG or WebP (transparent) of at most 1024px a side (2048 for a
// drawing), made by the app's sticker maker or drawn in the chat. A drawing
// also says the size it was drawn at on screen (display_width/_height), which
// is the size it's shown at.
func CreateSticker(c *gin.Context) {
	userID := c.GetString("userID")
	kind := stickerKindSticker
	if c.Query("kind") == stickerKindDrawing {
		kind = stickerKindDrawing
	}
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
	maxSide := maxStickerSide
	if kind == stickerKindDrawing {
		maxSide = maxDrawingSide
	}
	if cfg.Width > maxSide || cfg.Height > maxSide {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "the image is too large"})
		return
	}
	width, height := cfg.Width, cfg.Height
	if kind == stickerKindDrawing {
		w, errW := strconv.Atoi(c.PostForm("display_width"))
		h, errH := strconv.Atoi(c.PostForm("display_height"))
		if errW == nil && errH == nil && w > 0 && h > 0 && w <= 8000 && h <= 8000 {
			width, height = w, h
		}
	}

	var count int
	limit, limitQuery := maxStickersPerUser,
		`SELECT COUNT(*) FROM stickers WHERE owner_id = $1 AND deleted_at IS NULL AND kind = 'sticker'`
	if kind == stickerKindDrawing {
		limit, limitQuery = maxDrawingsPerHour,
			`SELECT COUNT(*) FROM stickers WHERE owner_id = $1 AND kind = 'drawing' AND created_at > NOW() - INTERVAL '1 hour'`
	}
	if err := db.DB.QueryRow(limitQuery, userID).Scan(&count); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save sticker"})
		return
	}
	if count >= limit {
		msg := "you have too many stickers, remove some first"
		if kind == stickerKindDrawing {
			msg = "too many drawings, try again later"
		}
		c.JSON(http.StatusConflict, gin.H{"error": msg})
		return
	}

	id := uuid.NewString()
	key := "stickers/" + userID + "/" + id + "." + allowedImageTypes[contentType]
	if kind == stickerKindDrawing {
		key = "drawings/" + userID + "/" + id + "." + allowedImageTypes[contentType]
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	if err := storage.Put(ctx, key, contentType, data); err != nil {
		log.Printf("storage: put %s failed: %v", key, err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to store sticker"})
		return
	}
	if _, err := db.DB.Exec(
		`INSERT INTO stickers (id, owner_id, object_key, content_type, width, height, kind) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		id, userID, key, contentType, width, height, kind,
	); err != nil {
		storage.Delete(context.Background(), key)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save sticker"})
		return
	}
	c.JSON(http.StatusCreated, messageSticker{ID: id, Width: width, Height: height, Kind: publicKind(kind)})
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
		`SELECT id, width, height, kind FROM stickers WHERE id = $1 AND owner_id = $2 AND deleted_at IS NULL`,
		stickerID, userID,
	).Scan(&s.ID, &s.Width, &s.Height, &s.Kind)
	if err != nil {
		return nil, err
	}
	s.Kind = publicKind(s.Kind)
	return &s, nil
}

// scanSticker turns a message's nullable sticker columns into its sticker.
func scanSticker(id sql.NullString, width, height sql.NullInt64, kind sql.NullString) *messageSticker {
	if !id.Valid {
		return nil
	}
	return &messageSticker{ID: id.String, Width: int(width.Int64), Height: int(height.Int64), Kind: publicKind(kind.String)}
}
