package main

import (
	"encoding/xml"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"
)

// NewsItem is a normalized feed entry used by the rest of the bot.
type NewsItem struct {
	ID        string   // stable dedup key, derived from the canonical article URL
	LegacyIDs []string // raw guid/link, so history from older builds still matches
	Title     string
	Link      string
	Published time.Time
	Summary   string // plain text (tags stripped)
	ImageURL  string // best banner/hero image from the entry, if any
}

// keys returns every key under which this article may have been recorded:
// the current one first, then any a previous build would have used.
func (n NewsItem) keys() []string {
	return append([]string{n.ID}, n.LegacyIDs...)
}

// trackingParams are query keys that vary per request or per referrer and say
// nothing about which article this is.
var trackingParams = map[string]bool{
	"utm_source": true, "utm_medium": true, "utm_campaign": true,
	"utm_term": true, "utm_content": true, "utm_id": true,
	"ref": true, "source": true, "fbclid": true, "gclid": true,
	"mc_cid": true, "mc_eid": true, "_ga": true,
}

// normalizeKey turns a URL into a canonical, comparable form: lowercase scheme
// and host, no fragment, no tracking parameters, no trailing slash. Anything
// that is not a URL is just trimmed and lowercased.
func normalizeKey(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return strings.ToLower(raw)
	}

	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	u.Fragment = ""
	u.RawFragment = ""

	if q := u.Query(); len(q) > 0 {
		for k := range q {
			if trackingParams[strings.ToLower(k)] {
				q.Del(k)
			}
		}
		u.RawQuery = q.Encode() // Encode sorts keys, so order cannot matter
	}
	if u.Path != "/" {
		u.Path = strings.TrimSuffix(u.Path, "/")
	}
	return u.String()
}

// itemKeys derives the dedup key for an entry, plus the keys older builds may
// already have recorded for it.
//
// The article link is preferred over the guid: a guid is only required to be
// unique, not stable, and feed bridges regenerate it on every request (adding a
// timestamp or a hash of the fetch). A rotating guid makes every poll look like
// fresh news, which re-posts the same articles forever. The canonical URL of an
// article does not change.
func itemKeys(guid, link, title string) (id string, legacy []string) {
	switch {
	case link != "":
		id = normalizeKey(link)
	case guid != "":
		id = normalizeKey(guid)
	default:
		// No URL at all: fall back to the title so the entry still dedups.
		if t := strings.ToLower(strings.Join(strings.Fields(title), " ")); t != "" {
			id = "title:" + t
		}
	}
	if id == "" {
		return "", nil
	}

	// Keys a previous build would have written for this same entry.
	for _, raw := range []string{strings.TrimSpace(guid), strings.TrimSpace(link)} {
		if raw != "" && raw != id {
			legacy = append(legacy, raw)
		}
	}
	return id, legacy
}

// fetchNews retrieves and parses the configured RSS or Atom feed.
// Supports both RSS 2.0 and Atom 1.0.
func fetchNews(url string) ([]NewsItem, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	// Identify ourselves; some feeds reject empty/generic User-Agents.
	req.Header.Set("User-Agent", "anthropic-discord-bot/1.0 (+https://github.com/m4rcel-lol)")
	req.Header.Set("Accept", "application/rss+xml, application/atom+xml, application/xml, text/xml, */*")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http get: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20)) // 2 MiB cap
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	return parseFeed(body)
}

func parseFeed(data []byte) ([]NewsItem, error) {
	// Try Atom first (common for modern feeds), then RSS.
	var atom atomFeed
	if err := xml.Unmarshal(data, &atom); err == nil && (atom.XMLName.Local == "feed" || len(atom.Entries) > 0) {
		return atom.toItems(), nil
	}

	var rss rssFeed
	if err := xml.Unmarshal(data, &rss); err == nil && (rss.Channel.Title != "" || len(rss.Channel.Items) > 0) {
		return rss.toItems(), nil
	}

	return nil, fmt.Errorf("unrecognized feed format (neither RSS nor Atom)")
}

// --- Atom ---

type atomFeed struct {
	XMLName xml.Name    `xml:"feed"`
	Entries []atomEntry `xml:"entry"`
}

type atomEntry struct {
	ID        string     `xml:"id"`
	Title     string     `xml:"title"`
	Updated   string     `xml:"updated"`
	Published string     `xml:"published"`
	Summary   atomText   `xml:"summary"`
	Content   atomText   `xml:"content"`
	Links     []atomLink `xml:"link"`
}

type atomText struct {
	Type string `xml:"type,attr"`
	Body string `xml:",chardata"`
	HTML string `xml:",innerxml"` // fallback when type=html
}

type atomLink struct {
	Rel  string `xml:"rel,attr"`
	Href string `xml:"href,attr"`
	Type string `xml:"type,attr"`
}

func (f atomFeed) toItems() []NewsItem {
	items := make([]NewsItem, 0, len(f.Entries))
	seen := make(map[string]bool, len(f.Entries))
	for _, e := range f.Entries {
		link := e.primaryLink()
		id, legacy := itemKeys(e.ID, link, e.Title)
		if id == "" || seen[id] {
			continue // unusable, or the same article twice in one feed
		}
		seen[id] = true

		summary := e.Summary.plain()
		if summary == "" {
			summary = e.Content.plain()
		}

		pub := parseTime(e.Published)
		if pub.IsZero() {
			pub = parseTime(e.Updated)
		}

		rawHTML := e.Summary.HTML + e.Content.HTML + e.Summary.Body + e.Content.Body
		items = append(items, NewsItem{
			ID:        id,
			LegacyIDs: legacy,
			Title:     strings.TrimSpace(e.Title),
			Link:      link,
			Published: pub,
			Summary:   summary,
			ImageURL:  extractImageURL(rawHTML),
		})
	}
	return items
}

func (e atomEntry) primaryLink() string {
	// Prefer rel=alternate, then first href, then empty.
	var first string
	for _, l := range e.Links {
		if l.Href == "" {
			continue
		}
		if first == "" {
			first = l.Href
		}
		if l.Rel == "alternate" || l.Rel == "" {
			return l.Href
		}
	}
	return first
}

func (t atomText) plain() string {
	raw := t.Body
	if raw == "" {
		raw = t.HTML
	}
	return stripTags(raw)
}

// --- RSS 2.0 ---

type rssFeed struct {
	XMLName xml.Name   `xml:"rss"`
	Channel rssChannel `xml:"channel"`
}

type rssChannel struct {
	Title string    `xml:"title"`
	Items []rssItem `xml:"item"`
}

type rssItem struct {
	GUID        rssGUID `xml:"guid"`
	Title       string  `xml:"title"`
	Link        string  `xml:"link"`
	PubDate     string  `xml:"pubDate"`
	Description string  `xml:"description"`
	Content     string  `xml:"encoded"` // content:encoded
}

type rssGUID struct {
	Value       string `xml:",chardata"`
	IsPermaLink string `xml:"isPermaLink,attr"`
}

func (f rssFeed) toItems() []NewsItem {
	items := make([]NewsItem, 0, len(f.Channel.Items))
	seen := make(map[string]bool, len(f.Channel.Items))
	for _, it := range f.Channel.Items {
		link := strings.TrimSpace(it.Link)
		id, legacy := itemKeys(it.GUID.Value, link, it.Title)
		if id == "" || seen[id] {
			continue // unusable, or the same article twice in one feed
		}
		seen[id] = true

		summary := it.Description
		if summary == "" {
			summary = it.Content
		}
		summary = stripTags(summary)

		items = append(items, NewsItem{
			ID:        id,
			LegacyIDs: legacy,
			Title:     strings.TrimSpace(it.Title),
			Link:      link,
			Published: parseTime(it.PubDate),
			Summary:   summary,
			ImageURL:  extractImageURL(it.Description, it.Content),
		})
	}
	return items
}

// --- helpers ---

func parseTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	// Common formats used by RSS/Atom.
	formats := []string{
		time.RFC3339,
		time.RFC3339Nano,
		time.RFC1123Z,
		time.RFC1123,
		"2006-01-02T15:04:05Z",
		"2006-01-02T15:04:05-07:00",
		"Mon, 02 Jan 2006 15:04:05 MST",
		"2006-01-02",
	}
	for _, f := range formats {
		if t, err := time.Parse(f, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// stripTags removes HTML tags and unescapes entities. Minimal, no external dep.
func stripTags(s string) string {
	s = html.UnescapeString(s)
	var b strings.Builder
	b.Grow(len(s))
	inTag := false
	for _, r := range s {
		switch {
		case r == '<':
			inTag = true
		case r == '>':
			inTag = false
		case !inTag:
			b.WriteRune(r)
		}
	}
	// Collapse whitespace.
	return strings.Join(strings.Fields(b.String()), " ")
}

// cropExcerpt truncates s to at most max runes, cutting at the nearest word
// boundary, and appends an ellipsis when truncation occurs.
func cropExcerpt(s string, max int) string {
	s = strings.TrimSpace(s)
	if max <= 0 || s == "" {
		return s
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	// Leave room for "…"
	limit := max - 1
	if limit < 1 {
		limit = 1
	}
	cut := runes[:limit]
	// Walk back to last whitespace so we don't split a word.
	for i := len(cut) - 1; i > 0; i-- {
		if unicode.IsSpace(cut[i]) {
			cut = cut[:i]
			break
		}
	}
	return strings.TrimSpace(string(cut)) + "…"
}

// imgSrcRE finds img src attributes in HTML fragments from the feed.
var imgSrcRE = regexp.MustCompile(`(?i)<img[^>]+src=["']([^"']+)["']`)

// extractImageURL picks the best banner-like image from HTML bodies.
// Prefers non-SVG raster images (png/jpg/webp); falls back to any http(s) img.
func extractImageURL(htmlParts ...string) string {
	var candidates []string
	for _, part := range htmlParts {
		for _, m := range imgSrcRE.FindAllStringSubmatch(part, -1) {
			if len(m) < 2 {
				continue
			}
			u := strings.TrimSpace(m[1])
			if strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
				candidates = append(candidates, u)
			}
		}
	}
	if len(candidates) == 0 {
		return ""
	}
	// Prefer wide/banner-looking rasters over small SVG icons.
	var best string
	bestScore := -1
	for _, u := range candidates {
		lower := strings.ToLower(u)
		score := 0
		if strings.Contains(lower, ".png") || strings.Contains(lower, ".jpg") ||
			strings.Contains(lower, ".jpeg") || strings.Contains(lower, ".webp") {
			score += 3
		}
		if strings.Contains(lower, ".svg") {
			score -= 2
		}
		if strings.Contains(lower, "banner") || strings.Contains(lower, "hero") ||
			strings.Contains(lower, "og-image") || strings.Contains(lower, "3840") ||
			strings.Contains(lower, "wide") {
			score += 2
		}
		// Anthropic CDN wide assets often encode dimensions in the filename.
		if strings.Contains(lower, "x1855") || strings.Contains(lower, "x1080") ||
			strings.Contains(lower, "x1200") {
			score += 2
		}
		if score > bestScore {
			bestScore = score
			best = u
		}
	}
	if best != "" {
		return best
	}
	return candidates[0]
}
