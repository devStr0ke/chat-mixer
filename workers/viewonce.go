package workers

import (
	"context"
	"database/sql"
	"log"
	"time"

	"github.com/devstr0ke/chat-mixer/storage"
)

// ViewWindowSQL is how long (seconds) a view-once attachment `a` stays
// readable after someone opens it: two minutes for a photo (time to load
// it), a video's length plus two minutes (15 minutes at most).
const ViewWindowSQL = `CASE WHEN a.content_type LIKE 'video/%' THEN LEAST(900, COALESCE(a.duration_ms, 0) / 1000 + 120) ELSE 120 END`

// PurgeViewOnce deletes the files of view-once messages nobody can open any
// more: everyone else in the room has opened them and the last viewing
// window has closed, or they were sent over 14 days ago. The message stays,
// shown as expired.
func PurgeViewOnce(database *sql.DB) {
	rows, err := database.Query(
		`UPDATE attachments a SET purged_at = NOW()
		 FROM messages m
		 WHERE a.message_id = m.id AND m.view_once AND a.purged_at IS NULL
		   AND (
		     m.sent_at < NOW() - INTERVAL '14 days'
		     OR (
		       EXISTS (SELECT 1 FROM message_opens o WHERE o.message_id = m.id)
		       AND NOT EXISTS (
		         SELECT 1 FROM room_members rm
		         WHERE rm.room_id = m.room_id AND rm.user_id <> m.sender_id
		           AND NOT EXISTS (SELECT 1 FROM message_opens o WHERE o.message_id = m.id AND o.user_id = rm.user_id)
		       )
		       AND NOT EXISTS (
		         SELECT 1 FROM message_opens o
		         WHERE o.message_id = m.id AND EXTRACT(EPOCH FROM NOW() - o.opened_at) < ` + ViewWindowSQL + `
		       )
		     )
		   )
		 RETURNING a.object_key, a.poster_key, a.overlay_key`,
	)
	if err != nil {
		log.Printf("worker: view-once purge query failed: %v", err)
		return
	}

	keys, purged := AttachmentKeys(rows)
	rows.Close()

	if !storage.Enabled() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	for _, key := range keys {
		if err := storage.Delete(ctx, key); err != nil {
			log.Printf("worker: failed to delete view-once file %s: %v", key, err)
		}
	}
	if purged > 0 {
		log.Printf("worker: deleted %d opened or expired view-once files", purged)
	}
}

// AttachmentKeys reads rows of (object_key, poster_key, overlay_key) into the
// bucket keys to delete, and how many attachments they came from.
func AttachmentKeys(rows *sql.Rows) (keys []string, count int) {
	for rows.Next() {
		var key string
		var poster, overlay sql.NullString
		if err := rows.Scan(&key, &poster, &overlay); err != nil {
			continue
		}
		count++
		keys = append(keys, key)
		for _, k := range []sql.NullString{poster, overlay} {
			if k.Valid {
				keys = append(keys, k.String)
			}
		}
	}
	return keys, count
}
