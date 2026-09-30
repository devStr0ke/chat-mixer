// Package linkpreview fetches a web page on behalf of users and extracts what
// a link card needs (title, description, image).
//
// Fetching URLs chosen by users is an SSRF risk, so every connection goes
// through a dialer that only allows public IP addresses on ports 80/443. The
// check runs on the address actually being connected to, after DNS resolution
// and on every redirect hop, so hostnames that resolve to internal addresses
// (or change between checks) are refused too.
package linkpreview

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"golang.org/x/net/html"
	"golang.org/x/net/html/charset"
)

const (
	maxURLLength   = 2000
	maxHTMLBytes   = 1 << 20
	maxRedirects   = 5
	maxTitleRunes  = 200
	maxDescRunes   = 300
	requestTimeout = 10 * time.Second
	// Names itself, and (like Telegram's "TelegramBot (like TwitterBot)") says
	// which crawlers it behaves like: many sites only send preview tags to those.
	userAgent = "Mozilla/5.0 (compatible; ChatMixerBot/1.0; like TwitterBot; like facebookexternalhit)"
)

var (
	ErrInvalidURL     = errors.New("invalid url")
	ErrBlockedAddress = errors.New("address not allowed")
	ErrNotHTML        = errors.New("not an html page")
)

// Metadata is what a page says about itself.
type Metadata struct {
	Title       string
	Description string
	SiteName    string
	ImageURL    string // absolute http(s) URL, or empty
}

// NormalizeURL checks that a user-supplied string is a plain http(s) web
// address and returns it in canonical form.
func NormalizeURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > maxURLLength {
		return "", ErrInvalidURL
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil {
		return "", ErrInvalidURL
	}
	if port := u.Port(); port != "" && port != "80" && port != "443" {
		return "", ErrInvalidURL
	}
	u.Host = strings.ToLower(u.Host)
	u.Fragment = ""
	return u.String(), nil
}

// ranges that IsPrivate/IsLoopback/… don't cover but still aren't the public internet
var blockedNets = mustParseCIDRs(
	"0.0.0.0/8",       // "this network"
	"100.64.0.0/10",   // carrier-grade NAT
	"192.0.0.0/24",    // IETF protocol assignments
	"192.0.2.0/24",    // documentation
	"198.18.0.0/15",   // benchmarking
	"198.51.100.0/24", // documentation
	"203.0.113.0/24",  // documentation
	"240.0.0.0/4",     // reserved
	"64:ff9b::/96",    // NAT64 (can map to private IPv4)
	"2001:db8::/32",   // documentation
)

func mustParseCIDRs(cidrs ...string) []*net.IPNet {
	nets := make([]*net.IPNet, len(cidrs))
	for i, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			panic(err)
		}
		nets[i] = n
	}
	return nets
}

// IsPublicIP reports whether ip is an address on the public internet.
func IsPublicIP(ip net.IP) bool {
	if ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() || ip.IsMulticast() {
		return false
	}
	for _, n := range blockedNets {
		if n.Contains(ip) {
			return false
		}
	}
	return true
}

// guardDial runs just before each TCP connection, with the resolved address.
func guardDial(_, address string, _ syscall.RawConn) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return ErrBlockedAddress
	}
	if port != "80" && port != "443" {
		return ErrBlockedAddress
	}
	if !IsPublicIP(net.ParseIP(host)) {
		return ErrBlockedAddress
	}
	return nil
}

var client = &http.Client{
	Timeout: requestTimeout,
	Transport: &http.Transport{
		Proxy: nil, // never route through an environment-configured proxy
		// a custom dialer turns HTTP/2 off by default, and some sites reject HTTP/1.1 crawlers
		ForceAttemptHTTP2: true,
		DialContext: (&net.Dialer{
			Timeout: 5 * time.Second,
			Control: guardDial,
		}).DialContext,
		TLSHandshakeTimeout:    5 * time.Second,
		ResponseHeaderTimeout:  8 * time.Second,
		MaxResponseHeaderBytes: 64 << 10,
		DisableKeepAlives:      true,
	},
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= maxRedirects {
			return errors.New("too many redirects")
		}
		if _, err := NormalizeURL(req.URL.String()); err != nil {
			return err
		}
		return nil
	},
}

func get(ctx context.Context, target, accept string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, ErrInvalidURL
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", accept)
	req.Header.Set("Accept-Language", "en")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	return resp, nil
}

// Fetch downloads pageURL and extracts its preview metadata.
func Fetch(ctx context.Context, pageURL string) (Metadata, error) {
	resp, err := get(ctx, pageURL, "text/html,application/xhtml+xml")
	if err != nil {
		return Metadata{}, err
	}
	defer resp.Body.Close()

	contentType := resp.Header.Get("Content-Type")
	if !strings.Contains(contentType, "html") {
		return Metadata{}, ErrNotHTML
	}
	// converts legacy encodings to UTF-8 based on the header and <meta charset>
	body, err := charset.NewReader(io.LimitReader(resp.Body, maxHTMLBytes), contentType)
	if err != nil {
		return Metadata{}, err
	}
	return Parse(body, resp.Request.URL), nil
}

// FetchImage downloads a preview image, refusing anything over maxBytes.
func FetchImage(ctx context.Context, imageURL string, maxBytes int64) ([]byte, error) {
	resp, err := get(ctx, imageURL, "image/*")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, errors.New("image too large")
	}
	return data, nil
}

// Parse reads an HTML document and picks the best available title,
// description, site name and image, preferring Open Graph over Twitter cards
// over plain HTML, and the first occurrence of each tag. Relative image URLs
// are resolved against base.
func Parse(r io.Reader, base *url.URL) Metadata {
	var (
		og, twitter, plain Metadata
		inTitle            bool
	)

	z := html.NewTokenizer(r)
loop:
	for {
		switch z.Next() {
		case html.ErrorToken:
			break loop
		case html.StartTagToken, html.SelfClosingTagToken:
			name, hasAttr := z.TagName()
			switch string(name) {
			case "title":
				inTitle = plain.Title == ""
			case "meta":
				if !hasAttr {
					continue
				}
				var key, content string
				for {
					k, v, more := z.TagAttr()
					switch string(k) {
					case "property", "name":
						if key == "" {
							key = strings.ToLower(strings.TrimSpace(string(v)))
						}
					case "content":
						content = string(v)
					}
					if !more {
						break
					}
				}
				// first occurrence wins: some sites (mobile YouTube) put these tags in
				// <body>, but anything in <head> is seen first and takes precedence
				switch key {
				case "og:title":
					setOnce(&og.Title, content)
				case "og:description":
					setOnce(&og.Description, content)
				case "og:site_name":
					setOnce(&og.SiteName, content)
				case "og:image", "og:image:url", "og:image:secure_url":
					setOnce(&og.ImageURL, content)
				case "twitter:title":
					setOnce(&twitter.Title, content)
				case "twitter:description":
					setOnce(&twitter.Description, content)
				case "twitter:image", "twitter:image:src":
					setOnce(&twitter.ImageURL, content)
				case "description":
					setOnce(&plain.Description, content)
				}
			}
		case html.TextToken:
			if inTitle {
				plain.Title += string(z.Text())
			}
		case html.EndTagToken:
			name, _ := z.TagName()
			if string(name) == "title" {
				inTitle = false
			}
		}
	}

	return Metadata{
		Title:       clean(firstNonEmpty(og.Title, twitter.Title, plain.Title), maxTitleRunes),
		Description: clean(firstNonEmpty(og.Description, twitter.Description, plain.Description), maxDescRunes),
		SiteName:    clean(og.SiteName, maxTitleRunes),
		ImageURL:    resolveImage(firstNonEmpty(og.ImageURL, twitter.ImageURL), base),
	}
}

func setOnce(field *string, value string) {
	if *field == "" {
		*field = value
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// clean collapses whitespace and truncates to max runes.
func clean(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	runes := []rune(s)
	return strings.TrimSpace(string(runes[:max-1])) + "…"
}

func resolveImage(raw string, base *url.URL) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if base != nil {
		u = base.ResolveReference(u)
	}
	normalized, err := NormalizeURL(u.String())
	if err != nil {
		return ""
	}
	return normalized
}
