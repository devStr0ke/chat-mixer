package handlers

import (
	"database/sql"
	"net/http"
	"strings"
	"time"

	"github.com/devstr0ke/chat-mixer/db"
	"github.com/gin-gonic/gin"
)

type invitationResponse struct {
	ID        string      `json:"id"`
	RoomID    string      `json:"room_id"`
	RoomName  string      `json:"room_name"`
	Inviter   userSummary `json:"inviter"`
	Invitee   userSummary `json:"invitee"`
	CreatedAt time.Time   `json:"created_at"`
}

const invitationSelect = `
	SELECT i.id, i.room_id, r.name, i.created_at,
	       inv.id, inv.pseudo, inv.country,
	       ee.id, ee.pseudo, ee.country
	FROM room_invitations i
	JOIN rooms r   ON r.id = i.room_id
	JOIN users inv ON inv.id = i.inviter_id
	JOIN users ee  ON ee.id = i.invitee_id
`

func queryInvitations(where string, args ...any) ([]invitationResponse, error) {
	rows, err := db.DB.Query(invitationSelect+" WHERE "+where+" ORDER BY i.created_at DESC", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	invitations := make([]invitationResponse, 0)
	for rows.Next() {
		var i invitationResponse
		if err := rows.Scan(&i.ID, &i.RoomID, &i.RoomName, &i.CreatedAt,
			&i.Inviter.ID, &i.Inviter.Pseudo, &i.Inviter.Country,
			&i.Invitee.ID, &i.Invitee.Pseudo, &i.Invitee.Country); err != nil {
			return nil, err
		}
		invitations = append(invitations, i)
	}
	return invitations, rows.Err()
}

type inviteRequest struct {
	Pseudo string `json:"pseudo" binding:"required"`
}

// InviteToRoom lets any member invite another user by pseudo.
func InviteToRoom(c *gin.Context) {
	userID := c.GetString("userID")
	roomID, _, ok := requireMember(c)
	if !ok {
		return
	}

	var req inviteRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "pseudo is required"})
		return
	}

	var inviteeID string
	err := db.DB.QueryRow(`SELECT id FROM users WHERE pseudo = $1`, strings.TrimSpace(req.Pseudo)).Scan(&inviteeID)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to look up user"})
		return
	}

	var isMember bool
	if err := db.DB.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM room_members WHERE room_id = $1 AND user_id = $2)`,
		roomID, inviteeID,
	).Scan(&isMember); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to check membership"})
		return
	}
	if isMember {
		c.JSON(http.StatusConflict, gin.H{"error": "user is already a member"})
		return
	}

	var invitationID string
	err = db.DB.QueryRow(
		`INSERT INTO room_invitations (room_id, inviter_id, invitee_id)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (room_id, invitee_id) DO NOTHING
		 RETURNING id`,
		roomID, userID, inviteeID,
	).Scan(&invitationID)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusConflict, gin.H{"error": "user is already invited"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create invitation"})
		return
	}

	invitations, err := queryInvitations("i.id = $1", invitationID)
	if err != nil || len(invitations) == 0 {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load invitation"})
		return
	}

	WSHub.NotifyUser(inviteeID, mustJSON(WSMessage{Type: "invitation", RoomID: roomID}))
	WSHub.BroadcastToAll(roomID, mustJSON(WSMessage{Type: "room_updated", RoomID: roomID}))

	c.JSON(http.StatusCreated, invitations[0])
}

// GetRoomInvitations lists a room's pending invitations. Members only.
func GetRoomInvitations(c *gin.Context) {
	roomID, _, ok := requireMember(c)
	if !ok {
		return
	}

	invitations, err := queryInvitations("i.room_id = $1", roomID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch invitations"})
		return
	}
	c.JSON(http.StatusOK, invitations)
}

// GetMyInvitations lists the invitations the caller has received.
func GetMyInvitations(c *gin.Context) {
	invitations, err := queryInvitations("i.invitee_id = $1", c.GetString("userID"))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch invitations"})
		return
	}
	c.JSON(http.StatusOK, invitations)
}

func AcceptInvitation(c *gin.Context) {
	userID := c.GetString("userID")
	invitationID, ok := uuidParam(c, "invitation_id", "invitation not found")
	if !ok {
		return
	}

	tx, err := db.DB.Begin()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to accept invitation"})
		return
	}
	defer tx.Rollback()

	var roomID string
	err = tx.QueryRow(
		`DELETE FROM room_invitations WHERE id = $1 AND invitee_id = $2 RETURNING room_id`,
		invitationID, userID,
	).Scan(&roomID)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "invitation not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to accept invitation"})
		return
	}

	if _, err := tx.Exec(
		`INSERT INTO room_members (room_id, user_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
		roomID, userID,
	); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to join room"})
		return
	}

	if err := tx.Commit(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to accept invitation"})
		return
	}

	WSHub.BroadcastToAll(roomID, mustJSON(WSMessage{Type: "member_joined", RoomID: roomID, UserID: userID}))

	c.JSON(http.StatusOK, gin.H{"room_id": roomID})
}

// DeleteInvitation declines (invitee) or cancels (inviter or room owner) an invitation.
func DeleteInvitation(c *gin.Context) {
	userID := c.GetString("userID")
	invitationID, ok := uuidParam(c, "invitation_id", "invitation not found")
	if !ok {
		return
	}

	var inviteeID, roomID string
	err := db.DB.QueryRow(
		`DELETE FROM room_invitations i
		 USING rooms r
		 WHERE i.id = $1 AND r.id = i.room_id
		   AND (i.invitee_id = $2 OR i.inviter_id = $2 OR r.owner_id = $2)
		 RETURNING i.invitee_id, i.room_id`,
		invitationID, userID,
	).Scan(&inviteeID, &roomID)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "invitation not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete invitation"})
		return
	}

	if inviteeID != userID {
		WSHub.NotifyUser(inviteeID, mustJSON(WSMessage{Type: "invitation", RoomID: roomID}))
	}
	WSHub.BroadcastToAll(roomID, mustJSON(WSMessage{Type: "room_updated", RoomID: roomID}))

	c.JSON(http.StatusOK, gin.H{"message": "invitation deleted"})
}
