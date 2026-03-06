package models

import "time"

type Message struct {
	ID       string    `json:"id"`
	RoomID   string    `json:"room_id"`
	SenderID string    `json:"sender_id"`
	Content  string    `json:"content"`
	SentAt   time.Time `json:"sent_at"`
}
