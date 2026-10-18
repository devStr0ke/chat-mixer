package handlers

import (
	"database/sql"
	"log"

	"github.com/devstr0ke/chat-mixer/encryption"
)

// undecryptable stands in for a message whose ciphertext won't open (its key
// was removed from the config, or the row was tampered with).
const undecryptable = "🔒 This message can't be decrypted."

// sealMessage encrypts message text for storage; it returns the ciphertext
// and the key id to store next to it.
func sealMessage(roomID, messageID, text string) (string, string) {
	return encryption.Seal(text, encryption.MessageAAD(roomID, messageID))
}

// openMessage decrypts stored message text. A failure is logged and shown as
// a placeholder rather than failing the whole page.
func openMessage(roomID, messageID, stored string, keyID sql.NullString) string {
	text, err := encryption.Open(stored, keyID.String, encryption.MessageAAD(roomID, messageID))
	if err != nil {
		log.Printf("encryption: message %s: %v", messageID, err)
		return undecryptable
	}
	return text
}

// truncateRunes cuts s to at most n characters.
func truncateRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}
