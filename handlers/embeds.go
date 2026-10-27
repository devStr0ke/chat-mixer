package handlers

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/devstr0ke/chat-mixer/linkpreview"
	"github.com/gin-gonic/gin"
)

// The app plays YouTube, TikTok and Instagram videos in place, from the
// sites' own embedded players. Their apps share short links
// (vm.tiktok.com/…, instagram.com/share/…) that don't say which video they
// are: those are resolved here, following only those sites' own redirects.

const (
	embedResolveTimeout = 8 * time.Second
	embedCacheTTL       = 24 * time.Hour
	embedCacheMax       = 5000
)

var embedCache = struct {
	sync.Mutex
	entries map[string]embedCacheEntry
}{entries: make(map[string]embedCacheEntry)}

type embedCacheEntry struct {
	url string
	at  time.Time
}

func hostIn(u *url.URL, hosts ...string) bool {
	host := strings.ToLower(u.Hostname())
	for _, h := range hosts {
		if host == h || host == "www."+h || host == "m."+h {
			return true
		}
	}
	return false
}

// isShortVideoLink tells the share links the player can't read by itself.
func isShortVideoLink(u *url.URL) bool {
	host := strings.ToLower(u.Hostname())
	switch {
	case host == "vm.tiktok.com" || host == "vt.tiktok.com":
		return true
	case hostIn(u, "tiktok.com"):
		return strings.HasPrefix(u.Path, "/t/")
	case hostIn(u, "instagram.com"):
		return strings.HasPrefix(u.Path, "/share/")
	}
	return false
}

// allowedVideoHop keeps the resolution on TikTok's and Instagram's own sites.
func allowedVideoHop(u *url.URL) bool {
	host := strings.ToLower(u.Hostname())
	return host == "vm.tiktok.com" || host == "vt.tiktok.com" || hostIn(u, "tiktok.com", "instagram.com")
}

// ResolveVideoLink — GET /link-embed?url=: the full address behind a short
// TikTok or Instagram share link, { "url": "…" }.
func ResolveVideoLink(c *gin.Context) {
	raw, err := linkpreview.NormalizeURL(c.Query("url"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid url"})
		return
	}
	u, _ := url.Parse(raw)
	if u == nil || !isShortVideoLink(u) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "not a short video link"})
		return
	}

	embedCache.Lock()
	entry, ok := embedCache.entries[raw]
	embedCache.Unlock()
	if ok && time.Since(entry.at) < embedCacheTTL {
		c.JSON(http.StatusOK, gin.H{"url": entry.url})
		return
	}
	if !allowLinkFetch(c.GetString("userID")) {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "too many links, try again in a minute"})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), embedResolveTimeout)
	defer cancel()
	final, err := linkpreview.Follow(ctx, raw, allowedVideoHop, func(next *url.URL) bool { return !isShortVideoLink(next) })
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "couldn't open this link"})
		return
	}

	embedCache.Lock()
	if len(embedCache.entries) >= embedCacheMax {
		embedCache.entries = make(map[string]embedCacheEntry)
	}
	embedCache.entries[raw] = embedCacheEntry{url: final, at: time.Now()}
	embedCache.Unlock()
	c.JSON(http.StatusOK, gin.H{"url": final})
}
