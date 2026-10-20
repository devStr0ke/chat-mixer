package workers

import (
	"context"
	"database/sql"
	"log"
	"time"

	"github.com/devstr0ke/chat-mixer/storage"
)

// StartAttachmentCleanup periodically removes bucket objects nobody needs
// anymore: uploads never attached to a message (abandoned composer drafts) and
// link-preview images that haven't been looked at for a month.
func StartAttachmentCleanup(database *sql.DB) {
	ticker := time.NewTicker(time.Hour)

	go func() {
		deleteUnsentAttachments(database)
		deleteStaleLinkPreviews(database)
		PurgeViewOnce(database)
		PurgeRemovedStickers(database)
		for range ticker.C {
			deleteUnsentAttachments(database)
			deleteStaleLinkPreviews(database)
			PurgeViewOnce(database)
			PurgeRemovedStickers(database)
		}
	}()

	log.Println("worker: attachment cleanup started (every hour)")
}

// deleteStaleLinkPreviews drops cached link cards not refreshed for 30 days.
// A preview is refreshed whenever it's viewed after its 7-day cache expires,
// so these are links nobody has scrolled past in a while; they're simply
// fetched again if someone does.
func deleteStaleLinkPreviews(database *sql.DB) {
	if removed := deleteLinkPreviews(database, `fetched_at < NOW() - INTERVAL '30 days'`); removed > 0 {
		log.Printf("worker: removed %d stale link previews", removed)
	}
}

// deleteLinkPreviews drops the cached link cards matching condition, with
// their images, and returns how many were removed.
func deleteLinkPreviews(database *sql.DB, condition string, args ...any) int {
	rows, err := database.Query(`DELETE FROM link_previews WHERE `+condition+` RETURNING image_id`, args...)
	if err != nil {
		log.Printf("worker: link preview cleanup query failed: %v", err)
		return 0
	}

	var imageIDs []string
	removed := 0
	for rows.Next() {
		var imageID sql.NullString
		if err := rows.Scan(&imageID); err == nil {
			removed++
			if imageID.Valid {
				imageIDs = append(imageIDs, imageID.String)
			}
		}
	}
	rows.Close()

	if !storage.Enabled() {
		return removed
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	for _, id := range imageIDs {
		if err := storage.Delete(ctx, "link-previews/"+id); err != nil {
			log.Printf("worker: failed to delete link preview image %s: %v", id, err)
		}
	}
	return removed
}

func deleteUnsentAttachments(database *sql.DB) {
	rows, err := database.Query(
		`DELETE FROM attachments
		 WHERE message_id IS NULL AND created_at < NOW() - INTERVAL '24 hours'
		 RETURNING object_key, poster_key, overlay_key`,
	)
	if err != nil {
		log.Printf("worker: attachment cleanup query failed: %v", err)
		return
	}

	keys, removed := AttachmentKeys(rows)
	rows.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	for _, key := range keys {
		if err := storage.Delete(ctx, key); err != nil {
			log.Printf("worker: failed to delete object %s: %v", key, err)
		}
	}
	if removed > 0 {
		log.Printf("worker: removed %d unsent attachments", removed)
	}
}

// PurgeRemovedStickers deletes stickers removed from their owner's collection,
// and drawings older than a day, once no message uses them (their files go
// with them).
func PurgeRemovedStickers(database *sql.DB) {
	rows, err := database.Query(
		`DELETE FROM stickers s
		 WHERE (s.deleted_at IS NOT NULL OR (s.kind = 'drawing' AND s.created_at < NOW() - INTERVAL '24 hours'))
		   AND NOT EXISTS (SELECT 1 FROM messages m WHERE m.sticker_id = s.id)
		 RETURNING s.object_key`,
	)
	if err != nil {
		log.Printf("worker: sticker cleanup query failed: %v", err)
		return
	}
	var keys []string
	for rows.Next() {
		var key string
		if rows.Scan(&key) == nil {
			keys = append(keys, key)
		}
	}
	rows.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	for _, key := range keys {
		if err := storage.Delete(ctx, key); err != nil {
			log.Printf("worker: failed to delete sticker %s: %v", key, err)
		}
	}
	if len(keys) > 0 {
		log.Printf("worker: deleted %d removed stickers and unsent drawings", len(keys))
	}
}
