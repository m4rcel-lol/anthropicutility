package main

import (
	"fmt"
	"path/filepath"
	"testing"
)

func TestNormalizeRoleID(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"123456789", "123456789"},
		{"<@&123456789>", "123456789"}, // a full mention got stored
		{"@123456789", "123456789"},
		{"&123456789", "123456789"},
		{"  123456789  ", "123456789"},
		{"", ""},
		{"@everyone", ""}, // no snowflake to extract
	}
	for _, tc := range tests {
		if got := normalizeRoleID(tc.in); got != tc.want {
			t.Errorf("normalizeRoleID(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A stored value must never render into a doubled @, whatever shape it arrived in.
func TestRoleMentionRendersSingleAt(t *testing.T) {
	store, err := openStore(filepath.Join(t.TempDir(), "bot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if err := store.SetGuildConfig("g", "c", "<@&999>"); err != nil {
		t.Fatal(err)
	}
	cfg, err := store.GetGuildConfig("g")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PingRoleID != "999" {
		t.Fatalf("stored ping role = %q, want %q", cfg.PingRoleID, "999")
	}
	if got, want := roleLabel(cfg.PingRoleID), "<@&999>"; got != want {
		t.Errorf("roleLabel() = %q, want %q", got, want)
	}
	if got, want := fmt.Sprintf("<@&%s>", cfg.PingRoleID), "<@&999>"; got != want {
		t.Errorf("post content = %q, want %q", got, want)
	}
}

// Reading a row written by an older build repairs it on the way out.
func TestGetGuildConfigRepairsStoredMention(t *testing.T) {
	store, err := openStore(filepath.Join(t.TempDir(), "bot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Bypass SetGuildConfig to simulate a row an older build wrote directly.
	if _, err := store.db.Exec(
		`INSERT INTO guild_config (guild_id, channel_id, ping_role_id) VALUES ('g','c','<@&777>')`,
	); err != nil {
		t.Fatal(err)
	}

	cfg, err := store.GetGuildConfig("g")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PingRoleID != "777" {
		t.Errorf("PingRoleID = %q, want %q", cfg.PingRoleID, "777")
	}

	all, err := store.AllGuildConfigs()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].PingRoleID != "777" {
		t.Errorf("AllGuildConfigs = %+v, want ping role 777", all)
	}
}

func TestNormalizeKey(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"plain", "https://www.anthropic.com/news/a", "https://www.anthropic.com/news/a"},
		{"trailing slash", "https://www.anthropic.com/news/a/", "https://www.anthropic.com/news/a"},
		{"fragment", "https://www.anthropic.com/news/a#intro", "https://www.anthropic.com/news/a"},
		{"host case", "https://WWW.Anthropic.com/news/a", "https://www.anthropic.com/news/a"},
		{"utm stripped", "https://www.anthropic.com/news/a?utm_source=rss&utm_medium=feed", "https://www.anthropic.com/news/a"},
		{"real query kept", "https://www.anthropic.com/news?page=2", "https://www.anthropic.com/news?page=2"},
		{"not a url", "  Some GUID Value ", "some guid value"},
		{"empty", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeKey(tc.in); got != tc.want {
				t.Errorf("normalizeKey(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// The reported bug: a feed whose guid changes on every request made each poll
// look like brand-new articles, so the same news was posted over and over.
func TestRotatingGUIDKeepsStableKey(t *testing.T) {
	feed := func(guid string) string {
		return `<?xml version="1.0"?><rss version="2.0"><channel><title>Anthropic</title>
			<item>
				<guid isPermaLink="false">` + guid + `</guid>
				<title>Claude gets better</title>
				<link>https://www.anthropic.com/news/claude-gets-better</link>
				<pubDate>Mon, 03 Aug 2026 10:00:00 GMT</pubDate>
				<description>Details.</description>
			</item>
		</channel></rss>`
	}

	first, err := parseFeed([]byte(feed("urn:hash:1754212800-aaaa")))
	if err != nil {
		t.Fatal(err)
	}
	second, err := parseFeed([]byte(feed("urn:hash:1754216400-bbbb")))
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("parsed %d and %d items, want 1 each", len(first), len(second))
	}
	if first[0].ID != second[0].ID {
		t.Errorf("key changed with the guid: %q then %q", first[0].ID, second[0].ID)
	}

	// End to end: posting the first fetch must suppress the second.
	store, err := openStore(filepath.Join(t.TempDir(), "bot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if err := store.Mark(first[0].ID, "g"); err != nil {
		t.Fatal(err)
	}
	seen, err := store.HasAny("g", second[0].keys()...)
	if err != nil {
		t.Fatal(err)
	}
	if !seen {
		t.Error("second fetch looked unseen — the article would be re-posted")
	}
}

// History written under the old guid-based key must still be recognized, so the
// key change does not replay the whole backlog once.
func TestLegacyKeyStillMatches(t *testing.T) {
	store, err := openStore(filepath.Join(t.TempDir(), "bot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	legacyGUID := "https://www.anthropic.com/news/older-post/"
	if err := store.Mark(legacyGUID, "g"); err != nil {
		t.Fatal(err)
	}

	items, err := parseFeed([]byte(`<?xml version="1.0"?><rss version="2.0"><channel><title>A</title>
		<item>
			<guid>` + legacyGUID + `</guid>
			<title>Older post</title>
			<link>https://www.anthropic.com/news/older-post/</link>
		</item>
	</channel></rss>`))
	if err != nil {
		t.Fatal(err)
	}

	seen, err := store.HasAny("g", items[0].keys()...)
	if err != nil {
		t.Fatal(err)
	}
	if !seen {
		t.Error("item recorded under the legacy key looked new")
	}
}

// The same article twice in one fetch must only be posted once.
func TestDuplicateEntriesInOneFeedCollapse(t *testing.T) {
	items, err := parseFeed([]byte(`<?xml version="1.0"?><rss version="2.0"><channel><title>A</title>
		<item><guid>x1</guid><title>Same</title><link>https://www.anthropic.com/news/same</link></item>
		<item><guid>x2</guid><title>Same</title><link>https://www.anthropic.com/news/same/</link></item>
	</channel></rss>`))
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Errorf("parsed %d items, want 1 after collapsing the duplicate", len(items))
	}
}
