package handlers

import (
	"database/sql"

	"github.com/devstr0ke/chat-mixer/db"
)

// replyPreviewLen caps how much of the quoted message travels with a reply.
const replyPreviewLen = 140

// replyPreview is the quoted message shown inside a reply.
type replyPreview struct {
	ID              string `json:"id"`
	SenderID        string `json:"sender_id"`
	SenderPseudo    string `json:"sender_pseudo"`
	Content         string `json:"content"`
	AttachmentCount int    `json:"attachment_count"`
	HasGif          bool   `json:"has_gif"`
}

// replyPreviewColumns selects a replyPreview from `messages rm` joined to its
// sender `users ru`; both may be NULL when the message isn't a reply.
const replyPreviewColumns = `rm.id, rm.sender_id, ru.pseudo, LEFT(rm.content, 140), rm.gif_id IS NOT NULL,
	(SELECT COUNT(*) FROM attachments ra WHERE ra.message_id = rm.id)`

// nullableReply holds the scan targets for replyPreviewColumns.
type nullableReply struct {
	id, senderID, pseudo, content sql.NullString
	hasGif                        sql.NullBool
	attachments                   int
}

func (r *nullableReply) targets() []any {
	return []any{&r.id, &r.senderID, &r.pseudo, &r.content, &r.hasGif, &r.attachments}
}

func (r *nullableReply) preview() *replyPreview {
	if !r.id.Valid {
		return nil
	}
	return &replyPreview{
		ID:              r.id.String,
		SenderID:        r.senderID.String,
		SenderPseudo:    r.pseudo.String,
		Content:         r.content.String,
		AttachmentCount: r.attachments,
		HasGif:          r.hasGif.Bool,
	}
}

// loadReplyPreview returns the message being replied to, or sql.ErrNoRows if
// it doesn't exist in this room.
func loadReplyPreview(roomID, messageID string) (*replyPreview, error) {
	var r nullableReply
	err := db.DB.QueryRow(
		`SELECT `+replyPreviewColumns+`
		 FROM messages rm
		 JOIN users ru ON ru.id = rm.sender_id
		 WHERE rm.id = $1 AND rm.room_id = $2`,
		messageID, roomID,
	).Scan(r.targets()...)
	if err != nil {
		return nil, err
	}
	return r.preview(), nil
}
