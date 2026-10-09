package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestUploadCORS(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	cors := UploadCORS(" https://chatmixer.space/ , https://www.chatmixer.space")
	r.OPTIONS("/upload", cors)
	r.POST("/upload", cors, func(c *gin.Context) { c.Status(http.StatusCreated) })

	do := func(method, origin string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/upload", nil)
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}

	pre := do(http.MethodOptions, "https://chatmixer.space")
	if pre.Code != http.StatusNoContent ||
		pre.Header().Get("Access-Control-Allow-Origin") != "https://chatmixer.space" ||
		pre.Header().Get("Access-Control-Allow-Headers") != "Authorization" {
		t.Errorf("allowed preflight: %d %v", pre.Code, pre.Header())
	}
	if pre.Header().Get("Access-Control-Allow-Credentials") != "" {
		t.Error("credentials must not be allowed")
	}

	if got := do(http.MethodOptions, "https://evil.example").Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("other origin got Allow-Origin %q", got)
	}

	post := do(http.MethodPost, "https://www.chatmixer.space")
	if post.Code != http.StatusCreated || post.Header().Get("Access-Control-Allow-Origin") != "https://www.chatmixer.space" {
		t.Errorf("allowed POST: %d %v", post.Code, post.Header())
	}
	if post.Header().Get("Access-Control-Allow-Methods") != "" {
		t.Error("preflight-only headers sent on the actual request")
	}

	if same := do(http.MethodPost, ""); same.Code != http.StatusCreated || same.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("request without Origin: %d %v", same.Code, same.Header())
	}
}
