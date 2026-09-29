package handlers

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/devstr0ke/chat-mixer/db"
	"github.com/devstr0ke/chat-mixer/storage"
	"github.com/gin-gonic/gin"
	"github.com/lib/pq"
	_ "golang.org/x/image/webp"
)

const (
	maxAttachmentBytes     = 10 << 20
	maxAttachmentPixels    = 50_000_000
	maxAttachmentsPerMsg   = 10
	attachmentStoreTimeout = 30 * time.Second
)

// file extension per accepted (sniffed) content type
var allowedImageTypes = map[string]string{
	"image/jpeg": "jpg",
	"image/png":  "png",
	"image/gif":  "gif",
	"image/webp": "webp",
}

type attachmentResponse struct {
	ID          string `json:"id"`
	ContentType string `json:"content_type"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	Size        int64  `json:"size"`
}

// UploadAttachment stores an image in the bucket. It stays private to the
// uploader until a message references it.
func UploadAttachment(c *gin.Context) {
	userID := c.GetString("userID")
	roomID, _, ok := requireMember(c)
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
	ext := allowedImageTypes[contentType]

	var id string
	if err := db.DB.QueryRow(`SELECT uuid_generate_v4()`).Scan(&id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to store file"})
		return
	}
	key := fmt.Sprintf("rooms/%s/%s.%s", roomID, id, ext)

	ctx, cancel := context.WithTimeout(c.Request.Context(), attachmentStoreTimeout)
	defer cancel()
	if err := storage.Put(ctx, key, contentType, data); err != nil {
		log.Printf("storage: put %s failed: %v", key, err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to store file"})
		return
	}

	if _, err := db.DB.Exec(
		`INSERT INTO attachments (id, room_id, uploader_id, object_key, content_type, size_bytes, width, height)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		id, roomID, userID, key, contentType, len(data), cfg.Width, cfg.Height,
	); err != nil {
		storage.Delete(context.Background(), key)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to store file"})
		return
	}

	c.JSON(http.StatusCreated, attachmentResponse{
		ID:          id,
		ContentType: contentType,
		Width:       cfg.Width,
		Height:      cfg.Height,
		Size:        int64(len(data)),
	})
}

// readImageUpload reads the multipart "file" field and checks it's a supported
// image by its bytes (not the client's filename or Content-Type). It writes the
// error response itself when ok is false.
func readImageUpload(c *gin.Context, maxBytes int64) (data []byte, contentType string, cfg image.Config, ok bool) {
	tooLarge := fmt.Sprintf("file is too large (max %d MB)", maxBytes>>20)

	// leave room for the multipart envelope around the file
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBytes+1<<20)
	fh, err := c.FormFile("file")
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": tooLarge})
			return nil, "", cfg, false
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": "file is required"})
		return nil, "", cfg, false
	}
	if fh.Size > maxBytes {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": tooLarge})
		return nil, "", cfg, false
	}

	f, err := fh.Open()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read file"})
		return nil, "", cfg, false
	}
	data, err = io.ReadAll(io.LimitReader(f, maxBytes+1))
	f.Close()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read file"})
		return nil, "", cfg, false
	}

	contentType = http.DetectContentType(data)
	if _, allowed := allowedImageTypes[contentType]; !allowed {
		c.JSON(http.StatusUnsupportedMediaType, gin.H{"error": "only JPEG, PNG, GIF and WebP images are supported"})
		return nil, "", cfg, false
	}
	cfg, _, err = image.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 {
		c.JSON(http.StatusUnsupportedMediaType, gin.H{"error": "unreadable image"})
		return nil, "", cfg, false
	}
	return data, contentType, cfg, true
}

// setImageHeaders marks a served image as immutable (its id never changes
// content) and keeps browsers from sniffing or scripting it.
func setImageHeaders(c *gin.Context) {
	c.Header("Cache-Control", "private, max-age=31536000, immutable")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Content-Security-Policy", "default-src 'none'")
}

// GetAttachment streams an image to room members (or to its uploader before
// it's sent). Accepts the token cookie since <img> tags can't send headers.
func GetAttachment(c *gin.Context) {
	userID := c.GetString("userID")
	id, ok := uuidParam(c, "attachment_id", "attachment not found")
	if !ok {
		return
	}
	if !storage.Enabled() {
		c.JSON(http.StatusNotFound, gin.H{"error": "attachment not found"})
		return
	}

	var key, contentType string
	var size int64
	err := db.DB.QueryRow(
		`SELECT a.object_key, a.content_type, a.size_bytes
		 FROM attachments a
		 JOIN room_members rm ON rm.room_id = a.room_id AND rm.user_id = $2
		 WHERE a.id = $1 AND (a.message_id IS NOT NULL OR a.uploader_id = $2)`,
		id, userID,
	).Scan(&key, &contentType, &size)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "attachment not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch attachment"})
		return
	}

	obj, _, err := storage.Get(c.Request.Context(), key)
	if err != nil {
		log.Printf("storage: get %s failed: %v", key, err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to fetch attachment"})
		return
	}
	defer obj.Close()

	setImageHeaders(c)
	c.DataFromReader(http.StatusOK, size, contentType, obj, nil)
}

// attachmentsByMessage loads the attachments of the given messages, in the order they were sent.
func attachmentsByMessage(messageIDs []string) (map[string][]attachmentResponse, error) {
	result := make(map[string][]attachmentResponse)
	if len(messageIDs) == 0 {
		return result, nil
	}

	rows, err := db.DB.Query(
		`SELECT message_id, id, content_type, width, height, size_bytes
		 FROM attachments
		 WHERE message_id = ANY($1::uuid[])
		 ORDER BY position ASC, id ASC`,
		pq.Array(messageIDs),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var messageID string
		var a attachmentResponse
		if err := rows.Scan(&messageID, &a.ID, &a.ContentType, &a.Width, &a.Height, &a.Size); err != nil {
			return nil, err
		}
		result[messageID] = append(result[messageID], a)
	}
	return result, rows.Err()
}

// deleteRoomFiles removes a deleted room's objects in the background.
func deleteRoomFiles(roomID string) {
	if !storage.Enabled() {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if err := storage.DeletePrefix(ctx, "rooms/"+roomID+"/"); err != nil {
			log.Printf("storage: failed to delete files of room %s: %v", roomID, err)
		}
	}()
}
