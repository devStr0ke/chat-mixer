package models

import "time"

type Room struct {
	ID        string    `json:"id"`
	UserAID   string    `json:"user_a_id"`
	UserBID   string    `json:"user_b_id"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	IsActive  bool      `json:"is_active"`
}
