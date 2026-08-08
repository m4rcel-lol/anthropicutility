# Anthropic Utility

Minimal Go Discord bot that polls a global Anthropic news RSS/Atom feed and posts new entries as embeds.

## Features

- Polls a single global feed (`NEWS_SOURCE_URL`) on a fixed interval
- Per-server `/setup` (channel + optional ping role)
- `/info` — feed status, newest item, posts anything not yet delivered to this server
- `/credits` — author and stack
- SQLite history so items are not re-posted after restarts
- Embeds: white accent, AI logo thumbnail/footer, post banner image when available
- Presence: Idle · Watching `Anthropic News | N servers`

## Commands

| Command | Who | What |
|---------|-----|------|
| `/setup channel:#… [ping_role:@…]` | Manage Server / Admin | Configure this server (once) |
| `/info` | Anyone | Feed info + check/post new items |
| `/credits` | Anyone | Credits and tech stack |

## Discord invite

OAuth2 URL Generator:

- Scopes: `bot`, `applications.commands`
- Permissions: **Send Messages**, **Embed Links** (and role mention ability if you use a ping role)

## Configuration

```bash
cp .env.example .env
# edit .env — set DISCORD_BOT_TOKEN (never commit it)
```

| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| `DISCORD_BOT_TOKEN` | yes | — | Bot token |
| `NEWS_SOURCE_URL` | yes | (see `.env.example`) | Global RSS/Atom URL |
| `POLL_INTERVAL_MINUTES` | no | `15` | Poll interval |
| `EXCERPT_LENGTH` | no | `280` | Embed excerpt length |
| `SQLITE_PATH` | no | `/data/bot.db` | SQLite path in container |

Anthropic does not publish an official RSS feed. The example URL is a community mirror — confirm it still works before production use.

## Run

```bash
docker compose up -d --build
```

Named volume `bot-data` persists `/data/bot.db`.

## Layout

```
main.go              config + entrypoint
bot.go               Discord, commands, presence, posting
fetch.go             RSS/Atom fetch + parse + excerpt
store.go             SQLite posted_items + guild_config
assets/icon.jpg      embed thumbnail / footer icon
Dockerfile           multi-stage → static binary on scratch
docker-compose.yml
.env.example
```

## Credits

> Built by [m4rcel-lol](https://github.com/m4rcel-lol), with little help of Claude.
