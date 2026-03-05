package db

import (
	"database/sql"
	"fmt"
	"log"

	_ "github.com/lib/pq"
)

// DB is the global database connection pool.
var DB *sql.DB

// Connect opens a connection to PostgreSQL and verifies it with a ping.
func Connect(databaseURL string) {
	var err error
	DB, err = sql.Open("postgres", databaseURL)
	if err != nil {
		log.Fatalf("failed to open database: %v", err)
	}

	if err = DB.Ping(); err != nil {
		log.Fatalf("failed to ping database: %v", err)
	}

	fmt.Println("✅ Connected to PostgreSQL")
}

// Migrate creates all tables if they don't already exist.
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
		sent_at   TIMESTAMP NOT NULL DEFAULT NOW()
	);
	`

	_, err := DB.Exec(query)
	if err != nil {
		log.Fatalf("failed to run migrations: %v", err)
	}

	fmt.Println("✅ Database migrated")
}
