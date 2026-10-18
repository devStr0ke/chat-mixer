package encryption

import (
	"strings"
	"testing"
)

const (
	keyA = "0000000000000000000000000000000000000000000000000000000000000001"
	keyB = "0000000000000000000000000000000000000000000000000000000000000002"
)

func mustLoad(t *testing.T, cur, old string) {
	t.Helper()
	if err := load(cur, old); err != nil {
		t.Fatal(err)
	}
}

func TestSealOpen(t *testing.T) {
	mustLoad(t, keyA, "")
	sealed, id := Seal("hello 👋", "message/room/1")
	if sealed == "" || strings.Contains(sealed, "hello") || id != CurrentKeyID() {
		t.Fatalf("sealed=%q id=%q", sealed, id)
	}
	if again, _ := Seal("hello 👋", "message/room/1"); again == sealed {
		t.Error("same text sealed twice gives the same ciphertext (nonce reuse)")
	}
	if got, err := Open(sealed, id, "message/room/1"); err != nil || got != "hello 👋" {
		t.Errorf("open = %q, %v", got, err)
	}
	if _, err := Open(sealed, id, "message/room/2"); err != ErrCorrupt {
		t.Errorf("moved ciphertext opened: %v", err)
	}
	if _, err := Open(sealed[:len(sealed)-2]+"AA", id, "message/room/1"); err != ErrCorrupt {
		t.Errorf("tampered ciphertext opened: %v", err)
	}
	if _, err := Open(sealed, "deadbeef", "message/room/1"); err != ErrUnknownKey {
		t.Errorf("unknown key id: %v", err)
	}
}

func TestEmptyAndLegacy(t *testing.T) {
	mustLoad(t, keyA, "")
	if sealed, id := Seal("", "x"); sealed != "" || id != CurrentKeyID() {
		t.Errorf("empty text: %q %q", sealed, id)
	}
	if got, err := Open("", CurrentKeyID(), "x"); err != nil || got != "" {
		t.Errorf("open empty: %q %v", got, err)
	}
	if got, err := Open("written before encryption", "", "x"); err != nil || got != "written before encryption" {
		t.Errorf("legacy row: %q %v", got, err)
	}
}

func TestRotation(t *testing.T) {
	mustLoad(t, keyA, "")
	sealed, oldID := Seal("secret", "aad")
	oldFile, _ := FileKeys()
	oldHash := Hash("p", "v")

	mustLoad(t, keyB, keyA)
	if CurrentKeyID() == oldID {
		t.Fatal("key id didn't change with the key")
	}
	if got, err := Open(sealed, oldID, "aad"); err != nil || got != "secret" {
		t.Errorf("old ciphertext after rotation: %q %v", got, err)
	}
	cur, retiredKeys := FileKeys()
	if len(retiredKeys) != 1 || string(retiredKeys[0]) != string(oldFile) || string(cur) == string(oldFile) {
		t.Error("file keys not rotated")
	}
	if Hash("p", "v") == oldHash {
		t.Error("hash didn't change with the key")
	}

	mustLoad(t, keyB, "")
	if _, err := Open(sealed, oldID, "aad"); err != ErrUnknownKey {
		t.Errorf("dropped key still opens: %v", err)
	}
}

func TestDerivedKeysDiffer(t *testing.T) {
	mustLoad(t, keyA, "")
	file, _ := FileKeys()
	if string(file) == string(current.index) {
		t.Error("file and index keys are the same")
	}
	if Hash("a", "bc") == Hash("ab", "c") {
		t.Error("purpose and value aren't separated in the hash")
	}
}

func TestLoadValidation(t *testing.T) {
	for _, bad := range []string{"", "  ", "abc", strings.Repeat("z", 64), keyA[:62]} {
		if err := load(bad, ""); err == nil {
			t.Errorf("accepted key %q", bad)
		}
	}
	if err := load(keyA, "nope"); err == nil {
		t.Error("accepted a bad old key")
	}
	if err := load(keyA, " "+keyB+" , "); err != nil {
		t.Errorf("old keys with spaces: %v", err)
	}
}
