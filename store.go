package main

import (
	"database/sql"
	"fmt"
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
	`); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}

	return &Store{db: db}, nil
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
