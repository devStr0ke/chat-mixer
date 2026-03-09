package workers

import (
	"database/sql"
	"log"
	"time"
)

func StartExpirationWorker(database *sql.DB, closeRoomFn func(roomID string), notifyRoomClosedFn func(roomID string, userIDs ...string)) {
	ticker := time.NewTicker(5 * time.Minute)

	go func() {
		for range ticker.C {
			expireRooms(database, closeRoomFn, notifyRoomClosedFn)
		}
	}()

	log.Println("worker: expiration started (every 5 min)")
}

func expireRooms(database *sql.DB, closeRoomFn func(roomID string), notifyRoomClosedFn func(roomID string, userIDs ...string)) {
	rows, err := database.Query(
		`SELECT id, user_a_id, user_b_id FROM rooms WHERE is_active = true AND expires_at < NOW()`,
	)
	if err != nil {
		log.Printf("worker: query failed: %v", err)
		return
	}
	defer rows.Close()

	type expiredRoom struct {
		ID      string
		UserAID string
		UserBID string
	}

	var expired []expiredRoom
	for rows.Next() {
		var r expiredRoom
		if err := rows.Scan(&r.ID, &r.UserAID, &r.UserBID); err != nil {
			log.Printf("worker: scan failed: %v", err)
			continue
		}
		expired = append(expired, r)
	}

	for _, r := range expired {
		if _, err := database.Exec(`DELETE FROM messages WHERE room_id = $1`, r.ID); err != nil {
			log.Printf("worker: failed to delete messages for room %s: %v", r.ID, err)
			continue
		}
		if _, err := database.Exec(`UPDATE rooms SET is_active = false WHERE id = $1`, r.ID); err != nil {
			log.Printf("worker: failed to deactivate room %s: %v", r.ID, err)
			continue
		}
		closeRoomFn(r.ID)
		notifyRoomClosedFn(r.ID, r.UserAID, r.UserBID)
		log.Printf("worker: expired room %s", r.ID)
	}
}
