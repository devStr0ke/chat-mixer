package handlers

import (
	"database/sql"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/devstr0ke/chat-mixer/db"
	"github.com/devstr0ke/chat-mixer/models"
	"github.com/gin-gonic/gin"
	"github.com/lib/pq"
)

type lastMessageResponse struct {
	ID              string    `json:"id"`
	SenderID        string    `json:"sender_id"`
	SenderPseudo    string    `json:"sender_pseudo"`
	Content         string    `json:"content"`
	AttachmentCount int       `json:"attachment_count"`
	SentAt          time.Time `json:"sent_at"`
}

type roomSummaryResponse struct {
	ID          string               `json:"id"`
	Name        string               `json:"name"`
	OwnerID     string               `json:"owner_id"`
	CreatedAt   time.Time            `json:"created_at"`
	MemberCount int                  `json:"member_count"`
	UnreadCount int                  `json:"unread_count"`
	LastMessage *lastMessageResponse `json:"last_message"`
}

type memberResponse struct {
	userSummary
	JoinedAt   time.Time `json:"joined_at"`
	LastReadAt time.Time `json:"last_read_at"`
}

type roomDetailResponse struct {
	ID        string           `json:"id"`
	Name      string           `json:"name"`
	OwnerID   string           `json:"owner_id"`
	CreatedAt time.Time        `json:"created_at"`
	Members   []memberResponse `json:"members"`
}

type reactionResponse struct {
	UserID string `json:"user_id"`
	Emoji  string `json:"emoji"`
}

type messageResponse struct {
	ID           string               `json:"id"`
	SenderID     string               `json:"sender_id"`
	SenderPseudo string               `json:"sender_pseudo"`
	Content      string               `json:"content"`
	SentAt       time.Time            `json:"sent_at"`
	Reactions    []reactionResponse   `json:"reactions"`
	Attachments  []attachmentResponse `json:"attachments"`
}

type messagePageResponse struct {
	Messages []messageResponse `json:"messages"`
	HasMore  bool              `json:"has_more"`
}

const (
	defaultMessagePage = 50
	maxMessagePage     = 100
	maxInvitesOnCreate = 50
)

// normalizeRoomName trims the name and checks it fits the rooms.name column.
func normalizeRoomName(name string) (string, bool) {
	name = strings.TrimSpace(name)
	n := utf8.RuneCountInString(name)
	return name, n >= 1 && n <= 64
}

type createRoomRequest struct {
	Name    string   `json:"name"`
	Pseudos []string `json:"pseudos"`
}

// CreateRoom creates a group owned by the caller and invites the given pseudos.
func CreateRoom(c *gin.Context) {
	userID := c.GetString("userID")

	var req createRoomRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	name, ok := normalizeRoomName(req.Name)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required (1-64 chars)"})
		return
	}
	if len(req.Pseudos) > maxInvitesOnCreate {
		c.JSON(http.StatusBadRequest, gin.H{"error": "too many invitations"})
		return
	}

	tx, err := db.DB.Begin()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create room"})
		return
	}
	defer tx.Rollback()

	inviteeIDs, missing, err := resolvePseudos(tx, req.Pseudos, userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to look up users"})
		return
	}
	if len(missing) > 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unknown user: " + strings.Join(missing, ", ")})
		return
	}

	var room models.Room
	err = tx.QueryRow(
		`INSERT INTO rooms (name, owner_id) VALUES ($1, $2)
		 RETURNING id, name, owner_id, created_at`,
		name, userID,
	).Scan(&room.ID, &room.Name, &room.OwnerID, &room.CreatedAt)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create room"})
		return
	}

	if _, err := tx.Exec(`INSERT INTO room_members (room_id, user_id) VALUES ($1, $2)`, room.ID, userID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create room"})
		return
	}

	for _, inviteeID := range inviteeIDs {
		if _, err := tx.Exec(
			`INSERT INTO room_invitations (room_id, inviter_id, invitee_id) VALUES ($1, $2, $3)`,
			room.ID, userID, inviteeID,
		); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to send invitations"})
			return
		}
	}

	if err := tx.Commit(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create room"})
		return
	}

	WSHub.NotifyUsers(mustJSON(WSMessage{Type: "invitation", RoomID: room.ID}), inviteeIDs...)

	c.JSON(http.StatusCreated, room)
}

// resolvePseudos maps pseudos to user IDs, skipping the caller and duplicates.
// Pseudos that match no user are returned in missing.
func resolvePseudos(tx *sql.Tx, pseudos []string, callerID string) (ids, missing []string, err error) {
	if len(pseudos) == 0 {
		return nil, nil, nil
	}

	rows, err := tx.Query(`SELECT id, pseudo FROM users WHERE pseudo = ANY($1::text[])`, pq.Array(pseudos))
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	byPseudo := make(map[string]string)
	for rows.Next() {
		var id, pseudo string
		if err := rows.Scan(&id, &pseudo); err != nil {
			return nil, nil, err
		}
		byPseudo[pseudo] = id
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	seen := make(map[string]bool)
	for _, p := range pseudos {
		id, found := byPseudo[p]
		switch {
		case !found:
			missing = append(missing, p)
		case id == callerID || seen[id]:
		default:
			seen[id] = true
			ids = append(ids, id)
		}
	}
	return ids, missing, nil
}

func GetMyRooms(c *gin.Context) {
	userID := c.GetString("userID")

	rows, err := db.DB.Query(
		`SELECT r.id, r.name, r.owner_id, r.created_at,
		        (SELECT COUNT(*) FROM room_members WHERE room_id = r.id),
		        (SELECT COUNT(*) FROM messages m
		         WHERE m.room_id = r.id AND m.sender_id <> $1 AND m.sent_at > rm.last_read_at),
		        lm.id, lm.sender_id, lu.pseudo, lm.content, lm.sent_at,
		        (SELECT COUNT(*) FROM attachments a WHERE a.message_id = lm.id)
		 FROM room_members rm
		 JOIN rooms r ON r.id = rm.room_id
		 LEFT JOIN LATERAL (
		     SELECT id, sender_id, content, sent_at FROM messages
		     WHERE room_id = r.id
		     ORDER BY sent_at DESC, id DESC
		     LIMIT 1
		 ) lm ON true
		 LEFT JOIN users lu ON lu.id = lm.sender_id
		 WHERE rm.user_id = $1
		 ORDER BY COALESCE(lm.sent_at, r.created_at) DESC`,
		userID,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch rooms"})
		return
	}
	defer rows.Close()

	rooms := make([]roomSummaryResponse, 0)
	for rows.Next() {
		var (
			r                                   roomSummaryResponse
			lmID, lmSender, lmPseudo, lmContent sql.NullString
			lmSentAt                            sql.NullTime
			lmAttachments                       int
		)
		if err := rows.Scan(&r.ID, &r.Name, &r.OwnerID, &r.CreatedAt, &r.MemberCount, &r.UnreadCount,
			&lmID, &lmSender, &lmPseudo, &lmContent, &lmSentAt, &lmAttachments); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to scan room"})
			return
		}
		if lmID.Valid {
			r.LastMessage = &lastMessageResponse{
				ID:              lmID.String,
				SenderID:        lmSender.String,
				SenderPseudo:    lmPseudo.String,
				Content:         lmContent.String,
				AttachmentCount: lmAttachments,
				SentAt:          lmSentAt.Time,
			}
		}
		rooms = append(rooms, r)
	}

	c.JSON(http.StatusOK, rooms)
}

func GetRoom(c *gin.Context) {
	roomID, _, ok := requireMember(c)
	if !ok {
		return
	}

	var resp roomDetailResponse
	err := db.DB.QueryRow(
		`SELECT id, name, owner_id, created_at FROM rooms WHERE id = $1`,
		roomID,
	).Scan(&resp.ID, &resp.Name, &resp.OwnerID, &resp.CreatedAt)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch room"})
		return
	}

	rows, err := db.DB.Query(
		`SELECT u.id, u.pseudo, u.country, rm.joined_at, rm.last_read_at
		 FROM room_members rm
		 JOIN users u ON u.id = rm.user_id
		 WHERE rm.room_id = $1
		 ORDER BY rm.joined_at ASC`,
		roomID,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch members"})
		return
	}
	defer rows.Close()

	resp.Members = make([]memberResponse, 0)
	for rows.Next() {
		var m memberResponse
		if err := rows.Scan(&m.ID, &m.Pseudo, &m.Country, &m.JoinedAt, &m.LastReadAt); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to scan member"})
			return
		}
		resp.Members = append(resp.Members, m)
	}

	c.JSON(http.StatusOK, resp)
}

// GetMessages returns one page of history, oldest first. Pass ?before=<message_id>
// to page backwards from a message already loaded.
func GetMessages(c *gin.Context) {
	roomID, _, ok := requireMember(c)
	if !ok {
		return
	}

	limit := defaultMessagePage
	if raw := c.Query("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxMessagePage {
			c.JSON(http.StatusBadRequest, gin.H{"error": "limit must be between 1 and 100"})
			return
		}
		limit = n
	}

	var before sql.NullString
	if raw := c.Query("before"); raw != "" {
		if !isUUID(raw) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid before cursor"})
			return
		}
		before = sql.NullString{String: raw, Valid: true}
	}

	// fetch one extra row to know whether older messages remain
	rows, err := db.DB.Query(
		`SELECT m.id, m.sender_id, u.pseudo, m.content, m.sent_at
		 FROM messages m
		 JOIN users u ON u.id = m.sender_id
		 WHERE m.room_id = $1
		   AND ($2::uuid IS NULL OR (m.sent_at, m.id) <
		        (SELECT sent_at, id FROM messages WHERE id = $2::uuid AND room_id = $1))
		 ORDER BY m.sent_at DESC, m.id DESC
		 LIMIT $3`,
		roomID, before, limit+1,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch messages"})
		return
	}
	defer rows.Close()

	messages := make([]messageResponse, 0, limit+1)
	for rows.Next() {
		var m messageResponse
		if err := rows.Scan(&m.ID, &m.SenderID, &m.SenderPseudo, &m.Content, &m.SentAt); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to scan message"})
			return
		}
		m.Reactions = make([]reactionResponse, 0)
		m.Attachments = make([]attachmentResponse, 0)
		messages = append(messages, m)
	}
	rows.Close()

	hasMore := len(messages) > limit
	if hasMore {
		messages = messages[:limit]
	}

	// newest-first from the query; clients render oldest-first
	for i, j := 0, len(messages)-1; i < j; i, j = i+1, j-1 {
		messages[i], messages[j] = messages[j], messages[i]
	}

	if len(messages) > 0 {
		index := make(map[string]int, len(messages))
		ids := make([]string, len(messages))
		for i, m := range messages {
			index[m.ID] = i
			ids[i] = m.ID
		}

		reactionRows, err := db.DB.Query(
			`SELECT message_id, user_id, emoji FROM message_reactions
			 WHERE message_id = ANY($1::uuid[])
			 ORDER BY created_at ASC`,
			pq.Array(ids),
		)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch reactions"})
			return
		}
		defer reactionRows.Close()

		for reactionRows.Next() {
			var messageID string
			var r reactionResponse
			if err := reactionRows.Scan(&messageID, &r.UserID, &r.Emoji); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to scan reaction"})
				return
			}
			i := index[messageID]
			messages[i].Reactions = append(messages[i].Reactions, r)
		}

		attachments, err := attachmentsByMessage(ids)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch attachments"})
			return
		}
		for messageID, list := range attachments {
			messages[index[messageID]].Attachments = list
		}
	}

	c.JSON(http.StatusOK, messagePageResponse{Messages: messages, HasMore: hasMore})
}

type updateRoomRequest struct {
	Name string `json:"name"`
}

// UpdateRoom renames the room. Owner only.
func UpdateRoom(c *gin.Context) {
	roomID, isOwner, ok := requireMember(c)
	if !ok {
		return
	}
	if !isOwner {
		c.JSON(http.StatusForbidden, gin.H{"error": "only the owner can rename the room"})
		return
	}

	var req updateRoomRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	name, valid := normalizeRoomName(req.Name)
	if !valid {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required (1-64 chars)"})
		return
	}

	if _, err := db.DB.Exec(`UPDATE rooms SET name = $1 WHERE id = $2`, name, roomID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to rename room"})
		return
	}

	WSHub.BroadcastToAll(roomID, mustJSON(WSMessage{Type: "room_updated", RoomID: roomID}))

	c.JSON(http.StatusOK, gin.H{"message": "room renamed"})
}

// DeleteRoom removes the room and its history for everyone. Owner only.
func DeleteRoom(c *gin.Context) {
	roomID, isOwner, ok := requireMember(c)
	if !ok {
		return
	}
	if !isOwner {
		c.JSON(http.StatusForbidden, gin.H{"error": "only the owner can delete the room"})
		return
	}

	tx, err := db.DB.Begin()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete room"})
		return
	}
	defer tx.Rollback()

	memberIDs, inviteeIDs, err := deleteRoom(tx, roomID)
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete room"})
		return
	}

	WSHub.CloseRoom(roomID)
	deleteRoomFiles(roomID)
	WSHub.NotifyUsers(mustJSON(WSMessage{Type: "room_removed", RoomID: roomID}), memberIDs...)
	WSHub.NotifyUsers(mustJSON(WSMessage{Type: "invitation", RoomID: roomID}), inviteeIDs...)

	c.JSON(http.StatusOK, gin.H{"message": "room deleted"})
}

// deleteRoom deletes a room and returns who was in it and who had a pending
// invitation, so they can be notified.
func deleteRoom(tx *sql.Tx, roomID string) (memberIDs, inviteeIDs []string, err error) {
	memberIDs, err = roomMemberIDs(tx, roomID)
	if err != nil {
		return nil, nil, err
	}

	rows, err := tx.Query(
		`DELETE FROM room_invitations WHERE room_id = $1 RETURNING invitee_id`,
		roomID,
	)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, nil, err
		}
		inviteeIDs = append(inviteeIDs, id)
	}
	rows.Close()

	if _, err := tx.Exec(`DELETE FROM rooms WHERE id = $1`, roomID); err != nil {
		return nil, nil, err
	}
	return memberIDs, inviteeIDs, nil
}

// RemoveMember lets a member leave (target = self) or the owner remove someone.
// When the owner leaves, ownership passes to the longest-standing member; the
// room is deleted once nobody is left.
func RemoveMember(c *gin.Context) {
	callerID := c.GetString("userID")
	roomID, isOwner, ok := requireMember(c)
	if !ok {
		return
	}
	targetID, ok := uuidParam(c, "user_id", "member not found")
	if !ok {
		return
	}

	leaving := targetID == callerID
	if !leaving && !isOwner {
		c.JSON(http.StatusForbidden, gin.H{"error": "only the owner can remove members"})
		return
	}

	tx, err := db.DB.Begin()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to remove member"})
		return
	}
	defer tx.Rollback()

	res, err := tx.Exec(`DELETE FROM room_members WHERE room_id = $1 AND user_id = $2`, roomID, targetID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to remove member"})
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "member not found"})
		return
	}

	roomEmpty := false
	var inviteeIDs []string
	if leaving && isOwner {
		var nextOwner string
		err := tx.QueryRow(
			`SELECT user_id FROM room_members WHERE room_id = $1 ORDER BY joined_at ASC LIMIT 1`,
			roomID,
		).Scan(&nextOwner)
		switch {
		case err == sql.ErrNoRows:
			roomEmpty = true
			if _, inviteeIDs, err = deleteRoom(tx, roomID); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete empty room"})
				return
			}
		case err != nil:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to transfer ownership"})
			return
		default:
			if _, err := tx.Exec(`UPDATE rooms SET owner_id = $1 WHERE id = $2`, nextOwner, roomID); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to transfer ownership"})
				return
			}
		}
	}

	if err := tx.Commit(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to remove member"})
		return
	}

	WSHub.DisconnectUser(roomID, targetID)
	WSHub.NotifyUser(targetID, mustJSON(WSMessage{Type: "room_removed", RoomID: roomID}))

	if roomEmpty {
		WSHub.CloseRoom(roomID)
		deleteRoomFiles(roomID)
		WSHub.NotifyUsers(mustJSON(WSMessage{Type: "invitation", RoomID: roomID}), inviteeIDs...)
	} else {
		WSHub.BroadcastToAll(roomID, mustJSON(WSMessage{Type: "member_left", RoomID: roomID, UserID: targetID}))
	}

	c.JSON(http.StatusOK, gin.H{"message": "member removed"})
}
