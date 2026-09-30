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

// upgradeLegacyRooms converts the old 1:1 ephemeral schema (user_a_id / user_b_id /
// expires_at) into group rooms: closed rooms are dropped, active ones keep their
// history and become two-member groups owned by user A.
const upgradeLegacyRooms = `
DO $$
BEGIN
	IF EXISTS (
		SELECT 1 FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = 'rooms' AND column_name = 'user_a_id'
	) THEN
		DELETE FROM rooms WHERE is_active = false;

		ALTER TABLE rooms
			ADD COLUMN name     VARCHAR(64),
			ADD COLUMN owner_id UUID REFERENCES users(id);

		UPDATE rooms r
		SET owner_id = r.user_a_id,
		    name     = LEFT(ua.pseudo || ' & ' || ub.pseudo, 64)
		FROM users ua, users ub
		WHERE ua.id = r.user_a_id AND ub.id = r.user_b_id;

		INSERT INTO room_members (room_id, user_id, joined_at)
			SELECT id, user_a_id, created_at FROM rooms
			UNION ALL
			SELECT id, user_b_id, created_at FROM rooms
		ON CONFLICT DO NOTHING;

		ALTER TABLE rooms
			ALTER COLUMN name SET NOT NULL,
			ALTER COLUMN owner_id SET NOT NULL,
			DROP COLUMN user_a_id,
			DROP COLUMN user_b_id,
			DROP COLUMN expires_at,
			DROP COLUMN is_active,
			DROP COLUMN IF EXISTS user_a_room_name,
			DROP COLUMN IF EXISTS user_b_room_name;

		RAISE NOTICE 'converted legacy 1:1 rooms to group rooms';
	END IF;
END $$;
`

func Migrate() {
	migrations := []string{
		`CREATE EXTENSION IF NOT EXISTS "uuid-ossp"`,
		`CREATE TABLE IF NOT EXISTS users (
			id         UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
			pseudo     VARCHAR UNIQUE NOT NULL,
			email      VARCHAR UNIQUE NOT NULL,
			country    VARCHAR(2) NOT NULL,
			password   TEXT NOT NULL,
			created_at TIMESTAMP NOT NULL DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS rooms (
			id         UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
			name       VARCHAR(64) NOT NULL,
			owner_id   UUID NOT NULL REFERENCES users(id),
			created_at TIMESTAMP NOT NULL DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS room_members (
			room_id      UUID NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
			user_id      UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			joined_at    TIMESTAMP NOT NULL DEFAULT NOW(),
			last_read_at TIMESTAMP NOT NULL DEFAULT NOW(),
			PRIMARY KEY (room_id, user_id)
		)`,
		`CREATE TABLE IF NOT EXISTS room_invitations (
			id         UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
			room_id    UUID NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
			inviter_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			invitee_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			created_at TIMESTAMP NOT NULL DEFAULT NOW(),
			UNIQUE (room_id, invitee_id)
		)`,
		`CREATE TABLE IF NOT EXISTS messages (
			id        UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
			room_id   UUID NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
			sender_id UUID NOT NULL REFERENCES users(id),
			content   TEXT NOT NULL,
			sent_at   TIMESTAMP NOT NULL DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS message_reactions (
			id         UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
			message_id UUID NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
			user_id    UUID NOT NULL REFERENCES users(id),
			emoji      VARCHAR(32) NOT NULL,
			created_at TIMESTAMP NOT NULL DEFAULT NOW(),
			UNIQUE (message_id, user_id)
		)`,
		`CREATE TABLE IF NOT EXISTS attachments (
			id           UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
			room_id      UUID NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
			uploader_id  UUID NOT NULL REFERENCES users(id),
			message_id   UUID REFERENCES messages(id) ON DELETE CASCADE,
			object_key   TEXT NOT NULL,
			content_type VARCHAR(64) NOT NULL,
			size_bytes   BIGINT NOT NULL,
			width        INT NOT NULL,
			height       INT NOT NULL,
			position     INT NOT NULL DEFAULT 0,
			created_at   TIMESTAMP NOT NULL DEFAULT NOW()
		)`,
		upgradeLegacyRooms,
		`ALTER TABLE users ADD COLUMN IF NOT EXISTS country2 VARCHAR(2)`,
		`ALTER TABLE users ADD COLUMN IF NOT EXISTS avatar_id UUID`,
		`ALTER TABLE messages
			ADD COLUMN IF NOT EXISTS gif_id     VARCHAR(64),
			ADD COLUMN IF NOT EXISTS gif_url    TEXT,
			ADD COLUMN IF NOT EXISTS gif_width  INT,
			ADD COLUMN IF NOT EXISTS gif_height INT`,
		`ALTER TABLE messages ADD COLUMN IF NOT EXISTS reply_to_id UUID REFERENCES messages(id) ON DELETE SET NULL`,
		`ALTER TABLE messages ADD COLUMN IF NOT EXISTS edited_at TIMESTAMP`,
		// previous versions of edited messages; written_at is when that version was typed
		`CREATE TABLE IF NOT EXISTS message_edits (
			id         UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
			message_id UUID NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
			content    TEXT NOT NULL,
			written_at TIMESTAMP NOT NULL,
			created_at TIMESTAMP NOT NULL DEFAULT NOW()
		)`,
		`CREATE INDEX IF NOT EXISTS idx_message_edits_message ON message_edits (message_id, created_at)`,
		`ALTER TABLE rooms
			ADD COLUMN IF NOT EXISTS bg_color           VARCHAR(7),
			ADD COLUMN IF NOT EXISTS bg_image_id        UUID,
			ADD COLUMN IF NOT EXISTS bubble_own_color   VARCHAR(7),
			ADD COLUMN IF NOT EXISTS bubble_other_color VARCHAR(7)`,
		// read state now lives in room_members.last_read_at
		`ALTER TABLE messages DROP COLUMN IF EXISTS is_read`,
		`CREATE INDEX IF NOT EXISTS idx_room_members_user ON room_members (user_id)`,
		`CREATE INDEX IF NOT EXISTS idx_room_invitations_invitee ON room_invitations (invitee_id)`,
		`CREATE INDEX IF NOT EXISTS idx_messages_room_sent ON messages (room_id, sent_at DESC, id DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_attachments_message ON attachments (message_id)`,
		`CREATE INDEX IF NOT EXISTS idx_attachments_unsent ON attachments (created_at) WHERE message_id IS NULL`,
	}
	for _, m := range migrations {
		if _, err := DB.Exec(m); err != nil {
			log.Fatalf("db: migration failed: %v", err)
		}
	}

	log.Println("db: migrated")
}
