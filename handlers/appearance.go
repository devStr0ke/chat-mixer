package handlers

import (
	"context"
	"database/sql"
	"log"
	"net/http"
	"regexp"
	"strings"

	"github.com/devstr0ke/chat-mixer/db"
	"github.com/devstr0ke/chat-mixer/storage"
	"github.com/gin-gonic/gin"
)

// roomTheme is a room's custom look. Null fields fall back to the app's defaults.
type roomTheme struct {
	BackgroundColor   *string `json:"background_color"`
	BackgroundImageID *string `json:"background_image_id"`
	BubbleOwnColor    *string `json:"bubble_own_color"`
	BubbleOtherColor  *string `json:"bubble_other_color"`
}

const roomThemeColumns = `bg_color, bg_image_id, bubble_own_color, bubble_other_color`

func (t *roomTheme) scanTargets() []any {
	return []any{&t.BackgroundColor, &t.BackgroundImageID, &t.BubbleOwnColor, &t.BubbleOtherColor}
}

var hexColor = regexp.MustCompile(`^#[0-9a-f]{6}$`)

// kept under the room's prefix so deleting the room removes it too
func backgroundKey(roomID, imageID string) string {
	return "rooms/" + roomID + "/background-" + imageID
}

func loadRoomTheme(roomID string) (roomTheme, error) {
	var t roomTheme
	err := db.DB.QueryRow(`SELECT `+roomThemeColumns+` FROM rooms WHERE id = $1`, roomID).Scan(t.scanTargets()...)
	return t, err
}

// requireOwner is requireMember plus a 403 for non-owners.
func requireOwner(c *gin.Context, action string) (roomID string, ok bool) {
	roomID, isOwner, ok := requireMember(c)
	if !ok {
		return "", false
	}
	if !isOwner {
		c.JSON(http.StatusForbidden, gin.H{"error": "only the owner can " + action})
		return "", false
	}
	return roomID, true
}

// themeChanged tells open clients to refetch the room and answers with the new theme.
func themeChanged(c *gin.Context, roomID string) {
	WSHub.BroadcastToAll(roomID, mustJSON(WSMessage{Type: "room_updated", RoomID: roomID}))

	theme, err := loadRoomTheme(roomID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load theme"})
		return
	}
	c.JSON(http.StatusOK, theme)
}

type updateThemeRequest struct {
	BackgroundColor  *string `json:"background_color"`
	BubbleOwnColor   *string `json:"bubble_own_color"`
	BubbleOtherColor *string `json:"bubble_other_color"`
}

// normalizeColor lowercases a #rrggbb color; nil or "" means "use the default".
func normalizeColor(v *string) (sql.NullString, bool) {
	if v == nil || strings.TrimSpace(*v) == "" {
		return sql.NullString{}, true
	}
	color := strings.ToLower(strings.TrimSpace(*v))
	if !hexColor.MatchString(color) {
		return sql.NullString{}, false
	}
	return sql.NullString{String: color, Valid: true}, true
}

// UpdateRoomTheme sets the room's colors. Owner only.
func UpdateRoomTheme(c *gin.Context) {
	roomID, ok := requireOwner(c, "change the room's appearance")
	if !ok {
		return
	}

	var req updateThemeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	background, ok1 := normalizeColor(req.BackgroundColor)
	bubbleOwn, ok2 := normalizeColor(req.BubbleOwnColor)
	bubbleOther, ok3 := normalizeColor(req.BubbleOtherColor)
	if !ok1 || !ok2 || !ok3 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "colors must look like #rrggbb"})
		return
	}

	if _, err := db.DB.Exec(
		`UPDATE rooms SET bg_color = $1, bubble_own_color = $2, bubble_other_color = $3 WHERE id = $4`,
		background, bubbleOwn, bubbleOther, roomID,
	); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update appearance"})
		return
	}
	themeChanged(c, roomID)
}

// UploadRoomBackground replaces the room's background image. Owner only.
func UploadRoomBackground(c *gin.Context) {
	roomID, ok := requireOwner(c, "change the room's appearance")
	if !ok {
		return
	}
	if !storage.Enabled() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "image uploads are not available"})
		return
	}

	data, contentType, cfg, ok := readImageUpload(c, maxAttachmentBytes)
	if !ok {
		return
	}
	if cfg.Width*cfg.Height > maxAttachmentPixels {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "image dimensions are too large"})
		return
	}

	var imageID string
	if err := db.DB.QueryRow(`SELECT uuid_generate_v4()`).Scan(&imageID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to store background"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), attachmentStoreTimeout)
	defer cancel()
	if err := storage.Put(ctx, backgroundKey(roomID, imageID), contentType, data); err != nil {
		log.Printf("storage: put background failed: %v", err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to store background"})
		return
	}

	previous, err := swapBackground(roomID, sql.NullString{String: imageID, Valid: true})
	if err != nil {
		storage.Delete(context.Background(), backgroundKey(roomID, imageID))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to store background"})
		return
	}
	deleteBackgroundObject(roomID, previous)
	themeChanged(c, roomID)
}

// DeleteRoomBackground removes the background image. Owner only.
func DeleteRoomBackground(c *gin.Context) {
	roomID, ok := requireOwner(c, "change the room's appearance")
	if !ok {
		return
	}

	previous, err := swapBackground(roomID, sql.NullString{})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to remove background"})
		return
	}
	deleteBackgroundObject(roomID, previous)
	themeChanged(c, roomID)
}

// swapBackground sets the room's background image id and returns the one it replaced.
func swapBackground(roomID string, imageID sql.NullString) (previous sql.NullString, err error) {
	err = db.DB.QueryRow(
		`WITH old AS (SELECT bg_image_id FROM rooms WHERE id = $2)
		 UPDATE rooms SET bg_image_id = $1 WHERE id = $2
		 RETURNING (SELECT bg_image_id FROM old)`,
		imageID, roomID,
	).Scan(&previous)
	return previous, err
}

func deleteBackgroundObject(roomID string, imageID sql.NullString) {
	if !imageID.Valid || !storage.Enabled() {
		return
	}
	go func() {
		if err := storage.Delete(context.Background(), backgroundKey(roomID, imageID.String)); err != nil {
			log.Printf("storage: failed to delete background %s: %v", imageID.String, err)
		}
	}()
}

// GetRoomBackground streams the room's current background image to its members.
// The image id is part of the URL so each version can be cached forever.
func GetRoomBackground(c *gin.Context) {
	roomID, _, ok := requireMember(c)
	if !ok {
		return
	}
	imageID, ok := uuidParam(c, "image_id", "background not found")
	if !ok {
		return
	}

	var current bool
	if err := db.DB.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM rooms WHERE id = $1 AND bg_image_id = $2)`, roomID, imageID,
	).Scan(&current); err != nil || !current || !storage.Enabled() {
		c.JSON(http.StatusNotFound, gin.H{"error": "background not found"})
		return
	}

	obj, info, err := storage.Get(c.Request.Context(), backgroundKey(roomID, imageID))
	if err != nil {
		log.Printf("storage: get background failed: %v", err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to fetch background"})
		return
	}
	defer obj.Close()

	if _, allowed := allowedImageTypes[info.ContentType]; !allowed {
		info.ContentType = "application/octet-stream"
	}
	setImageHeaders(c)
	c.DataFromReader(http.StatusOK, info.Size, info.ContentType, obj, nil)
}
