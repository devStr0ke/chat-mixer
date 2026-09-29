package models

import "time"

type Room struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	OwnerID   string    `json:"owner_id"`
	CreatedAt time.Time `json:"created_at"`
}

type RoomMember struct {
	RoomID     string    `json:"room_id"`
	UserID     string    `json:"user_id"`
	JoinedAt   time.Time `json:"joined_at"`
	LastReadAt time.Time `json:"last_read_at"`
}

type RoomInvitation struct {
	ID        string    `json:"id"`
	RoomID    string    `json:"room_id"`
	InviterID string    `json:"inviter_id"`
	InviteeID string    `json:"invitee_id"`
	CreatedAt time.Time `json:"created_at"`
}
