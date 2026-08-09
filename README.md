# Anthropic Utility

Minimal Go Discord bot that polls a global Anthropic news RSS/Atom feed and posts new entries as embeds.

## Features

- Polls a single global feed (`NEWS_SOURCE_URL`) on a fixed interval
- Per-server `/setup` (channel + optional ping role), re-runnable to edit a wrong setup
- `/info` — feed status, newest item, posts anything not yet delivered to this server
- `/postall` — force-post the whole feed to this server's channel, ignoring history
- `/credits` — author and stack
- SQLite history, tracked **per server**, so items are not re-posted after restarts
- Embeds: white accent, AI logo thumbnail/footer, post banner image when available
- Presence: Idle · Watching `Anthropic News | N servers`

## Commands

| Command | Who | What |
|---------|-----|------|
| `/setup [channel:#…] [ping_role:@…] [remove_ping_role:true]` | Manage Server / Admin | Configure this server, or edit an existing setup |
| `/info` | Anyone | Feed info + check/post new items for this server |
| `/postall [ping:true\|false]` | Manage Server / Admin | Post **every** feed entry to this server's channel, even ones already posted |
| `/credits` | Anyone | Credits and tech stack |

### Editing a setup

Running `/setup` again in a server that is already configured **edits** it and
reports exactly what changed (`old → new`), rather than refusing. Options you
omit keep their current value, so a wrong channel can be corrected without
restating the ping role:

- `/setup channel:#correct-channel` — move the news channel, keep the role
- `/setup ping_role:@News` — change the role, keep the channel
- `/setup remove_ping_role:true` — stop mentioning any role
- Re-running with identical values reports **Nothing changed**

`channel` is only required the first time. Setting `ping_role` and
`remove_ping_role:true` in the same call is rejected. Editing a setup does not
re-post already-delivered items — use `/postall` for that.

### Per-server delivery history

Delivery is tracked per `(item, server)`, so each server has its own "already
posted" state — a server added later still receives everything it has not seen.
`/info` reports failures explicitly instead of claiming the channel is up to
date, so missing channel permissions are visible rather than silent.

`/postall` ignores that history entirely: it posts every entry currently in the
feed to the channel from `/setup`, mentioning the configured ping role (pass
`ping:false` to post the same backlog without the mention). Sends are spaced out
to stay within Discord's per-channel rate limit. Use it to seed a server that was
set up after the news had already been published elsewhere.

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

On startup the store upgrades a database written by an older build, where
`posted_items` was keyed by item id alone and dedup was global across all
servers. Existing rows are attributed to every server configured at that moment,
so no server is spammed with its backlog; use `/postall` to seed a server on
purpose.

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
