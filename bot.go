package main

import (
	"bytes"
	_ "embed"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
)

// Embed accent color: pure white (#FFFFFF). Explicit choice — not random.
const embedColor = 0xFFFFFF

// Brand icon used as embed thumbnail + footer icon on every post.
// Embedded into the binary so no external URL/hosting is required.
//
//go:embed assets/icon.jpg
var brandIcon []byte

const brandIconName = "icon.jpg"

// Pause between /postall sends so a full backlog does not hit the per-channel
// message rate limit in one burst.
const postAllDelay = 750 * time.Millisecond

// Bot owns the Discord session and orchestrates poll → post + slash commands.
type Bot struct {
	cfg   *Config
	store *Store
	sess  *discordgo.Session
}

func newBot(cfg *Config, store *Store) (*Bot, error) {
	sess, err := discordgo.New("Bot " + cfg.DiscordToken)
	if err != nil {
		return nil, fmt.Errorf("discordgo.New: %w", err)
	}

	b := &Bot{cfg: cfg, store: store, sess: sess}

	// Intent for guilds is enough for slash commands + ChannelMessageSendComplex.
	sess.Identify.Intents = discordgo.IntentsGuilds

	sess.AddHandler(b.onReady)
	sess.AddHandler(b.onInteractionCreate)

	if err := sess.Open(); err != nil {
		return nil, fmt.Errorf("discord open: %w", err)
	}
	return b, nil
}

func (b *Bot) Close() {
	_ = b.sess.Close()
}

func (b *Bot) onReady(s *discordgo.Session, r *discordgo.Ready) {
	log.Printf("event=ready user=%s#%s guilds=%d", r.User.Username, r.User.Discriminator, len(r.Guilds))

	// Presence: Idle + Watching "Anthropic News | N servers".
	b.setPresence(s)

	// Global slash commands (can take up to ~1 hour to appear the first time).
	commands := []*discordgo.ApplicationCommand{
		{
			Name:        "setup",
			Description: "Configure the news channel and optional ping role for this server",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionChannel,
					Name:        "channel",
					Description: "Channel where Anthropic news embeds will be posted",
					Required:    true,
					ChannelTypes: []discordgo.ChannelType{
						discordgo.ChannelTypeGuildText,
						discordgo.ChannelTypeGuildNews,
					},
				},
				{
					Type:        discordgo.ApplicationCommandOptionRole,
					Name:        "ping_role",
					Description: "Role to mention when a new post arrives (optional)",
					Required:    false,
				},
			},
		},
		{
			Name:        "credits",
			Description: "Show who built this bot and what it is written in",
		},
		{
			Name:        "info",
			Description: "Show RSS feed info, check for newest news, and post anything new",
		},
		{
			Name:        "postall",
			Description: "Force-post every news item to this server's channel, even ones already posted",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionBoolean,
					Name:        "ping",
					Description: "Mention the configured ping role on each post (default: true)",
					Required:    false,
				},
			},
		},
	}

	for _, cmd := range commands {
		_, err := s.ApplicationCommandCreate(s.State.User.ID, "", cmd)
		if err != nil {
			log.Printf("event=command_register_error name=%s err=%q", cmd.Name, err.Error())
			continue
		}
		log.Printf("event=command_registered name=%s", cmd.Name)
	}
}

func (b *Bot) onInteractionCreate(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if i.Type != discordgo.InteractionApplicationCommand {
		return
	}

	switch i.ApplicationCommandData().Name {
	case "setup":
		b.cmdSetup(s, i)
	case "credits":
		b.cmdCredits(s, i)
	case "info":
		b.cmdInfo(s, i)
	case "postall":
		b.cmdPostAll(s, i)
	}
}

func (b *Bot) cmdCredits(s *discordgo.Session, i *discordgo.InteractionCreate) {
	msg := strings.Join([]string{
		"**Credits**",
		"",
		"Built by [m4rcel-lol](https://github.com/m4rcel-lol), with little help of Claude.",
		"",
		"**Stack**",
		"• Language: **Go** (1.22+)",
		"• Discord: `github.com/bwmarrin/discordgo`",
		"• Storage: SQLite via `modernc.org/sqlite` (pure Go)",
		"• Feed: standard library `net/http` + `encoding/xml` (RSS 2.0 / Atom 1.0)",
	}, "\n")

	log.Printf("event=credits user=%s guild=%s", interactionUserID(i), i.GuildID)
	b.respondEphemeral(s, i, msg)
}

func (b *Bot) cmdInfo(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if i.GuildID == "" {
		b.respondEphemeral(s, i, "This command can only be used inside a server.")
		return
	}

	// Defer response — feed fetch may take a moment.
	err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Flags: discordgo.MessageFlagsEphemeral,
		},
	})
	if err != nil {
		log.Printf("event=interaction_respond_error op=defer err=%q", err.Error())
		return
	}

	cfg, err := b.store.GetGuildConfig(i.GuildID)
	if err != nil {
		log.Printf("event=info_error op=get_config guild=%s err=%q", i.GuildID, err.Error())
		b.followupEphemeral(s, i, "Failed to read server settings.")
		return
	}

	log.Printf("event=info_fetch guild=%s url=%s", i.GuildID, b.cfg.NewsSourceURL)
	items, err := fetchNews(b.cfg.NewsSourceURL)
	if err != nil {
		log.Printf("event=info_fetch_error err=%q", err.Error())
		b.followupEphemeral(s, i, fmt.Sprintf(
			"**RSS feed**\n• URL: `%s`\n• Status: **error** — %s",
			b.cfg.NewsSourceURL, err.Error(),
		))
		return
	}

	// Newest first for the summary.
	sort.Slice(items, func(a, b int) bool {
		return items[a].Published.After(items[b].Published)
	})

	var newestTitle, newestLink, newestDate string
	if len(items) > 0 {
		newestTitle = items[0].Title
		newestLink = items[0].Link
		if !items[0].Published.IsZero() {
			newestDate = items[0].Published.UTC().Format("2006-01-02 15:04 UTC")
		} else {
			newestDate = "unknown"
		}
	}

	var lines []string
	lines = append(lines,
		"**Feed info**",
		fmt.Sprintf("• URL: `%s`", b.cfg.NewsSourceURL),
		fmt.Sprintf("• Entries fetched: **%d**", len(items)),
		fmt.Sprintf("• Poll interval: every **%d** minutes", b.cfg.PollIntervalMinutes),
	)
	if newestTitle != "" {
		lines = append(lines,
			"",
			"**Newest in feed**",
			fmt.Sprintf("• Title: %s", newestTitle),
			fmt.Sprintf("• Date: %s", newestDate),
		)
		if newestLink != "" {
			lines = append(lines, fmt.Sprintf("• Link: %s", newestLink))
		}
	}

	if cfg == nil {
		lines = append(lines,
			"",
			"**This server:** not configured yet — run `/setup` first. Nothing was posted.",
		)
		log.Printf("event=info guild=%s result=no_setup items=%d", i.GuildID, len(items))
		b.followupEphemeral(s, i, strings.Join(lines, "\n"))
		return
	}

	lines = append(lines,
		"",
		"**This server**",
		fmt.Sprintf("• Channel: <#%s>", cfg.ChannelID),
	)
	if cfg.PingRoleID != "" {
		lines = append(lines, fmt.Sprintf("• Ping role: <@&%s>", cfg.PingRoleID))
	} else {
		lines = append(lines, "• Ping role: _(none)_")
	}

	// Check for items not yet posted to *this* guild; post them (oldest first).
	// Delivery history is tracked per server, so a server that was set up later
	// still receives everything it has not seen yet.
	var newItems []NewsItem
	checkFailed := 0
	var firstCheckErr string
	for _, it := range items {
		if it.ID == "" {
			continue
		}
		seen, err := b.store.Has(it.ID, i.GuildID)
		if err != nil {
			log.Printf("event=store_error op=has id=%q guild=%s err=%q", it.ID, i.GuildID, err.Error())
			checkFailed++
			if firstCheckErr == "" {
				firstCheckErr = err.Error()
			}
			continue
		}
		if !seen {
			newItems = append(newItems, it)
		}
	}

	// Post oldest-first for chronological channel order.
	sort.Slice(newItems, func(a, b int) bool {
		return newItems[a].Published.Before(newItems[b].Published)
	})

	posted, postFailed := 0, 0
	var firstPostErr string
	for _, it := range newItems {
		if err := b.postItemToGuild(it, *cfg); err != nil {
			log.Printf("event=info_post_error id=%q guild=%s err=%q", it.ID, i.GuildID, err.Error())
			postFailed++
			if firstPostErr == "" {
				firstPostErr = err.Error()
			}
			continue
		}
		posted++
	}

	// Never report "up to date" when a check or a post actually failed — that is
	// what made a newly configured server look like it had nothing to receive.
	lines = append(lines, "")
	switch {
	case posted > 0:
		lines = append(lines, fmt.Sprintf("**Check result:** found **%d** new item(s) for this server and posted them to <#%s>.", posted, cfg.ChannelID))
	case checkFailed == 0 && postFailed == 0:
		lines = append(lines, fmt.Sprintf("**Check result:** no new items for this server — <#%s> is up to date.", cfg.ChannelID))
	default:
		lines = append(lines, "**Check result:** nothing could be posted.")
	}
	if postFailed > 0 {
		lines = append(lines,
			fmt.Sprintf("⚠️ **%d item(s) failed to post** to <#%s> — `%s`", postFailed, cfg.ChannelID, firstPostErr),
			"Check the bot's channel permissions: **View Channel**, **Send Messages**, **Embed Links**, **Attach Files**.",
		)
	}
	if checkFailed > 0 {
		lines = append(lines, fmt.Sprintf("⚠️ **%d item(s) could not be checked** against this server's history — `%s`", checkFailed, firstCheckErr))
	}
	lines = append(lines, "", "_Delivery history is tracked separately for every server._")

	log.Printf("event=info guild=%s items=%d new=%d posted=%d post_failed=%d check_failed=%d",
		i.GuildID, len(items), len(newItems), posted, postFailed, checkFailed)
	b.followupEphemeral(s, i, strings.Join(lines, "\n"))
}

// cmdPostAll posts every entry currently in the feed to this server's configured
// channel, ignoring the per-server delivery history. Useful to seed a server that
// was set up after items had already been published elsewhere.
func (b *Bot) cmdPostAll(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if i.GuildID == "" || i.Member == nil {
		b.respondEphemeral(s, i, "This command can only be used inside a server.")
		return
	}
	// Same gate as /setup: this can write dozens of messages and ping a role.
	perms := i.Member.Permissions
	if perms&discordgo.PermissionAdministrator == 0 && perms&discordgo.PermissionManageServer == 0 {
		b.respondEphemeral(s, i, "You need **Manage Server** or **Administrator** to run `/postall`.")
		return
	}

	// Defer — fetching plus posting every item takes well past the 3s ACK window.
	err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Flags: discordgo.MessageFlagsEphemeral,
		},
	})
	if err != nil {
		log.Printf("event=interaction_respond_error op=defer cmd=postall err=%q", err.Error())
		return
	}

	cfg, err := b.store.GetGuildConfig(i.GuildID)
	if err != nil {
		log.Printf("event=postall_error op=get_config guild=%s err=%q", i.GuildID, err.Error())
		b.followupEphemeral(s, i, "Failed to read server settings.")
		return
	}
	if cfg == nil {
		b.followupEphemeral(s, i, "This server is not configured yet — run `/setup` first.")
		return
	}

	ping := true
	for _, o := range i.ApplicationCommandData().Options {
		if o.Name == "ping" {
			ping = o.BoolValue()
		}
	}

	log.Printf("event=postall_fetch guild=%s url=%s ping=%t", i.GuildID, b.cfg.NewsSourceURL, ping)
	items, err := fetchNews(b.cfg.NewsSourceURL)
	if err != nil {
		log.Printf("event=postall_fetch_error guild=%s err=%q", i.GuildID, err.Error())
		b.followupEphemeral(s, i, fmt.Sprintf("Failed to fetch the feed — `%s`\nNothing was posted.", err.Error()))
		return
	}
	if len(items) == 0 {
		b.followupEphemeral(s, i, "The feed returned no entries — nothing to post.")
		return
	}

	// Oldest first so the channel timeline reads chronologically.
	sort.Slice(items, func(a, b int) bool {
		return items[a].Published.Before(items[b].Published)
	})

	// Post to the configured channel with the configured role, per this server's
	// /setup. `ping:false` suppresses the mention without changing the setup.
	target := *cfg
	if !ping {
		target.PingRoleID = ""
	}

	posted, failed := 0, 0
	var firstErr string
	for idx, it := range items {
		if err := b.postItemToGuild(it, target); err != nil {
			log.Printf("event=postall_post_error guild=%s id=%q err=%q", i.GuildID, it.ID, err.Error())
			failed++
			if firstErr == "" {
				firstErr = err.Error()
			}
			continue
		}
		posted++
		// Space out sends so a full backlog does not slam the channel rate limit.
		if idx < len(items)-1 {
			time.Sleep(postAllDelay)
		}
	}

	var lines []string
	lines = append(lines,
		"**/postall**",
		fmt.Sprintf("• Entries in feed: **%d**", len(items)),
		fmt.Sprintf("• Posted to <#%s>: **%d**", cfg.ChannelID, posted),
	)
	if cfg.PingRoleID != "" {
		if ping {
			lines = append(lines, fmt.Sprintf("• Ping role: <@&%s>", cfg.PingRoleID))
		} else {
			lines = append(lines, fmt.Sprintf("• Ping role: <@&%s> _(suppressed for this run)_", cfg.PingRoleID))
		}
	} else {
		lines = append(lines, "• Ping role: _(none)_")
	}
	if failed > 0 {
		lines = append(lines,
			fmt.Sprintf("• Failed: **%d** — `%s`", failed, firstErr),
			"Check the bot's channel permissions: **View Channel**, **Send Messages**, **Embed Links**, **Attach Files**.",
		)
	}

	log.Printf("event=postall guild=%s channel=%s items=%d posted=%d failed=%d ping=%t",
		i.GuildID, cfg.ChannelID, len(items), posted, failed, ping)
	b.followupEphemeral(s, i, strings.Join(lines, "\n"))
}

func (b *Bot) cmdSetup(s *discordgo.Session, i *discordgo.InteractionCreate) {
	// Only allow server admins / manage-server users to run /setup.
	if i.Member == nil || i.GuildID == "" {
		b.respondEphemeral(s, i, "This command can only be used inside a server.")
		return
	}
	perms := i.Member.Permissions
	if perms&discordgo.PermissionAdministrator == 0 && perms&discordgo.PermissionManageServer == 0 {
		b.respondEphemeral(s, i, "You need **Manage Server** or **Administrator** to run `/setup`.")
		return
	}

	// Detect existing setup — do not overwrite; tell the user nothing more is needed.
	existing, err := b.store.GetGuildConfig(i.GuildID)
	if err != nil {
		log.Printf("event=setup_error op=get guild=%s err=%q", i.GuildID, err.Error())
		b.respondEphemeral(s, i, "Failed to read settings. Try again later.")
		return
	}
	if existing != nil {
		msg := fmt.Sprintf(
			"This server is **already set up** — nothing more needs to be configured.\n\n• News channel: <#%s>",
			existing.ChannelID,
		)
		if existing.PingRoleID != "" {
			msg += fmt.Sprintf("\n• Ping role: <@&%s>", existing.PingRoleID)
		} else {
			msg += "\n• Ping role: _(none)_"
		}
		msg += "\n\nThe news feed is global and the same for every server — it cannot be changed here."
		log.Printf("event=setup_already guild=%s channel=%s ping_role=%s", i.GuildID, existing.ChannelID, existing.PingRoleID)
		b.respondEphemeral(s, i, msg)
		return
	}

	opts := i.ApplicationCommandData().Options
	var channelID, pingRoleID string
	for _, o := range opts {
		switch o.Name {
		case "channel":
			if ch := o.ChannelValue(s); ch != nil {
				channelID = ch.ID
			}
		case "ping_role":
			if role := o.RoleValue(s, i.GuildID); role != nil {
				pingRoleID = role.ID
			}
		}
	}
	if channelID == "" {
		b.respondEphemeral(s, i, "A text channel is required.")
		return
	}

	if err := b.store.SetGuildConfig(i.GuildID, channelID, pingRoleID); err != nil {
		log.Printf("event=setup_error guild=%s err=%q", i.GuildID, err.Error())
		b.respondEphemeral(s, i, "Failed to save settings. Try again later.")
		return
	}

	msg := fmt.Sprintf("Setup saved.\n• News channel: <#%s>", channelID)
	if pingRoleID != "" {
		msg += fmt.Sprintf("\n• Ping role: <@&%s>", pingRoleID)
	} else {
		msg += "\n• Ping role: _(none)_"
	}
	msg += "\n\nThe news feed is global and the same for every server — it cannot be changed here."

	log.Printf("event=setup guild=%s channel=%s ping_role=%s", i.GuildID, channelID, pingRoleID)
	b.respondEphemeral(s, i, msg)
}

func (b *Bot) respondEphemeral(s *discordgo.Session, i *discordgo.InteractionCreate, content string) {
	err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: content,
			Flags:   discordgo.MessageFlagsEphemeral,
		},
	})
	if err != nil {
		log.Printf("event=interaction_respond_error err=%q", err.Error())
	}
}

func (b *Bot) followupEphemeral(s *discordgo.Session, i *discordgo.InteractionCreate, content string) {
	_, err := s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{
		Content: content,
		Flags:   discordgo.MessageFlagsEphemeral,
	})
	if err != nil {
		log.Printf("event=interaction_followup_error err=%q", err.Error())
	}
}

func interactionUserID(i *discordgo.InteractionCreate) string {
	if i.Member != nil && i.Member.User != nil {
		return i.Member.User.ID
	}
	if i.User != nil {
		return i.User.ID
	}
	return ""
}

// setPresence sets Idle + Watching "Anthropic News | N servers".
// Server count is how many guilds the bot is currently in (Discord state).
func (b *Bot) setPresence(s *discordgo.Session) {
	n := 0
	if s != nil && s.State != nil {
		n = len(s.State.Guilds)
	}
	label := "server"
	if n != 1 {
		label = "servers"
	}
	name := fmt.Sprintf("Anthropic News | %d %s", n, label)
	err := s.UpdateStatusComplex(discordgo.UpdateStatusData{
		Status: "idle",
		Activities: []*discordgo.Activity{
			{
				Name: name,
				Type: discordgo.ActivityTypeWatching,
			},
		},
	})
	if err != nil {
		log.Printf("event=presence_error err=%q", err.Error())
		return
	}
	log.Printf("event=presence status=idle activity=%q", name)
}

func (b *Bot) pollOnce() {
	// Refresh presence so the server count stays up to date.
	b.setPresence(b.sess)

	log.Printf("event=fetch url=%s", b.cfg.NewsSourceURL)

	items, err := fetchNews(b.cfg.NewsSourceURL)
	if err != nil {
		log.Printf("event=fetch_error err=%q", err.Error())
		return
	}
	log.Printf("event=fetch_ok count=%d", len(items))

	guilds, err := b.store.AllGuildConfigs()
	if err != nil {
		log.Printf("event=store_error op=all_guilds err=%q", err.Error())
		return
	}
	if len(guilds) == 0 {
		log.Printf("event=skip reason=no_guilds_configured")
		return
	}
	log.Printf("event=guilds_configured count=%d", len(guilds))

	// Process oldest-first so the channel timeline is chronological.
	sort.Slice(items, func(i, j int) bool {
		return items[i].Published.Before(items[j].Published)
	})

	for _, item := range items {
		b.handleItem(item, guilds)
	}
}

func (b *Bot) handleItem(item NewsItem, guilds []GuildConfig) {
	if item.ID == "" {
		log.Printf("event=skip reason=empty_id title=%q", item.Title)
		return
	}

	for _, g := range guilds {
		seen, err := b.store.Has(item.ID, g.GuildID)
		if err != nil {
			log.Printf("event=store_error op=has id=%q guild=%s err=%q", item.ID, g.GuildID, err.Error())
			continue
		}
		if seen {
			log.Printf("event=skip reason=already_posted id=%q guild=%s title=%q", item.ID, g.GuildID, item.Title)
			continue
		}
		if err := b.postItemToGuild(item, g); err != nil {
			log.Printf("event=post_error channel=%s guild=%s id=%q err=%q", g.ChannelID, g.GuildID, item.ID, err.Error())
			continue
		}
		log.Printf("event=posted channel=%s guild=%s id=%q title=%q", g.ChannelID, g.GuildID, item.ID, item.Title)
	}
}

// postItemToGuild builds the embed, sends it (with brand icon), and marks the item posted.
func (b *Bot) postItemToGuild(item NewsItem, g GuildConfig) error {
	excerpt := cropExcerpt(item.Summary, b.cfg.ExcerptLength)
	iconRef := "attachment://" + brandIconName

	embed := &discordgo.MessageEmbed{
		Title:       item.Title,
		Description: excerpt,
		URL:         item.Link,
		Color:       embedColor, // #FFFFFF white
		Thumbnail: &discordgo.MessageEmbedThumbnail{
			URL: iconRef, // AI logo (embedded brand icon)
		},
		Footer: &discordgo.MessageEmbedFooter{
			Text:    "via Anthropic Utlity", // spelling per project requirement
			IconURL: iconRef,
		},
	}
	// Large embed image = post banner/hero from the RSS entry, when available.
	if item.ImageURL != "" {
		embed.Image = &discordgo.MessageEmbedImage{URL: item.ImageURL}
	}
	if !item.Published.IsZero() {
		embed.Timestamp = item.Published.UTC().Format(time.RFC3339)
	}

	content := ""
	if g.PingRoleID != "" {
		content = fmt.Sprintf("<@&%s>", g.PingRoleID)
	}

	_, err := b.sess.ChannelMessageSendComplex(g.ChannelID, &discordgo.MessageSend{
		Content: content,
		Embeds:  []*discordgo.MessageEmbed{embed},
		Files: []*discordgo.File{
			{
				Name:   brandIconName,
				Reader: bytes.NewReader(brandIcon),
			},
		},
	})
	if err != nil {
		return err
	}
	return b.store.Mark(item.ID, g.GuildID)
}
