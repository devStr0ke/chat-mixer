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
	"github.com/devstr0ke/chat-mixer/workers"
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
	DurationMs  int    `json:"duration_ms,omitempty"` // videos
	HasPoster   bool   `json:"has_poster,omitempty"`  // videos with a poster frame
	purged      bool   // view-once file already deleted
}

// UploadAttachment stores an image or video in the bucket. It stays private
// to the uploader until a message references it.
func UploadAttachment(c *gin.Context) {
	userID := c.GetString("userID")
	roomID, _, ok := requireMember(c)
	if !ok {
		return
	}
	if !storage.Enabled() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "uploads are not available"})
		return
	}

	// room for the largest file type (videos) plus the poster and form fields
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxVideoBytes+maxPosterBytes+1<<20)
	fh, err := c.FormFile("file")
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": fmt.Sprintf("file is too large (max %d MB)", maxVideoBytes>>20)})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": "file is required"})
		return
	}
	f, err := fh.Open()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read file"})
		return
	}
	defer f.Close()

	// what the file is comes from its bytes, never from its name or declared type
	head := make([]byte, 512)
	n, _ := io.ReadFull(f, head)
	if videoType := sniffVideo(head[:n]); videoType != "" {
		storeVideoAttachment(c, userID, roomID, fh, f, videoType)
		return
	}

	if fh.Size > maxAttachmentBytes {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": fmt.Sprintf("images are limited to %d MB", maxAttachmentBytes>>20)})
		return
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read file"})
		return
	}
	data, err := io.ReadAll(io.LimitReader(f, maxAttachmentBytes+1))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read file"})
		return
	}
	contentType, cfg, status, msg := checkImage(data)
	if status == http.StatusUnsupportedMediaType && msg == unsupportedImageMsg {
		msg = "only images (JPEG, PNG, GIF, WebP) and videos (MP4, MOV, WebM) are supported"
	}
	if status != 0 {
		c.JSON(status, gin.H{"error": msg})
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

const unsupportedImageMsg = "only JPEG, PNG, GIF and WebP images are supported"

// checkImage validates image bytes by content. status is 0 when they're fine;
// otherwise status and msg describe the error response.
func checkImage(data []byte) (contentType string, cfg image.Config, status int, msg string) {
	contentType = http.DetectContentType(data)
	if _, allowed := allowedImageTypes[contentType]; !allowed {
		return "", cfg, http.StatusUnsupportedMediaType, unsupportedImageMsg
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 {
		return "", cfg, http.StatusUnsupportedMediaType, "unreadable image"
	}
	if cfg.Width*cfg.Height > maxAttachmentPixels {
		return "", cfg, http.StatusRequestEntityTooLarge, "image dimensions are too large"
	}
	return contentType, cfg, 0, ""
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

	contentType, cfg, status, msg := checkImage(data)
	if status != 0 {
		c.JSON(status, gin.H{"error": msg})
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

	var (
		key, contentType          string
		viewOnce, purged, viewing bool
	)
	err := db.DB.QueryRow(
		`SELECT a.object_key, a.content_type, COALESCE(m.view_once, false), a.purged_at IS NOT NULL,
		        o.opened_at IS NOT NULL AND EXTRACT(EPOCH FROM NOW() - o.opened_at) < `+workers.ViewWindowSQL+`
		 FROM attachments a
		 JOIN room_members rm ON rm.room_id = a.room_id AND rm.user_id = $2
		 LEFT JOIN messages m ON m.id = a.message_id
		 LEFT JOIN message_opens o ON o.message_id = a.message_id AND o.user_id = $2
		 WHERE a.id = $1 AND (a.message_id IS NOT NULL OR a.uploader_id = $2)`,
		id, userID,
	).Scan(&key, &contentType, &viewOnce, &purged, &viewing)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "attachment not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch attachment"})
		return
	}

	// view-once: only while the opener's viewing window is open (never the sender)
	if viewOnce && purged {
		c.JSON(http.StatusGone, gin.H{"error": "this has expired"})
		return
	}
	if viewOnce && !viewing {
		c.JSON(http.StatusForbidden, gin.H{"error": "view-once: it can only be opened once"})
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
	if viewOnce {
		// nothing left in the browser cache to look at again
		c.Header("Cache-Control", "no-store")
	}
	c.Header("Content-Type", servedVideoType(contentType))
	// handles Range requests: videos stream and seek instead of downloading whole
	http.ServeContent(c.Writer, c.Request, "", time.Time{}, obj)
}

// attachmentsByMessage loads the attachments of the given messages, in the order they were sent.
func attachmentsByMessage(messageIDs []string) (map[string][]attachmentResponse, error) {
	result := make(map[string][]attachmentResponse)
	if len(messageIDs) == 0 {
		return result, nil
	}

	rows, err := db.DB.Query(
		`SELECT message_id, id, content_type, width, height, size_bytes,
		        COALESCE(duration_ms, 0), poster_key IS NOT NULL, purged_at IS NOT NULL
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
		if err := rows.Scan(&messageID, &a.ID, &a.ContentType, &a.Width, &a.Height, &a.Size, &a.DurationMs, &a.HasPoster, &a.purged); err != nil {
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
