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
		for range ticker.C {
			deleteUnsentAttachments(database)
			deleteStaleLinkPreviews(database)
		}
	}()

	log.Println("worker: attachment cleanup started (every hour)")
}

// deleteStaleLinkPreviews drops cached link cards not refreshed for 30 days.
// A preview is refreshed whenever it's viewed after its 7-day cache expires,
// so these are links nobody has scrolled past in a while; they're simply
// fetched again if someone does.
func deleteStaleLinkPreviews(database *sql.DB) {
	rows, err := database.Query(
		`DELETE FROM link_previews
		 WHERE fetched_at < NOW() - INTERVAL '30 days'
		 RETURNING image_id`,
	)
	if err != nil {
		log.Printf("worker: link preview cleanup query failed: %v", err)
		return
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

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	for _, id := range imageIDs {
		if err := storage.Delete(ctx, "link-previews/"+id); err != nil {
			log.Printf("worker: failed to delete link preview image %s: %v", id, err)
		}
	}
	if removed > 0 {
		log.Printf("worker: removed %d stale link previews", removed)
	}
}

func deleteUnsentAttachments(database *sql.DB) {
	rows, err := database.Query(
		`DELETE FROM attachments
		 WHERE message_id IS NULL AND created_at < NOW() - INTERVAL '24 hours'
		 RETURNING object_key`,
	)
	if err != nil {
		log.Printf("worker: attachment cleanup query failed: %v", err)
		return
	}

	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err == nil {
			keys = append(keys, key)
		}
	}
	rows.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	for _, key := range keys {
		if err := storage.Delete(ctx, key); err != nil {
			log.Printf("worker: failed to delete object %s: %v", key, err)
		}
	}
	if len(keys) > 0 {
		log.Printf("worker: removed %d unsent attachments", len(keys))
	}
}
