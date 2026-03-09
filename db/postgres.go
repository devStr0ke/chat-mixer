package db

import (
	"database/sql"
	"log"

	_ "github.com/lib/pq"
)

var DB *sql.DB

func Connect(databaseURL string) {
	var err error
	DB, err = sql.Open("postgres", databaseURL)
	if err != nil {
		log.Fatalf("db: failed to open: %v", err)
	}
	if err = DB.Ping(); err != nil {
		log.Fatalf("db: failed to ping: %v", err)
	}
	log.Println("db: connected")
}

func Migrate() {
	query := `
	CREATE EXTENSION IF NOT EXISTS "uuid-ossp";

	CREATE TABLE IF NOT EXISTS users (
		id         UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
		pseudo     VARCHAR UNIQUE NOT NULL,
		email      VARCHAR UNIQUE NOT NULL,
		country    VARCHAR(2) NOT NULL,
		password   TEXT NOT NULL,
		created_at TIMESTAMP NOT NULL DEFAULT NOW()
	);

	CREATE TABLE IF NOT EXISTS rooms (
		id         UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
		user_a_id  UUID NOT NULL REFERENCES users(id),
		user_b_id  UUID NOT NULL REFERENCES users(id),
		created_at TIMESTAMP NOT NULL DEFAULT NOW(),
		expires_at TIMESTAMP NOT NULL,
		is_active  BOOLEAN NOT NULL DEFAULT true
	);

	CREATE TABLE IF NOT EXISTS messages (
		id        UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
		room_id   UUID NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
		sender_id UUID NOT NULL REFERENCES users(id),
		content   TEXT NOT NULL,
		is_read   BOOLEAN NOT NULL DEFAULT false,
		sent_at   TIMESTAMP NOT NULL DEFAULT NOW()
	);
	`
	if _, err := DB.Exec(query); err != nil {
		log.Fatalf("db: migration failed: %v", err)
	}

	migrations := []string{
		`ALTER TABLE rooms ADD COLUMN IF NOT EXISTS user_a_room_name VARCHAR(64) NOT NULL DEFAULT 'Stranger'`,
		`ALTER TABLE rooms ADD COLUMN IF NOT EXISTS user_b_room_name VARCHAR(64) NOT NULL DEFAULT 'Stranger'`,
		`ALTER TABLE messages ADD COLUMN IF NOT EXISTS is_read BOOLEAN NOT NULL DEFAULT false`,
	}
	for _, m := range migrations {
		if _, err := DB.Exec(m); err != nil {
			log.Fatalf("db: migration failed: %v", err)
		}
	}

	log.Println("db: migrated")
}
