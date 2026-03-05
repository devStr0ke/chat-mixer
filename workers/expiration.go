package workers

import (
	"database/sql"
	"log"
	"time"
)

// StartExpirationWorker launches a background goroutine that runs every 5 minutes.
// It deactivates expired rooms, deletes their messages, and closes live WebSocket connections.
func StartExpirationWorker(database *sql.DB, closeRoomFn func(roomID string)) {
	ticker := time.NewTicker(5 * time.Minute)

	go func() {
		for range ticker.C {
			expireRooms(database, closeRoomFn)
		}
	}()

	log.Println("Expiration worker started (every 5 min)")
}

func expireRooms(database *sql.DB, closeRoomFn func(roomID string)) {
	rows, err := database.Query(
		`SELECT id FROM rooms WHERE is_active = true AND expires_at < NOW()`,
	)
	if err != nil {
		log.Printf("expiration worker: query failed: %v", err)
		return
	}
	defer rows.Close()

	var roomIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			log.Printf("expiration worker: scan failed: %v", err)
			continue
		}
		roomIDs = append(roomIDs, id)
	}

	for _, roomID := range roomIDs {
		// Delete messages first (cascade would handle it, but be explicit)
		_, err := database.Exec(`DELETE FROM messages WHERE room_id = $1`, roomID)
		if err != nil {
			log.Printf("expiration worker: failed to delete messages for room %s: %v", roomID, err)
			continue
		}

		// Deactivate room
		_, err = database.Exec(`UPDATE rooms SET is_active = false WHERE id = $1`, roomID)
		if err != nil {
			log.Printf("expiration worker: failed to deactivate room %s: %v", roomID, err)
			continue
		}

		// Close any active WebSocket connections
		closeRoomFn(roomID)

		log.Printf("Expired room %s", roomID)
	}
}
