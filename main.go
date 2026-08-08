package main

import (
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("config error: %v", err)
	}

	store, err := openStore(cfg.SQLitePath)
	if err != nil {
		log.Fatalf("store error: %v", err)
	}
	defer store.Close()

	bot, err := newBot(cfg, store)
	if err != nil {
		log.Fatalf("bot error: %v", err)
	}
	defer bot.Close()

	log.Printf("event=started poll_interval_minutes=%d excerpt_length=%d sqlite=%s news_source=%s",
		cfg.PollIntervalMinutes, cfg.ExcerptLength, cfg.SQLitePath, cfg.NewsSourceURL)

	// Run once immediately, then on interval.
	bot.pollOnce()

	ticker := time.NewTicker(time.Duration(cfg.PollIntervalMinutes) * time.Minute)
	defer ticker.Stop()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	for {
		select {
		case <-ticker.C:
			bot.pollOnce()
		case sig := <-sigCh:
			log.Printf("event=shutdown signal=%s", sig.String())
			return
		}
	}
}

// Config holds all runtime settings. Every field is set explicitly from env
// (or the documented default). Fail-fast validation happens in loadConfig.
//
// Channel / ping-role targets are NOT env vars: each server configures them
// with the /setup slash command. NEWS_SOURCE_URL is global and identical for
// every server; it cannot be changed per-guild.
type Config struct {
	DiscordToken        string
	NewsSourceURL       string
	PollIntervalMinutes int
	ExcerptLength       int
	SQLitePath          string
}

func loadConfig() (*Config, error) {
	token := strings.TrimSpace(os.Getenv("DISCORD_BOT_TOKEN"))
	if token == "" {
		return nil, errMissing("DISCORD_BOT_TOKEN")
	}

	// Anthropic does not publish an official RSS/Atom feed for https://www.anthropic.com/news.
	// NEWS_SOURCE_URL is global for every Discord server and cannot be overridden per-guild.
	// Confirm the URL manually before production use; do not hardcode a scraper.
	newsURL := strings.TrimSpace(os.Getenv("NEWS_SOURCE_URL"))
	if newsURL == "" {
		return nil, errMissing("NEWS_SOURCE_URL")
	}

	pollMinutes := 15 // default: 15 minutes between polls
	if v := strings.TrimSpace(os.Getenv("POLL_INTERVAL_MINUTES")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return nil, errInvalid("POLL_INTERVAL_MINUTES", "must be a positive integer")
		}
		pollMinutes = n
	}

	excerptLen := 280 // default: Twitter-style ~280 characters
	if v := strings.TrimSpace(os.Getenv("EXCERPT_LENGTH")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return nil, errInvalid("EXCERPT_LENGTH", "must be a positive integer")
		}
		excerptLen = n
	}

	sqlitePath := "/data/bot.db" // default: named volume mount point
	if v := strings.TrimSpace(os.Getenv("SQLITE_PATH")); v != "" {
		sqlitePath = v
	}

	return &Config{
		DiscordToken:        token,
		NewsSourceURL:       newsURL,
		PollIntervalMinutes: pollMinutes,
		ExcerptLength:       excerptLen,
		SQLitePath:          sqlitePath,
	}, nil
}

func errMissing(name string) error {
	return &configError{msg: name + " is required"}
}

func errInvalid(name, reason string) error {
	return &configError{msg: name + " is invalid: " + reason}
}

type configError struct{ msg string }

func (e *configError) Error() string { return e.msg }
