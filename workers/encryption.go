package workers

import (
	"context"
	"database/sql"
	"log"
	"time"

	"github.com/devstr0ke/chat-mixer/encryption"
	"github.com/devstr0ke/chat-mixer/storage"
)

const (
	sweepBatch = 500
	// a second pass catches what the previous version still wrote while a
	// rolling deploy had both running
	sweepRepeatDelay = 5 * time.Minute
)

// EncryptRows encrypts message text and edit history stored before
// encryption was enabled, re-encrypts what was sealed with a retired key, and
// drops cached link cards in the same situation (they're fetched again when
// next needed). Rewriting a row leaves its old version behind until Postgres
// reuses the space, so the tables are then compacted with VACUUM FULL, which
// removes those old versions from the live database files.
func EncryptRows(database *sql.DB) {
	messages := sweepMessages(database)
	edits := sweepMessageEdits(database)
	previews := deleteLinkPreviews(database, `key_id IS DISTINCT FROM $1`, encryption.CurrentKeyID())
	if messages+edits+previews == 0 {
		return
	}
	log.Printf("encryption: encrypted %d messages and %d earlier versions, dropped %d cached link previews",
		messages, edits, previews)
	for _, table := range []string{"messages", "message_edits", "link_previews"} {
		if _, err := database.Exec(`VACUUM FULL ` + table); err != nil {
			log.Printf("encryption: VACUUM FULL %s failed: %v", table, err)
		}
	}
}

// StartEncryptionSweep encrypts the bucket's existing files in the
// background, then repeats both sweeps once after a rolling deploy settles.
func StartEncryptionSweep(database *sql.DB) {
	go func() {
		encryptObjects()
		time.Sleep(sweepRepeatDelay)
		EncryptRows(database)
		encryptObjects()
	}()
}

func encryptObjects() {
	if !storage.Enabled() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	n, err := storage.EncryptExisting(ctx)
	if err != nil {
		log.Printf("encryption: file sweep stopped: %v", err)
	}
	if n > 0 {
		log.Printf("encryption: encrypted %d existing files", n)
	}
}

type storedText struct {
	id, roomID, messageID, content string
	keyID                          sql.NullString
}

// reseal rewrites rows (with the given UPDATE, taking content, key_id, id and
// the old key_id) under the current key and returns how many changed. The
// old key_id guard skips rows edited in the meantime.
func reseal(database *sql.DB, update string, rows []storedText) int {
	done := 0
	for _, r := range rows {
		aad := encryption.MessageAAD(r.roomID, r.messageID)
		text, err := encryption.Open(r.content, r.keyID.String, aad)
		if err != nil {
			log.Printf("encryption: skipping %s: %v", r.id, err)
			continue
		}
		sealed, keyID := encryption.Seal(text, aad)
		res, err := database.Exec(update, sealed, keyID, r.id, r.keyID)
		if err != nil {
			log.Printf("encryption: failed to rewrite %s: %v", r.id, err)
			continue
		}
		if n, _ := res.RowsAffected(); n == 1 {
			done++
		}
	}
	return done
}

// sweep pages through the rows not sealed with the current key (by id, so
// rows that can't be resealed don't come back) and reseals them.
func sweep(database *sql.DB, query, update string) int {
	after, done := "00000000-0000-0000-0000-000000000000", 0
	for {
		rows, err := database.Query(query, encryption.CurrentKeyID(), after, sweepBatch)
		if err != nil {
			log.Printf("encryption: sweep query failed: %v", err)
			return done
		}
		var batch []storedText
		for rows.Next() {
			var r storedText
			if err := rows.Scan(&r.id, &r.roomID, &r.messageID, &r.content, &r.keyID); err != nil {
				log.Printf("encryption: sweep scan failed: %v", err)
				break
			}
			batch = append(batch, r)
		}
		rows.Close()
		if len(batch) == 0 {
			return done
		}
		after = batch[len(batch)-1].id
		done += reseal(database, update, batch)
	}
}

func sweepMessages(database *sql.DB) int {
	return sweep(database,
		`SELECT id, room_id, id, content, key_id FROM messages
		 WHERE key_id IS DISTINCT FROM $1 AND id > $2
		 ORDER BY id LIMIT $3`,
		`UPDATE messages SET content = $1, key_id = $2 WHERE id = $3 AND key_id IS NOT DISTINCT FROM $4`)
}

func sweepMessageEdits(database *sql.DB) int {
	return sweep(database,
		`SELECT e.id, m.room_id, e.message_id, e.content, e.key_id
		 FROM message_edits e JOIN messages m ON m.id = e.message_id
		 WHERE e.key_id IS DISTINCT FROM $1 AND e.id > $2
		 ORDER BY e.id LIMIT $3`,
		`UPDATE message_edits SET content = $1, key_id = $2 WHERE id = $3 AND key_id IS NOT DISTINCT FROM $4`)
}
