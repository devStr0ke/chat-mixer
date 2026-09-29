package handlers

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"regexp"

	"github.com/devstr0ke/chat-mixer/db"
	"github.com/gin-gonic/gin"
)

// userSummary is the public view of a user embedded in other responses.
type userSummary struct {
	ID      string `json:"id"`
	Pseudo  string `json:"pseudo"`
	Country string `json:"country"`
}

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func isUUID(s string) bool {
	return uuidPattern.MatchString(s)
}

// uuidParam reads a UUID path param, answering 404 when it's malformed so
// Postgres never sees an invalid uuid literal.
func uuidParam(c *gin.Context, name, notFound string) (string, bool) {
	v := c.Param(name)
	if !isUUID(v) {
		c.JSON(http.StatusNotFound, gin.H{"error": notFound})
		return "", false
	}
	return v, true
}

// requireMember resolves the :room_id param and checks the caller belongs to it.
// Non-members get the same 404 as a missing room.
func requireMember(c *gin.Context) (roomID string, isOwner bool, ok bool) {
	roomID, ok = uuidParam(c, "room_id", "room not found")
	if !ok {
		return "", false, false
	}

	err := db.DB.QueryRow(
		`SELECT r.owner_id = rm.user_id
		 FROM rooms r
		 JOIN room_members rm ON rm.room_id = r.id AND rm.user_id = $2
		 WHERE r.id = $1`,
		roomID, c.GetString("userID"),
	).Scan(&isOwner)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "room not found"})
		return "", false, false
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch room"})
		return "", false, false
	}
	return roomID, isOwner, true
}

// querier is satisfied by both *sql.DB and *sql.Tx.
type querier interface {
	Query(query string, args ...any) (*sql.Rows, error)
}

// roomMemberIDs lists the users currently in a room.
func roomMemberIDs(q querier, roomID string) ([]string, error) {
	rows, err := q.Query(`SELECT user_id FROM room_members WHERE room_id = $1`, roomID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func mustJSON(v any) []byte {
	out, _ := json.Marshal(v)
	return out
}
