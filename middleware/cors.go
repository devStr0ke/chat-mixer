package middleware

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// UploadCORS lets the web app send uploads straight to the API, from the
// origins listed in origins (comma-separated). They can't go through the
// front-end's /api rewrite: Next.js caps proxied bodies at 10 MB and keeps a
// copy of each in memory, which videos would blow through. Requests
// authenticate with the Authorization header, so no credentials are allowed.
// It goes before auth so that error responses are readable by the page too.
func UploadCORS(origins string) gin.HandlerFunc {
	allowed := map[string]bool{}
	for _, o := range strings.Split(origins, ",") {
		if o = strings.TrimRight(strings.TrimSpace(o), "/"); o != "" {
			allowed[o] = true
		}
	}

	return func(c *gin.Context) {
		h := c.Writer.Header()
		h.Add("Vary", "Origin")
		if origin := c.GetHeader("Origin"); allowed[origin] {
			h.Set("Access-Control-Allow-Origin", origin)
			if c.Request.Method == http.MethodOptions {
				h.Set("Access-Control-Allow-Methods", "POST")
				h.Set("Access-Control-Allow-Headers", "Authorization")
				h.Set("Access-Control-Max-Age", "600")
			}
		}
		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}
