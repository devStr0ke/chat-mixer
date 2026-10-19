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
	VideoCount      int    `json:"video_count"`
	HasGif          bool   `json:"has_gif"`
	Sticker         bool   `json:"sticker"`
	Drawing         bool   `json:"drawing"`
	ViewOnce        bool   `json:"view_once"`
	Deleted         bool   `json:"deleted"`
}

// replyPreviewColumns selects a replyPreview from `messages rm` joined to its
// sender `users ru`; both may be NULL when the message isn't a reply. The
// text is encrypted, so it's cut to replyPreviewLen after decrypting.
const replyPreviewColumns = `rm.id, rm.room_id, rm.sender_id, ru.pseudo, rm.content, rm.key_id, rm.gif_id IS NOT NULL, rm.gif_sticker OR rm.sticker_id IS NOT NULL,
	EXISTS (SELECT 1 FROM stickers rs WHERE rs.id = rm.sticker_id AND rs.kind = 'drawing'), rm.view_once, rm.deleted_at IS NOT NULL,
	(SELECT COUNT(*) FROM attachments ra WHERE ra.message_id = rm.id),
	(SELECT COUNT(*) FROM attachments ra WHERE ra.message_id = rm.id AND ra.content_type LIKE 'video/%')`

// nullableReply holds the scan targets for replyPreviewColumns.
type nullableReply struct {
	id, roomID, senderID, pseudo, content, keyID sql.NullString
	hasGif, sticker, drawing, viewOnce, deleted  sql.NullBool
	attachments, videos                          int
}

func (r *nullableReply) targets() []any {
	return []any{&r.id, &r.roomID, &r.senderID, &r.pseudo, &r.content, &r.keyID, &r.hasGif, &r.sticker, &r.drawing, &r.viewOnce, &r.deleted, &r.attachments, &r.videos}
}

func (r *nullableReply) preview() *replyPreview {
	if !r.id.Valid {
		return nil
	}
	return &replyPreview{
		ID:              r.id.String,
		SenderID:        r.senderID.String,
		SenderPseudo:    r.pseudo.String,
		Content:         truncateRunes(openMessage(r.roomID.String, r.id.String, r.content.String, r.keyID), replyPreviewLen),
		AttachmentCount: r.attachments,
		VideoCount:      r.videos,
		HasGif:          r.hasGif.Bool,
		Sticker:         r.sticker.Bool,
		Drawing:         r.drawing.Bool,
		ViewOnce:        r.viewOnce.Bool,
		Deleted:         r.deleted.Bool,
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
		 WHERE rm.id = $1 AND rm.room_id = $2 AND rm.deleted_at IS NULL`,
		messageID, roomID,
	).Scan(r.targets()...)
	if err != nil {
		return nil, err
	}
	return r.preview(), nil
}
