// Package encryption protects what users write when the database or the
// bucket leaks on its own (a dump, a backup, a stolen volume): message text
// and link previews are sealed with AES-256-GCM before they reach Postgres,
// and files are stored with SSE-C, so Garage only keeps them encrypted.
//
// Every key is derived from ENCRYPTION_KEY (32 random bytes, hex), which
// lives with the API's configuration, never in the database. Each encrypted
// row records the id of the key that sealed it, so the key can be replaced:
// set the new one in ENCRYPTION_KEY, move the old one to ENCRYPTION_OLD_KEYS
// (comma-separated) and the startup sweep re-encrypts everything with the new
// key, after which the old one can be dropped.
package encryption

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
)

var (
	ErrUnknownKey = errors.New("encrypted with a key that isn't configured")
	ErrCorrupt    = errors.New("ciphertext is corrupt or was moved")
)

type keySet struct {
	id    string      // short fingerprint stored next to ciphertexts
	text  cipher.AEAD // message text and link previews
	index []byte      // HMAC key for lookup hashes
	file  []byte      // SSE-C key for bucket objects
}

var (
	current *keySet
	byID    = map[string]*keySet{}
	retired []*keySet
)

func derive(master []byte, purpose string, size int) []byte {
	key, err := hkdf.Key(sha256.New, master, nil, "chat-mixer/"+purpose, size)
	if err != nil {
		panic(err) // only fails for absurd sizes
	}
	return key
}

func newKeySet(hexKey string) (*keySet, error) {
	master, err := hex.DecodeString(strings.TrimSpace(hexKey))
	if err != nil || len(master) != 32 {
		return nil, errors.New("must be 64 hex characters (generate one with: openssl rand -hex 32)")
	}
	block, err := aes.NewCipher(derive(master, "text/v1", 32))
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &keySet{
		id:    hex.EncodeToString(derive(master, "key-id/v1", 4)),
		text:  aead,
		index: derive(master, "index/v1", 32),
		file:  derive(master, "file/v1", 32),
	}, nil
}

// Load reads ENCRYPTION_KEY and ENCRYPTION_OLD_KEYS.
func Load() error {
	return load(os.Getenv("ENCRYPTION_KEY"), os.Getenv("ENCRYPTION_OLD_KEYS"))
}

func load(currentKey, oldKeys string) error {
	if strings.TrimSpace(currentKey) == "" {
		return errors.New("ENCRYPTION_KEY is not set (generate one with: openssl rand -hex 32)")
	}
	ks, err := newKeySet(currentKey)
	if err != nil {
		return fmt.Errorf("ENCRYPTION_KEY %w", err)
	}
	current, byID, retired = ks, map[string]*keySet{ks.id: ks}, nil
	for _, raw := range strings.Split(oldKeys, ",") {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		old, err := newKeySet(raw)
		if err != nil {
			return fmt.Errorf("ENCRYPTION_OLD_KEYS: a key %w", err)
		}
		if _, dup := byID[old.id]; !dup {
			byID[old.id] = old
			retired = append(retired, old)
		}
	}
	return nil
}

// CurrentKeyID is the id recorded next to everything sealed now.
func CurrentKeyID() string {
	return current.id
}

// Seal encrypts text with the current key. aad binds the ciphertext to where
// it's stored (e.g. a message's room and id): copied anywhere else, it no
// longer decrypts. Empty text stays empty.
func Seal(text, aad string) (sealed, keyID string) {
	if text == "" {
		return "", current.id
	}
	nonce := make([]byte, current.text.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		panic(err) // the OS random source never fails on supported platforms
	}
	out := current.text.Seal(nonce, nonce, []byte(text), []byte(aad))
	return base64.RawStdEncoding.EncodeToString(out), current.id
}

// Open decrypts what Seal returned. An empty keyID means the value was
// stored before encryption was enabled and is returned as is.
func Open(sealed, keyID, aad string) (string, error) {
	if keyID == "" || sealed == "" {
		return sealed, nil
	}
	ks := byID[keyID]
	if ks == nil {
		return "", ErrUnknownKey
	}
	raw, err := base64.RawStdEncoding.DecodeString(sealed)
	if err != nil || len(raw) < ks.text.NonceSize() {
		return "", ErrCorrupt
	}
	n := ks.text.NonceSize()
	text, err := ks.text.Open(nil, raw[:n], raw[n:], []byte(aad))
	if err != nil {
		return "", ErrCorrupt
	}
	return string(text), nil
}

// Hash is a keyed hash for looking values up without storing them, e.g. the
// link preview cache key. Unlike a plain SHA-256, nobody without the key can
// test a guess against it.
func Hash(purpose, value string) string {
	mac := hmac.New(sha256.New, current.index)
	mac.Write([]byte(purpose + "\x00" + value))
	return hex.EncodeToString(mac.Sum(nil))
}

// FileKeys returns the SSE-C key for new objects and the retired ones that
// older objects may still be encrypted with.
func FileKeys() (currentKey []byte, retiredKeys [][]byte) {
	for _, ks := range retired {
		retiredKeys = append(retiredKeys, ks.file)
	}
	return current.file, retiredKeys
}
