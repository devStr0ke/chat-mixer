package handlers

import (
	"net/url"
	"testing"
)

func TestShortVideoLinks(t *testing.T) {
	cases := map[string]bool{
		"https://vm.tiktok.com/ZMabcdefg/":                       true,
		"https://vt.tiktok.com/ZSabcdefg/":                       true,
		"https://www.tiktok.com/t/ZTabcdefg/":                    true,
		"https://www.instagram.com/share/reel/BAabcdefg":         true,
		"https://www.tiktok.com/@someone/video/7300000000000000": false,
		"https://www.instagram.com/reel/C1abcdefg/":              false,
		"https://youtu.be/dQw4w9WgXcQ":                           false,
		"https://evil.example/t/abc":                             false,
		"https://tiktok.com.evil.example/t/abc":                  false,
	}
	for raw, want := range cases {
		u, _ := url.Parse(raw)
		if got := isShortVideoLink(u); got != want {
			t.Errorf("isShortVideoLink(%s) = %v, want %v", raw, got, want)
		}
	}
}

func TestVideoHopsStayOnTheSites(t *testing.T) {
	for raw, want := range map[string]bool{
		"https://www.tiktok.com/@a/video/1":   true,
		"https://vm.tiktok.com/x":             true,
		"https://www.instagram.com/reel/abc/": true,
		"https://example.com/":                false,
		"https://tiktok.com.example.com/":     false,
	} {
		u, _ := url.Parse(raw)
		if got := allowedVideoHop(u); got != want {
			t.Errorf("allowedVideoHop(%s) = %v, want %v", raw, got, want)
		}
	}
}
