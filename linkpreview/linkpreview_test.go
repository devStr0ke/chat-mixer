package linkpreview

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestIsPublicIP(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "10.0.0.5", "172.17.0.2", "192.168.1.10", // loopback + private (docker, LAN)
		"169.254.169.254", // cloud metadata endpoint
		"100.64.0.1", "0.0.0.0", "224.0.0.1", "255.255.255.255",
		"::1", "fc00::1", "fe80::1", "::ffff:10.0.0.1", "64:ff9b::a00:1",
	}
	for _, ip := range blocked {
		if IsPublicIP(net.ParseIP(ip)) {
			t.Errorf("%s should be blocked", ip)
		}
	}
	for _, ip := range []string{"8.8.8.8", "1.1.1.1", "140.82.121.4", "2606:4700:4700::1111"} {
		if !IsPublicIP(net.ParseIP(ip)) {
			t.Errorf("%s should be allowed", ip)
		}
	}
	if IsPublicIP(nil) {
		t.Error("nil IP should be blocked")
	}
}

func TestGuardDial(t *testing.T) {
	cases := map[string]bool{
		"8.8.8.8:443":        true,
		"8.8.8.8:80":         true,
		"8.8.8.8:22":         false, // not a web port
		"8.8.8.8:5432":       false,
		"127.0.0.1:80":       false,
		"10.1.2.3:443":       false,
		"169.254.169.254:80": false,
		"[::1]:443":          false,
		"not-an-address":     false,
	}
	for address, allowed := range cases {
		err := guardDial("tcp", address, nil)
		if allowed && err != nil {
			t.Errorf("%s should be allowed, got %v", address, err)
		}
		if !allowed && !errors.Is(err, ErrBlockedAddress) {
			t.Errorf("%s should be blocked, got %v", address, err)
		}
	}
}

func TestNormalizeURL(t *testing.T) {
	ok := map[string]string{
		" https://Example.com/Path?q=1#frag ": "https://example.com/Path?q=1",
		"http://example.com":                  "http://example.com",
		"https://example.com:443/a":           "https://example.com:443/a",
	}
	for in, want := range ok {
		got, err := NormalizeURL(in)
		if err != nil || got != want {
			t.Errorf("NormalizeURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	bad := []string{
		"", "example.com", "ftp://example.com/file", "javascript:alert(1)", "file:///etc/passwd",
		"http://user:pass@example.com", "http://example.com:8080/", "http://example.com:5432",
		"https:///nohost", "https://example.com/" + strings.Repeat("a", 2100),
	}
	for _, in := range bad {
		if got, err := NormalizeURL(in); err == nil {
			t.Errorf("NormalizeURL(%q) should fail, got %q", in, got)
		}
	}
}

func TestParse(t *testing.T) {
	base, _ := url.Parse("https://example.com/articles/42")

	t.Run("open graph wins", func(t *testing.T) {
		m := Parse(strings.NewReader(`<html><head>
			<title>Plain title</title>
			<meta name="description" content="plain description">
			<meta name="twitter:title" content="Twitter title">
			<meta property="og:title" content="  OG   title &amp; more ">
			<meta property="og:description" content="OG description">
			<meta property="og:site_name" content="Example">
			<meta property="og:image" content="/img/cover.jpg">
			<meta property="og:image" content="/img/second.jpg">
		</head><body></body></html>`), base)
		if m.Title != "OG title & more" || m.Description != "OG description" || m.SiteName != "Example" {
			t.Errorf("unexpected metadata: %+v", m)
		}
		if m.ImageURL != "https://example.com/img/cover.jpg" {
			t.Errorf("relative image should resolve against the page, got %q", m.ImageURL)
		}
	})

	t.Run("falls back to twitter then plain html", func(t *testing.T) {
		m := Parse(strings.NewReader(`<head><title>
			Plain   title
		</title><meta name="description" content="plain description">
		<meta name="twitter:image" content="https://cdn.example.com/t.png"></head>`), base)
		if m.Title != "Plain title" || m.Description != "plain description" || m.ImageURL != "https://cdn.example.com/t.png" {
			t.Errorf("unexpected metadata: %+v", m)
		}
	})

	t.Run("reads tags placed in the body, like mobile youtube", func(t *testing.T) {
		m := Parse(strings.NewReader(`<head><title>YouTube</title></head>
			<body><div><meta property="og:title" content="The actual video title">
			<meta property="og:image" content="https://i.ytimg.com/vi/x/hq.jpg"></div></body>`), base)
		if m.Title != "The actual video title" || m.ImageURL != "https://i.ytimg.com/vi/x/hq.jpg" {
			t.Errorf("unexpected metadata: %+v", m)
		}
	})

	t.Run("first occurrence wins, so head beats body", func(t *testing.T) {
		m := Parse(strings.NewReader(`<head><meta property="og:title" content="From head"></head>
			<body><meta property="og:title" content="From body"></body>`), base)
		if m.Title != "From head" {
			t.Errorf("expected the head tag to win, got %q", m.Title)
		}
	})

	t.Run("drops unsafe image urls", func(t *testing.T) {
		m := Parse(strings.NewReader(`<head><title>T</title>
			<meta property="og:image" content="javascript:alert(1)"></head>`), base)
		if m.Title != "T" || m.ImageURL != "" {
			t.Errorf("unexpected metadata: %+v", m)
		}
	})

	t.Run("truncates long text", func(t *testing.T) {
		m := Parse(strings.NewReader(`<head><title>`+strings.Repeat("é", 500)+`</title></head>`), base)
		if n := len([]rune(m.Title)); n != maxTitleRunes || !strings.HasSuffix(m.Title, "…") {
			t.Errorf("title should be cut to %d runes with an ellipsis, got %d", maxTitleRunes, n)
		}
	})

	t.Run("empty document", func(t *testing.T) {
		if m := Parse(strings.NewReader(""), base); m != (Metadata{}) {
			t.Errorf("expected empty metadata, got %+v", m)
		}
	})
}

// A server on this machine stands in for any internal service: the fetcher
// must refuse to talk to it, directly or through a redirect.
func TestFetchRefusesInternalAddresses(t *testing.T) {
	hit := false
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<title>secret admin panel</title>`))
	}))
	defer internal.Close()

	if _, err := Fetch(context.Background(), internal.URL); !errors.Is(err, ErrBlockedAddress) {
		t.Errorf("Fetch of a loopback server should be blocked, got %v", err)
	}
	if _, err := FetchImage(context.Background(), internal.URL, 1<<20); !errors.Is(err, ErrBlockedAddress) {
		t.Errorf("FetchImage of a loopback server should be blocked, got %v", err)
	}
	for _, target := range []string{"http://localhost/", "http://127.0.0.1/", "http://[::1]/", "http://169.254.169.254/latest/meta-data/"} {
		if _, err := Fetch(context.Background(), target); err == nil {
			t.Errorf("Fetch(%s) should fail", target)
		}
	}
	if hit {
		t.Error("the internal server received a request")
	}
}
