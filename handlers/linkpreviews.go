package handlers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"image"
	"log"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/devstr0ke/chat-mixer/db"
	"github.com/devstr0ke/chat-mixer/linkpreview"
	"github.com/devstr0ke/chat-mixer/storage"
	"github.com/gin-gonic/gin"
)

const (
	linkPreviewTTL        = 7 * 24 * time.Hour
	linkPreviewFailTTL    = time.Hour // pages without a usable preview are retried sooner
	linkPreviewTimeout    = 12 * time.Second
	maxPreviewImageBytes  = 3 << 20
	linkFetchesPerMinute  = 20 // per user, uncached lookups only
	linkFetchWindow       = time.Minute
	linkFetchLimiterUsers = 5000
)

type linkPreviewResponse struct {
	URL         string  `json:"url"`
	Title       string  `json:"title"`
	Description string  `json:"description"`
	SiteName    string  `json:"site_name"`
	ImageID     *string `json:"image_id"`
	ImageWidth  int     `json:"image_width"`
	ImageHeight int     `json:"image_height"`
}

func previewImageKey(imageID string) string {
	return "link-previews/" + imageID
}

// --- per-user limiter for uncached fetches ---

var linkFetchLimiter = struct {
	mu   sync.Mutex
	hits map[string][]time.Time
}{hits: make(map[string][]time.Time)}

func allowLinkFetch(userID string) bool {
	now := time.Now()
	linkFetchLimiter.mu.Lock()
	defer linkFetchLimiter.mu.Unlock()

	if len(linkFetchLimiter.hits) > linkFetchLimiterUsers {
		linkFetchLimiter.hits = make(map[string][]time.Time)
	}
	recent := linkFetchLimiter.hits[userID][:0]
	for _, t := range linkFetchLimiter.hits[userID] {
		if now.Sub(t) < linkFetchWindow {
			recent = append(recent, t)
		}
	}
	if len(recent) >= linkFetchesPerMinute {
		linkFetchLimiter.hits[userID] = recent
		return false
	}
	linkFetchLimiter.hits[userID] = append(recent, now)
	return true
}

// --- one fetch at a time per URL ---

var linkFetchInFlight = struct {
	mu    sync.Mutex
	calls map[string]chan struct{}
}{calls: make(map[string]chan struct{})}

// GetLinkPreview — GET /link-preview?url=
// Answers 404 when the page has no usable preview.
func GetLinkPreview(c *gin.Context) {
	pageURL, err := linkpreview.NormalizeURL(c.Query("url"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid url"})
		return
	}
	sum := sha256.Sum256([]byte(pageURL))
	hash := hex.EncodeToString(sum[:])

	if respondWithCachedPreview(c, hash) {
		return
	}
	if !allowLinkFetch(c.GetString("userID")) {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "too many link previews, try again in a minute"})
		return
	}

	// if another request is already fetching this URL, wait for it and reuse its result
	linkFetchInFlight.mu.Lock()
	if wait, busy := linkFetchInFlight.calls[hash]; busy {
		linkFetchInFlight.mu.Unlock()
		select {
		case <-wait:
		case <-c.Request.Context().Done():
			return
		}
		if !respondWithCachedPreview(c, hash) {
			c.JSON(http.StatusNotFound, gin.H{"error": "no preview"})
		}
		return
	}
	done := make(chan struct{})
	linkFetchInFlight.calls[hash] = done
	linkFetchInFlight.mu.Unlock()
	defer func() {
		linkFetchInFlight.mu.Lock()
		delete(linkFetchInFlight.calls, hash)
		linkFetchInFlight.mu.Unlock()
		close(done)
	}()

	// not tied to the request: the result is cached for everyone even if this caller leaves
	ctx, cancel := context.WithTimeout(context.Background(), linkPreviewTimeout)
	defer cancel()
	preview, ok := buildLinkPreview(ctx, pageURL)
	if err := storeLinkPreview(hash, preview, ok); err != nil {
		log.Printf("link preview: failed to store: %v", err)
	}

	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "no preview"})
		return
	}
	c.JSON(http.StatusOK, preview)
}

// respondWithCachedPreview answers from the cache when there's a fresh entry.
func respondWithCachedPreview(c *gin.Context, hash string) bool {
	var (
		p          linkPreviewResponse
		ok         bool
		ageSeconds float64
	)
	// age is computed by the database so both timestamps share one clock
	err := db.DB.QueryRow(
		`SELECT url, ok, title, description, site_name, image_id, image_width, image_height,
		        EXTRACT(EPOCH FROM (NOW() - fetched_at))
		 FROM link_previews WHERE url_hash = $1`,
		hash,
	).Scan(&p.URL, &ok, &p.Title, &p.Description, &p.SiteName, &p.ImageID, &p.ImageWidth, &p.ImageHeight, &ageSeconds)
	if err != nil {
		return false
	}

	ttl := linkPreviewTTL
	if !ok {
		ttl = linkPreviewFailTTL
	}
	if time.Duration(ageSeconds*float64(time.Second)) > ttl {
		return false
	}
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "no preview"})
		return true
	}
	c.JSON(http.StatusOK, p)
	return true
}

// buildLinkPreview fetches the page and, when it has one, a copy of its
// preview image. ok is false when there's nothing worth showing.
func buildLinkPreview(ctx context.Context, pageURL string) (linkPreviewResponse, bool) {
	preview := linkPreviewResponse{URL: pageURL}

	meta, err := linkpreview.Fetch(ctx, pageURL)
	if err != nil {
		// expected for plenty of links (blocked bots, non-HTML, private hosts…)
		return preview, false
	}
	if meta.Title == "" {
		return preview, false
	}
	preview.Title, preview.Description, preview.SiteName = meta.Title, meta.Description, meta.SiteName
	if preview.SiteName == "" {
		if u, err := url.Parse(pageURL); err == nil {
			preview.SiteName = u.Hostname()
		}
	}

	if meta.ImageURL != "" && storage.Enabled() {
		if imageID, w, h, ok := copyPreviewImage(ctx, meta.ImageURL); ok {
			preview.ImageID, preview.ImageWidth, preview.ImageHeight = &imageID, w, h
		}
	}
	return preview, true
}

// copyPreviewImage stores our own copy of a page's preview image, so viewers'
// browsers never contact the linked site.
func copyPreviewImage(ctx context.Context, imageURL string) (imageID string, width, height int, ok bool) {
	data, err := linkpreview.FetchImage(ctx, imageURL, maxPreviewImageBytes)
	if err != nil {
		return "", 0, 0, false
	}
	contentType := http.DetectContentType(data)
	if _, allowed := allowedImageTypes[contentType]; !allowed {
		return "", 0, 0, false
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width*cfg.Height > maxAttachmentPixels {
		return "", 0, 0, false
	}

	if err := db.DB.QueryRow(`SELECT uuid_generate_v4()`).Scan(&imageID); err != nil {
		return "", 0, 0, false
	}
	if err := storage.Put(ctx, previewImageKey(imageID), contentType, data); err != nil {
		log.Printf("link preview: failed to store image: %v", err)
		return "", 0, 0, false
	}
	return imageID, cfg.Width, cfg.Height, true
}

// storeLinkPreview caches a result (good or not) and drops the image it replaces.
func storeLinkPreview(hash string, p linkPreviewResponse, ok bool) error {
	var previousImage sql.NullString
	err := db.DB.QueryRow(
		`WITH old AS (SELECT image_id FROM link_previews WHERE url_hash = $1)
		 INSERT INTO link_previews (url_hash, url, ok, title, description, site_name, image_id, image_width, image_height, fetched_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, NOW())
		 ON CONFLICT (url_hash) DO UPDATE SET
		     ok = EXCLUDED.ok, title = EXCLUDED.title, description = EXCLUDED.description,
		     site_name = EXCLUDED.site_name, image_id = EXCLUDED.image_id,
		     image_width = EXCLUDED.image_width, image_height = EXCLUDED.image_height,
		     fetched_at = EXCLUDED.fetched_at
		 RETURNING (SELECT image_id FROM old)`,
		hash, p.URL, ok, p.Title, p.Description, p.SiteName, p.ImageID, p.ImageWidth, p.ImageHeight,
	).Scan(&previousImage)
	if err != nil {
		return err
	}

	if previousImage.Valid && storage.Enabled() {
		go func() {
			if err := storage.Delete(context.Background(), previewImageKey(previousImage.String)); err != nil {
				log.Printf("link preview: failed to delete old image: %v", err)
			}
		}()
	}
	return nil
}

// GetLinkPreviewImage streams our stored copy of a preview image. Accepts the
// token cookie since <img> tags can't send headers.
func GetLinkPreviewImage(c *gin.Context) {
	imageID, ok := uuidParam(c, "image_id", "image not found")
	if !ok {
		return
	}

	var current bool
	if err := db.DB.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM link_previews WHERE image_id = $1)`, imageID,
	).Scan(&current); err != nil || !current || !storage.Enabled() {
		c.JSON(http.StatusNotFound, gin.H{"error": "image not found"})
		return
	}

	obj, info, err := storage.Get(c.Request.Context(), previewImageKey(imageID))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "image not found"})
		return
	}
	defer obj.Close()

	if _, allowed := allowedImageTypes[info.ContentType]; !allowed {
		info.ContentType = "application/octet-stream"
	}
	setImageHeaders(c)
	c.DataFromReader(http.StatusOK, info.Size, info.ContentType, obj, nil)
}
