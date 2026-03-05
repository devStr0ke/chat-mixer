package models

import "time"

type User struct {
	ID        string    `json:"id"`
	Pseudo    string    `json:"pseudo"`
	Email     string    `json:"email"`
	Country   string    `json:"country"`
	Password  string    `json:"-"`
	CreatedAt time.Time `json:"created_at"`
}
