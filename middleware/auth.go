package middleware

import (
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
)

func GenerateToken(userID string) (string, error) {
	claims := jwt.MapClaims{
		"sub": userID,
		"iat": time.Now().Unix(),
		"exp": time.Now().Add(72 * time.Hour).Unix(),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString([]byte(os.Getenv("JWT_SECRET")))
}

func AuthRequired() gin.HandlerFunc {
	return authenticate(false)
}

// AuthRequiredOrCookie also accepts the frontend's token cookie, for resources
// the browser loads by itself (e.g. <img> tags) and that can't send headers.
// Only use it on read-only routes.
func AuthRequiredOrCookie() gin.HandlerFunc {
	return authenticate(true)
}

func authenticate(allowCookie bool) gin.HandlerFunc {
	secret := []byte(os.Getenv("JWT_SECRET"))

	return func(c *gin.Context) {
		raw := extractToken(c, allowCookie)
		if raw == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing authorization"})
			return
		}

		token, err := jwt.Parse(raw, func(t *jwt.Token) (interface{}, error) {
			if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, jwt.ErrSignatureInvalid
			}
			return secret, nil
		})
		if err != nil || !token.Valid {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired token"})
			return
		}

		claims, ok := token.Claims.(jwt.MapClaims)
		if !ok {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid token claims"})
			return
		}

		sub, err := claims.GetSubject()
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid token subject"})
			return
		}

		c.Set("userID", sub)
		c.Next()
	}
}

func extractToken(c *gin.Context, allowCookie bool) string {
	if header := c.GetHeader("Authorization"); header != "" {
		parts := strings.SplitN(header, " ", 2)
		if len(parts) == 2 && parts[0] == "Bearer" {
			return parts[1]
		}
	}
	if token := c.Query("token"); token != "" {
		return token
	}
	if allowCookie {
		if token, err := c.Cookie("token"); err == nil {
			return token
		}
	}
	return ""
}
