package handlers

import (
	"context"
	"database/sql"
	"log"
	"net/http"
	"time"

	"github.com/devstr0ke/chat-mixer/db"
	"github.com/devstr0ke/chat-mixer/storage"
	"github.com/gin-gonic/gin"
)

// DeleteMessage — DELETE /messages/:message_id: the sender deletes their
// message for everyone. Its text, files, GIF, reactions, edit history and
// view-once opens are erased; the row stays, marked deleted, so the
// conversation shows where it was ("This message was deleted").
func DeleteMessage(c *gin.Context) {
	userID := c.GetString("userID")
	messageID, roomID, ok := messageRoomForMember(c)
	if !ok {
		return
	}

	tx, err := db.DB.Begin()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete message"})
		return
	}
	defer tx.Rollback()

	var senderID string
	var deleted bool
	err = tx.QueryRow(
		`SELECT sender_id, deleted_at IS NOT NULL FROM messages WHERE id = $1 FOR UPDATE`,
		messageID,
	).Scan(&senderID, &deleted)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete message"})
		return
	}
	if senderID != userID {
		c.JSON(http.StatusForbidden, gin.H{"error": "you can only delete your own messages"})
		return
	}
	if deleted {
		c.Status(http.StatusNoContent)
		return
	}

	empty, keyID := sealMessage(roomID, messageID, "")
	if _, err := tx.Exec(
		`UPDATE messages
		 SET deleted_at = NOW(), content = $2, key_id = $3, edited_at = NULL, reply_to_id = NULL,
		     gif_id = NULL, gif_url = NULL, gif_width = NULL, gif_height = NULL, gif_sticker = false, sticker_id = NULL,
		     view_once = false
		 WHERE id = $1`,
		messageID, empty, keyID,
	); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete message"})
		return
	}
	for _, q := range []string{
		`DELETE FROM message_edits WHERE message_id = $1`,
		`DELETE FROM message_reactions WHERE message_id = $1`,
		`DELETE FROM message_opens WHERE message_id = $1`,
	} {
		if _, err := tx.Exec(q, messageID); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete message"})
			return
		}
	}

	rows, err := tx.Query(`DELETE FROM attachments WHERE message_id = $1 RETURNING object_key, poster_key`, messageID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete message"})
		return
	}
	var keys []string
	for rows.Next() {
		var key string
		var posterKey sql.NullString
		if err := rows.Scan(&key, &posterKey); err == nil {
			keys = append(keys, key)
			if posterKey.Valid {
				keys = append(keys, posterKey.String)
			}
		}
	}
	rows.Close()

	if err := tx.Commit(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete message"})
		return
	}

	// the files go once the database no longer points at them
	if storage.Enabled() && len(keys) > 0 {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			for _, key := range keys {
				if err := storage.Delete(ctx, key); err != nil {
					log.Printf("storage: failed to delete %s of a deleted message: %v", key, err)
				}
			}
		}()
	}

	event := mustJSON(gin.H{"type": "message_deleted", "id": messageID, "room_id": roomID})
	WSHub.BroadcastToAll(roomID, event)
	// room lists update their last-message preview
	if memberIDs, err := roomMemberIDs(db.DB, roomID); err == nil {
		WSHub.NotifyUsers(event, memberIDs...)
	}
	c.Status(http.StatusNoContent)
}
