package handlers

import (
	"net/http"
	"strings"

	"github.com/devstr0ke/chat-mixer/db"
	"github.com/gin-gonic/gin"
)

var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// SearchUsers returns up to 10 users whose pseudo starts with ?q=, excluding the caller.
func SearchUsers(c *gin.Context) {
	userID := c.GetString("userID")
	q := strings.TrimSpace(c.Query("q"))

	users := make([]userSummary, 0)
	if q == "" {
		c.JSON(http.StatusOK, users)
		return
	}

	rows, err := db.DB.Query(
		`SELECT `+userSummaryColumns("u")+` FROM users u
		 WHERE u.pseudo ILIKE $1 AND u.id <> $2
		 ORDER BY length(u.pseudo), u.pseudo
		 LIMIT 10`,
		likeEscaper.Replace(q)+"%", userID,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to search users"})
		return
	}
	defer rows.Close()

	for rows.Next() {
		var u userSummary
		if err := rows.Scan(u.scanTargets()...); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to scan user"})
			return
		}
		users = append(users, u)
	}

	c.JSON(http.StatusOK, users)
}
