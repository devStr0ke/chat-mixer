package handlers

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"strconv"
	"time"

	"github.com/devstr0ke/chat-mixer/db"
	"github.com/devstr0ke/chat-mixer/storage"
	"github.com/gin-gonic/gin"
)

// Videos are stored as uploaded (no transcoding). The browser that sends one
// also sends a poster frame, whose dimensions give the video's shape.
const (
	maxVideoBytes      = 100 << 20
	maxPosterBytes     = 2 << 20
	maxVideoDurationMs = 24 * 60 * 60 * 1000
	videoStoreTimeout  = 3 * time.Minute
)

var videoExtensions = map[string]string{
	"video/mp4":       "mp4",
	"video/quicktime": "mov",
	"video/webm":      "webm",
}

// ISO base-media brands of playable video files (an "ftyp" box at offset 4).
// Image formats sharing the container (HEIC/AVIF: heic, mif1, avif) and audio
// (M4A) are deliberately absent.
var mp4VideoBrands = map[string]bool{
	"isom": true, "iso2": true, "iso4": true, "iso5": true, "iso6": true,
	"mp41": true, "mp42": true, "avc1": true, "M4V ": true, "M4VP": true,
	"MSNV": true, "dash": true, "3gp4": true, "3gp5": true, "3g2a": true,
}

// sniffVideo identifies a supported video container from its first bytes,
// returning "" for anything else.
func sniffVideo(head []byte) string {
	if len(head) >= 12 && string(head[4:8]) == "ftyp" {
		brand := string(head[8:12])
		if brand == "qt  " {
			return "video/quicktime"
		}
		if mp4VideoBrands[brand] {
			return "video/mp4"
		}
		return ""
	}
	// EBML header; only the WebM flavour of Matroska plays in browsers
	if bytes.HasPrefix(head, []byte{0x1A, 0x45, 0xDF, 0xA3}) && bytes.Contains(head[:min(len(head), 64)], []byte("webm")) {
		return "video/webm"
	}
	return ""
}

// servedVideoType is the Content-Type used for playback. QuickTime files from
// phones are MP4-family containers; labelled as MP4 they play in browsers
// (Firefox refuses video/quicktime outright).
func servedVideoType(contentType string) string {
	if contentType == "video/quicktime" {
		return "video/mp4"
	}
	return contentType
}

// storeVideoAttachment streams an uploaded video to the bucket along with its
// optional poster frame. f is positioned anywhere; it's rewound first.
func storeVideoAttachment(c *gin.Context, userID, roomID string, fh *multipart.FileHeader, f multipart.File, contentType string) {
	if fh.Size > maxVideoBytes {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": fmt.Sprintf("videos are limited to %d MB", maxVideoBytes>>20)})
		return
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read file"})
		return
	}

	// shape from the poster frame; a 16:9 placeholder when the browser couldn't make one
	width, height := 640, 360
	var poster []byte
	var posterType string
	if pfh, err := c.FormFile("poster"); err == nil && pfh.Size <= maxPosterBytes {
		if pf, err := pfh.Open(); err == nil {
			data, _ := io.ReadAll(io.LimitReader(pf, maxPosterBytes+1))
			pf.Close()
			if ct, cfg, status, _ := checkImage(data); status == 0 && ct != "image/gif" {
				poster, posterType = data, ct
				width, height = cfg.Width, cfg.Height
			}
		}
	}
	durationMs, _ := strconv.Atoi(c.PostForm("duration_ms"))
	durationMs = max(0, min(durationMs, maxVideoDurationMs))

	var id string
	if err := db.DB.QueryRow(`SELECT uuid_generate_v4()`).Scan(&id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to store file"})
		return
	}
	key := fmt.Sprintf("rooms/%s/%s.%s", roomID, id, videoExtensions[contentType])

	ctx, cancel := context.WithTimeout(c.Request.Context(), videoStoreTimeout)
	defer cancel()
	if err := storage.PutStream(ctx, key, contentType, f, fh.Size); err != nil {
		log.Printf("storage: put %s failed: %v", key, err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to store file"})
		return
	}

	var posterKey sql.NullString
	if poster != nil {
		pk := fmt.Sprintf("rooms/%s/%s-poster.%s", roomID, id, allowedImageTypes[posterType])
		if err := storage.Put(ctx, pk, posterType, poster); err != nil {
			log.Printf("storage: put poster failed: %v", err) // the video still works without it
		} else {
			posterKey = sql.NullString{String: pk, Valid: true}
		}
	}

	if _, err := db.DB.Exec(
		`INSERT INTO attachments (id, room_id, uploader_id, object_key, content_type, size_bytes, width, height, duration_ms, poster_key)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		id, roomID, userID, key, contentType, fh.Size, width, height, durationMs, posterKey,
	); err != nil {
		storage.Delete(context.Background(), key)
		if posterKey.Valid {
			storage.Delete(context.Background(), posterKey.String)
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to store file"})
		return
	}

	c.JSON(http.StatusCreated, attachmentResponse{
		ID:          id,
		ContentType: contentType,
		Width:       width,
		Height:      height,
		Size:        fh.Size,
		DurationMs:  durationMs,
		HasPoster:   posterKey.Valid,
	})
}

// GetAttachmentPoster streams a video's poster frame, with the same access
// rules as the video itself.
func GetAttachmentPoster(c *gin.Context) {
	userID := c.GetString("userID")
	id, ok := uuidParam(c, "attachment_id", "attachment not found")
	if !ok {
		return
	}
	if !storage.Enabled() {
		c.JSON(http.StatusNotFound, gin.H{"error": "attachment not found"})
		return
	}

	var posterKey sql.NullString
	err := db.DB.QueryRow(
		`SELECT a.poster_key
		 FROM attachments a
		 JOIN room_members rm ON rm.room_id = a.room_id AND rm.user_id = $2
		 WHERE a.id = $1 AND (a.message_id IS NOT NULL OR a.uploader_id = $2)`,
		id, userID,
	).Scan(&posterKey)
	if err != nil || !posterKey.Valid {
		c.JSON(http.StatusNotFound, gin.H{"error": "poster not found"})
		return
	}

	obj, info, err := storage.Get(c.Request.Context(), posterKey.String)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "poster not found"})
		return
	}
	defer obj.Close()

	setImageHeaders(c)
	c.Header("Content-Type", info.ContentType)
	http.ServeContent(c.Writer, c.Request, "", time.Time{}, obj)
}
