package main

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// A database written by the old global-dedup build must be upgraded in place,
// not silently left alone by CREATE TABLE IF NOT EXISTS.
func TestOpenStoreMigratesLegacyPostedItems(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bot.db")

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		CREATE TABLE posted_items (id TEXT PRIMARY KEY, posted_at TEXT NOT NULL);
		CREATE TABLE guild_config (guild_id TEXT PRIMARY KEY, channel_id TEXT NOT NULL, ping_role_id TEXT NOT NULL DEFAULT '');
		INSERT INTO posted_items VALUES ('item-a','2026-01-01T00:00:00Z'),('item-b','2026-01-02T00:00:00Z');
		INSERT INTO guild_config VALUES ('guild-1','chan-1',''),('guild-2','chan-2','role-2');
	`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	store, err := openStore(path)
	if err != nil {
		t.Fatalf("openStore on legacy db: %v", err)
	}
	defer store.Close()

	// Old rows are attributed to every server configured at migration time.
	for _, g := range []string{"guild-1", "guild-2"} {
		for _, id := range []string{"item-a", "item-b"} {
			seen, err := store.Has(id, g)
			if err != nil {
				t.Fatalf("Has(%s,%s): %v", id, g, err)
			}
			if !seen {
				t.Errorf("Has(%s,%s) = false, want true", id, g)
			}
		}
	}

	// A server configured after the migration starts with a clean history.
	if err := store.SetGuildConfig("guild-3", "chan-3", ""); err != nil {
		t.Fatal(err)
	}
	seen, err := store.Has("item-a", "guild-3")
	if err != nil {
		t.Fatalf("Has for new guild: %v", err)
	}
	if seen {
		t.Error("new guild inherited delivery history, want independent state")
	}

	// Marking in one server must not affect another.
	if err := store.Mark("item-c", "guild-3"); err != nil {
		t.Fatal(err)
	}
	if seen, _ := store.Has("item-c", "guild-1"); seen {
		t.Error("Mark leaked across guilds")
	}
}

// Running against an already-migrated database must be a no-op.
func TestOpenStoreIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bot.db")

	s1, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s1.SetGuildConfig("g", "c", "r"); err != nil {
		t.Fatal(err)
	}
	if err := s1.Mark("item-a", "g"); err != nil {
		t.Fatal(err)
	}
	s1.Close()

	s2, err := openStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()

	seen, err := s2.Has("item-a", "g")
	if err != nil {
		t.Fatal(err)
	}
	if !seen {
		t.Error("history lost on reopen")
	}
}

// The welcome DM must be sent at most once per server, and tracked per server.
func TestGreetedGuildsIsPerGuildAndOnce(t *testing.T) {
	store, err := openStore(filepath.Join(t.TempDir(), "bot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	greeted, err := store.HasGreeted("guild-1")
	if err != nil {
		t.Fatal(err)
	}
	if greeted {
		t.Fatal("HasGreeted on a fresh store = true, want false")
	}

	if err := store.MarkGreeted("guild-1", "user-1"); err != nil {
		t.Fatal(err)
	}
	if greeted, _ := store.HasGreeted("guild-1"); !greeted {
		t.Error("HasGreeted after MarkGreeted = false, want true")
	}

	// A different server is untouched.
	if greeted, _ := store.HasGreeted("guild-2"); greeted {
		t.Error("greeting leaked to another guild")
	}

	// Re-marking (re-invite, reconnect) must not error or duplicate.
	if err := store.MarkGreeted("guild-1", "user-2"); err != nil {
		t.Fatalf("second MarkGreeted: %v", err)
	}

	// Upgrading an older database that predates the table must still work.
	var rows int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM greeted_guilds WHERE guild_id = 'guild-1'`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Errorf("greeted_guilds rows for guild-1 = %d, want 1", rows)
	}
}
