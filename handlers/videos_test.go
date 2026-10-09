package handlers

import "testing"

func ftyp(brand string) []byte {
	return append([]byte{0, 0, 0, 0x20, 'f', 't', 'y', 'p'}, []byte(brand+"\x00\x00\x02\x00")...)
}

func TestSniffVideo(t *testing.T) {
	webm := append([]byte{0x1A, 0x45, 0xDF, 0xA3, 0x9F, 0x42, 0x86, 0x81, 0x01, 0x42, 0x82, 0x84}, []byte("webm\x42\x87")...)
	mkv := append([]byte{0x1A, 0x45, 0xDF, 0xA3, 0x9F, 0x42, 0x86, 0x81, 0x01, 0x42, 0x82, 0x88}, []byte("matroska")...)

	cases := []struct {
		name string
		head []byte
		want string
	}{
		{"mp4 (isom)", ftyp("isom"), "video/mp4"},
		{"mp4 (mp42)", ftyp("mp42"), "video/mp4"},
		{"iphone mov", ftyp("qt  "), "video/quicktime"},
		{"webm", webm, "video/webm"},
		{"heic photo shares the container", ftyp("heic"), ""},
		{"avif image", ftyp("avif"), ""},
		{"m4a audio", ftyp("M4A "), ""},
		{"matroska that isn't webm", mkv, ""},
		{"jpeg", []byte{0xFF, 0xD8, 0xFF, 0xE0, 0, 0x10, 'J', 'F', 'I', 'F', 0, 1}, ""},
		{"text", []byte("this is definitely not a video file"), ""},
		{"too short", []byte{0, 0, 0}, ""},
	}
	for _, tc := range cases {
		if got := sniffVideo(tc.head); got != tc.want {
			t.Errorf("%s: sniffVideo = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestServedVideoType(t *testing.T) {
	if servedVideoType("video/quicktime") != "video/mp4" || servedVideoType("video/webm") != "video/webm" || servedVideoType("image/png") != "image/png" {
		t.Error("unexpected served content types")
	}
}
