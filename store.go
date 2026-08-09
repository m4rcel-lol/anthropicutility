package main

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// GuildConfig is the per-server setup written by /setup.
type GuildConfig struct {
	GuildID    string
	ChannelID  string
	PingRoleID string // empty string = no role ping
}

// Store tracks already-posted items (per guild) and per-guild /setup config.
type Store struct {
	db *sql.DB
}

func openStore(path string) (*Store, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create data dir %s: %w", dir, err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// Single connection is enough for this workload; avoids lock contention.
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS posted_items (
			id        TEXT NOT NULL,
			guild_id  TEXT NOT NULL,
			posted_at TEXT NOT NULL,
			PRIMARY KEY (id, guild_id)
		);
		CREATE TABLE IF NOT EXISTS guild_config (
			guild_id     TEXT PRIMARY KEY,
			channel_id   TEXT NOT NULL,
			ping_role_id TEXT NOT NULL DEFAULT ''
		);
		CREATE TABLE IF NOT EXISTS greeted_guilds (
			guild_id   TEXT PRIMARY KEY,
			user_id    TEXT NOT NULL,
			greeted_at TEXT NOT NULL
		);
	`); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}

	// CREATE TABLE IF NOT EXISTS silently does nothing when the table already
	// exists, so a database written by an older build — where posted_items was
	// keyed by id alone and dedup was global across every server — keeps its old
	// shape. Every Has(id, guildID) would then fail with "no such column:
	// guild_id" and each server would look permanently up to date. Rebuild it.
	if err := migrateLegacyPostedItems(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate posted_items: %w", err)
	}

	return &Store{db: db}, nil
}

// migrateLegacyPostedItems upgrades a global (id-only) posted_items table to the
// per-guild (id, guild_id) schema. Existing rows are attributed to every server
// configured at migration time, so those servers are not spammed with their
// whole backlog on the next poll; /postall exists to seed a server on purpose.
func migrateLegacyPostedItems(db *sql.DB) error {
	cols, err := tableColumns(db, "posted_items")
	if err != nil {
		return err
	}
	if len(cols) == 0 || cols["guild_id"] {
		return nil // fresh database, or already per-guild
	}

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`ALTER TABLE posted_items RENAME TO posted_items_legacy`); err != nil {
		return err
	}
	if _, err := tx.Exec(`
		CREATE TABLE posted_items (
			id        TEXT NOT NULL,
			guild_id  TEXT NOT NULL,
			posted_at TEXT NOT NULL,
			PRIMARY KEY (id, guild_id)
		)
	`); err != nil {
		return err
	}

	// Older builds always had posted_at, but do not assume it.
	postedAt := "?"
	if cols["posted_at"] {
		postedAt = "COALESCE(l.posted_at, ?)"
	}
	res, err := tx.Exec(`
		INSERT OR IGNORE INTO posted_items (id, guild_id, posted_at)
		SELECT l.id, g.guild_id, `+postedAt+`
		FROM posted_items_legacy l
		CROSS JOIN guild_config g
	`, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DROP TABLE posted_items_legacy`); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}

	copied, _ := res.RowsAffected()
	log.Printf("event=migration name=posted_items_per_guild rows_backfilled=%d", copied)
	return nil
}

// tableColumns returns the column names of table, or an empty map if it does not exist.
func tableColumns(db *sql.DB, table string) (map[string]bool, error) {
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	cols := map[string]bool{}
	for rows.Next() {
		var (
			cid     int
			name    string
			ctype   sql.NullString
			notNull sql.NullInt64
			dflt    sql.NullString
			pk      sql.NullInt64
		)
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk); err != nil {
			return nil, err
		}
		cols[name] = true
	}
	return cols, rows.Err()
}

func (s *Store) Close() error {
	return s.db.Close()
}

// Has returns true if this item was already posted to this guild.
func (s *Store) Has(id, guildID string) (bool, error) {
	var exists int
	err := s.db.QueryRow(
		`SELECT 1 FROM posted_items WHERE id = ? AND guild_id = ?`,
		id, guildID,
	).Scan(&exists)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// Mark records that the item was successfully posted to this guild.
func (s *Store) Mark(id, guildID string) error {
	_, err := s.db.Exec(
		`INSERT OR IGNORE INTO posted_items (id, guild_id, posted_at) VALUES (?, ?, ?)`,
		id,
		guildID,
		time.Now().UTC().Format(time.RFC3339),
	)
	return err
}

// HasGreeted reports whether the welcome DM was already sent for this guild.
// Keeps a re-invite or a reconnect from DMing the same person twice.
func (s *Store) HasGreeted(guildID string) (bool, error) {
	var exists int
	err := s.db.QueryRow(`SELECT 1 FROM greeted_guilds WHERE guild_id = ?`, guildID).Scan(&exists)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// MarkGreeted records that the welcome DM for this guild reached userID.
func (s *Store) MarkGreeted(guildID, userID string) error {
	_, err := s.db.Exec(
		`INSERT OR IGNORE INTO greeted_guilds (guild_id, user_id, greeted_at) VALUES (?, ?, ?)`,
		guildID, userID, time.Now().UTC().Format(time.RFC3339),
	)
	return err
}

// SetGuildConfig upserts the /setup values for a guild.
func (s *Store) SetGuildConfig(guildID, channelID, pingRoleID string) error {
	_, err := s.db.Exec(`
		INSERT INTO guild_config (guild_id, channel_id, ping_role_id)
		VALUES (?, ?, ?)
		ON CONFLICT(guild_id) DO UPDATE SET
			channel_id   = excluded.channel_id,
			ping_role_id = excluded.ping_role_id
	`, guildID, channelID, pingRoleID)
	return err
}

// GetGuildConfig returns the setup for one guild, or nil if never configured.
func (s *Store) GetGuildConfig(guildID string) (*GuildConfig, error) {
	var c GuildConfig
	err := s.db.QueryRow(
		`SELECT guild_id, channel_id, ping_role_id FROM guild_config WHERE guild_id = ?`,
		guildID,
	).Scan(&c.GuildID, &c.ChannelID, &c.PingRoleID)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// AllGuildConfigs returns every server that has run /setup.
func (s *Store) AllGuildConfigs() ([]GuildConfig, error) {
	rows, err := s.db.Query(`SELECT guild_id, channel_id, ping_role_id FROM guild_config`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []GuildConfig
	for rows.Next() {
		var c GuildConfig
		if err := rows.Scan(&c.GuildID, &c.ChannelID, &c.PingRoleID); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
