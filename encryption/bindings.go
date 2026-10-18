package encryption

// What each kind of ciphertext is bound to (its associated data), shared by
// the handlers and the startup sweep so both agree.

// MessageAAD ties a message's text to its room and id: copied onto another
// message or into another room, it no longer decrypts. Its edit history uses
// the same binding, so old versions are copied over as they are.
func MessageAAD(roomID, messageID string) string {
	return "message/" + roomID + "/" + messageID
}

// LinkPreviewKey is the cache key of a page's link card. It's keyed, so the
// cache can't be checked for a guessed URL without the encryption key.
func LinkPreviewKey(pageURL string) string {
	return Hash("link-preview", pageURL)
}

// LinkPreviewAAD binds a cached card's encrypted fields to its cache entry.
func LinkPreviewAAD(key string) string {
	return "link-preview/" + key
}
