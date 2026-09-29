package workers

import (
	"context"
	"database/sql"
	"log"
	"time"

	"github.com/devstr0ke/chat-mixer/storage"
)

// StartAttachmentCleanup periodically deletes uploads that were never attached
// to a message (abandoned composer drafts), along with their bucket objects.
func StartAttachmentCleanup(database *sql.DB) {
	ticker := time.NewTicker(time.Hour)

	go func() {
		deleteUnsentAttachments(database)
		for range ticker.C {
			deleteUnsentAttachments(database)
		}
	}()

	log.Println("worker: attachment cleanup started (every hour)")
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
