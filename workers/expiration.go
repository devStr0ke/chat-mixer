package workers

import (
	"database/sql"
	"log"
	"time"
)

func StartExpirationWorker(database *sql.DB, closeRoomFn func(roomID string)) {
	ticker := time.NewTicker(5 * time.Minute)

	go func() {
		for range ticker.C {
			expireRooms(database, closeRoomFn)
		}
	}()

	log.Println("worker: expiration started (every 5 min)")
}

func expireRooms(database *sql.DB, closeRoomFn func(roomID string)) {
	rows, err := database.Query(
		`SELECT id FROM rooms WHERE is_active = true AND expires_at < NOW()`,
	)
	if err != nil {
		log.Printf("worker: query failed: %v", err)
		return
	}
	defer rows.Close()

	var roomIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			log.Printf("worker: scan failed: %v", err)
			continue
		}
		roomIDs = append(roomIDs, id)
	}

	for _, roomID := range roomIDs {
		if _, err := database.Exec(`DELETE FROM messages WHERE room_id = $1`, roomID); err != nil {
			log.Printf("worker: failed to delete messages for room %s: %v", roomID, err)
			continue
		}
		if _, err := database.Exec(`UPDATE rooms SET is_active = false WHERE id = $1`, roomID); err != nil {
			log.Printf("worker: failed to deactivate room %s: %v", roomID, err)
			continue
		}
		closeRoomFn(roomID)
		log.Printf("worker: expired room %s", roomID)
	}
}
