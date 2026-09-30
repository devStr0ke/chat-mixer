package handlers

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// GIF search is proxied through the API so the GIPHY key never reaches the
// browser. Results are cached: starter keys only allow 100 calls per hour.

const (
	giphyBaseURL   = "https://api.giphy.com/v1/gifs"
	gifPageSize    = 24
	gifRating      = "pg-13"
	gifMaxOffset   = 4999 // GIPHY's own limit
	gifSearchTTL   = 10 * time.Minute
	gifTrendingTTL = 15 * time.Minute
	gifByIDTTL     = time.Hour
	gifCacheMax    = 2000
)

var (
	giphyHTTP = &http.Client{Timeout: 8 * time.Second}
	giphyID   = regexp.MustCompile(`^[A-Za-z0-9]{1,64}$`)

	errGifRateLimited = errors.New("giphy rate limit reached")
	errGifNotFound    = errors.New("gif not found")
)

// messageGif is the GIF attached to a message.
type messageGif struct {
	ID     string `json:"id"`
	URL    string `json:"url"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

// gifResponse is a search result: the rendition sent in messages plus a
// smaller one for the picker grid.
type gifResponse struct {
	messageGif
	Title         string `json:"title"`
	PreviewURL    string `json:"preview_url"`
	PreviewWidth  int    `json:"preview_width"`
	PreviewHeight int    `json:"preview_height"`
}

type gifPageResponse struct {
	Gifs       []gifResponse `json:"gifs"`
	NextOffset *int          `json:"next_offset"`
}

func giphyKey() string {
	return os.Getenv("GIPHY_API_KEY")
}

// --- cache ---

type cachedPage struct {
	page    gifPageResponse
	expires time.Time
}

type cachedGif struct {
	gif     messageGif
	expires time.Time
}

var gifCache = struct {
	mu    sync.Mutex
	pages map[string]cachedPage
	gifs  map[string]cachedGif
}{pages: make(map[string]cachedPage), gifs: make(map[string]cachedGif)}

func cachedGifPage(key string) (gifPageResponse, bool) {
	gifCache.mu.Lock()
	defer gifCache.mu.Unlock()
	entry, ok := gifCache.pages[key]
	if !ok || time.Now().After(entry.expires) {
		return gifPageResponse{}, false
	}
	return entry.page, true
}

func storeGifPage(key string, page gifPageResponse, ttl time.Duration) {
	now := time.Now()
	gifCache.mu.Lock()
	defer gifCache.mu.Unlock()

	if len(gifCache.pages) >= gifCacheMax || len(gifCache.gifs) >= gifCacheMax {
		for k, v := range gifCache.pages {
			if now.After(v.expires) {
				delete(gifCache.pages, k)
			}
		}
		for k, v := range gifCache.gifs {
			if now.After(v.expires) {
				delete(gifCache.gifs, k)
			}
		}
		// still full of live entries: start over rather than grow without bound
		if len(gifCache.pages) >= gifCacheMax {
			gifCache.pages = make(map[string]cachedPage)
		}
		if len(gifCache.gifs) >= gifCacheMax {
			gifCache.gifs = make(map[string]cachedGif)
		}
	}

	gifCache.pages[key] = cachedPage{page: page, expires: now.Add(ttl)}
	// remembered so sending one of these doesn't cost another GIPHY call
	for _, g := range page.Gifs {
		gifCache.gifs[g.ID] = cachedGif{gif: g.messageGif, expires: now.Add(gifByIDTTL)}
	}
}

// --- GIPHY client ---

type giphyRendition struct {
	URL    string `json:"url"`
	Webp   string `json:"webp"`
	Width  string `json:"width"`
	Height string `json:"height"`
}

type giphyItem struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Images struct {
		FixedHeight giphyRendition `json:"fixed_height"`
		FixedWidth  giphyRendition `json:"fixed_width"`
	} `json:"images"`
}

// media picks the lighter animated WebP when GIPHY provides one and returns
// false if the rendition is unusable or not served from giphy.com.
func (r giphyRendition) media() (mediaURL string, width, height int, ok bool) {
	mediaURL = r.Webp
	if mediaURL == "" {
		mediaURL = r.URL
	}
	u, err := url.Parse(mediaURL)
	if err != nil || u.Scheme != "https" || !strings.HasSuffix(u.Hostname(), ".giphy.com") {
		return "", 0, 0, false
	}
	width, _ = strconv.Atoi(r.Width)
	height, _ = strconv.Atoi(r.Height)
	if width <= 0 || height <= 0 {
		return "", 0, 0, false
	}
	return mediaURL, width, height, true
}

func (item giphyItem) toResponse() (gifResponse, bool) {
	if !giphyID.MatchString(item.ID) {
		return gifResponse{}, false
	}
	mediaURL, w, h, ok := item.Images.FixedHeight.media()
	if !ok {
		return gifResponse{}, false
	}
	previewURL, pw, ph, ok := item.Images.FixedWidth.media()
	if !ok {
		previewURL, pw, ph = mediaURL, w, h
	}
	return gifResponse{
		messageGif:    messageGif{ID: item.ID, URL: mediaURL, Width: w, Height: h},
		Title:         item.Title,
		PreviewURL:    previewURL,
		PreviewWidth:  pw,
		PreviewHeight: ph,
	}, true
}

// giphyGet calls a GIPHY endpoint and decodes its JSON body into out.
func giphyGet(path string, params url.Values, out any) error {
	params.Set("api_key", giphyKey())
	resp, err := giphyHTTP.Get(giphyBaseURL + path + "?" + params.Encode())
	if err != nil {
		// don't log err itself: it contains the request URL with the API key
		return errors.New("giphy request failed")
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return errGifRateLimited
	case resp.StatusCode == http.StatusNotFound:
		return errGifNotFound
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("giphy answered %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func fetchGifPage(path string, params url.Values, offset int) (gifPageResponse, error) {
	params.Set("limit", strconv.Itoa(gifPageSize))
	params.Set("offset", strconv.Itoa(offset))
	params.Set("rating", gifRating)
	params.Set("bundle", "messaging_non_clips")

	var body struct {
		Data       []giphyItem `json:"data"`
		Pagination struct {
			TotalCount int `json:"total_count"`
			Count      int `json:"count"`
			Offset     int `json:"offset"`
		} `json:"pagination"`
	}
	if err := giphyGet(path, params, &body); err != nil {
		return gifPageResponse{}, err
	}

	page := gifPageResponse{Gifs: make([]gifResponse, 0, len(body.Data))}
	for _, item := range body.Data {
		if g, ok := item.toResponse(); ok {
			page.Gifs = append(page.Gifs, g)
		}
	}
	if next := body.Pagination.Offset + body.Pagination.Count; body.Pagination.Count > 0 &&
		next < body.Pagination.TotalCount && next <= gifMaxOffset {
		page.NextOffset = &next
	}
	return page, nil
}

// resolveGif returns the GIF for an id picked in the picker. The URL always
// comes from GIPHY (cache or API), never from the client.
func resolveGif(id string) (messageGif, error) {
	if !giphyID.MatchString(id) || giphyKey() == "" {
		return messageGif{}, errGifNotFound
	}

	gifCache.mu.Lock()
	entry, ok := gifCache.gifs[id]
	gifCache.mu.Unlock()
	if ok && time.Now().Before(entry.expires) {
		return entry.gif, nil
	}

	var body struct {
		Data giphyItem `json:"data"`
	}
	if err := giphyGet("/"+id, url.Values{}, &body); err != nil {
		return messageGif{}, err
	}
	g, ok := body.Data.toResponse()
	if !ok {
		return messageGif{}, errGifNotFound
	}

	gifCache.mu.Lock()
	gifCache.gifs[id] = cachedGif{gif: g.messageGif, expires: time.Now().Add(gifByIDTTL)}
	gifCache.mu.Unlock()
	return g.messageGif, nil
}

// scanGif turns nullable gif columns into a message's gif.
func scanGif(id, mediaURL sql.NullString, width, height sql.NullInt64) *messageGif {
	if !id.Valid || !mediaURL.Valid {
		return nil
	}
	return &messageGif{ID: id.String, URL: mediaURL.String, Width: int(width.Int64), Height: int(height.Int64)}
}

// --- handlers ---

// gifOffset parses ?offset=, answering 400 itself when it's invalid.
func gifOffset(c *gin.Context) (int, bool) {
	raw := c.Query("offset")
	if raw == "" {
		return 0, true
	}
	offset, err := strconv.Atoi(raw)
	if err != nil || offset < 0 || offset > gifMaxOffset {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid offset"})
		return 0, false
	}
	return offset, true
}

func respondWithGifPage(c *gin.Context, cacheKey, path string, params url.Values, offset int, ttl time.Duration) {
	if giphyKey() == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "GIF search is not available"})
		return
	}
	if page, ok := cachedGifPage(cacheKey); ok {
		c.JSON(http.StatusOK, page)
		return
	}

	page, err := fetchGifPage(path, params, offset)
	if errors.Is(err, errGifRateLimited) {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "GIF search is busy, try again in a few minutes"})
		return
	}
	if err != nil {
		log.Printf("gifs: %v", err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "GIF search failed"})
		return
	}

	storeGifPage(cacheKey, page, ttl)
	c.JSON(http.StatusOK, page)
}

// SearchGifs — GET /gifs/search?q=&offset=
func SearchGifs(c *gin.Context) {
	q := strings.ToLower(strings.TrimSpace(c.Query("q")))
	if q == "" || len(q) > 100 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "q is required (max 100 chars)"})
		return
	}
	offset, ok := gifOffset(c)
	if !ok {
		return
	}

	params := url.Values{"q": {q}, "lang": {"en"}}
	respondWithGifPage(c, fmt.Sprintf("search|%s|%d", q, offset), "/search", params, offset, gifSearchTTL)
}

// TrendingGifs — GET /gifs/trending?offset=
func TrendingGifs(c *gin.Context) {
	offset, ok := gifOffset(c)
	if !ok {
		return
	}
	respondWithGifPage(c, fmt.Sprintf("trending|%d", offset), "/trending", url.Values{}, offset, gifTrendingTTL)
}
