# Twitch broadcaster stack

This repository defines a Twitch EventSub receiver that republishes channel events to RabbitMQ so they can be consumed by any downstream service. A helper `desktop_tools/background.py` example shows how to listen for events and print them to the terminal.

For a one-page summary of what's on stream, the overlay's pieces and the known issues, see `FEATURES.md`.

## Prerequisites

- Docker and Docker Compose
- Twitch application credentials (`CLIENT_ID`, `CLIENT_SECRET`)
- A refresh token with the channel-related scopes required by Twitch EventSub (generate it once and reuse it for long-running refreshes)

## Environment variables

Copy `.env.example` to `.env` and fill in the Twitch and RabbitMQ values:

```
CLIENT_ID=your_twitch_client_id
CLIENT_SECRET=your_twitch_client_secret
REFRESH_TOKEN=your_initial_refresh_token
CHANNEL_ID=your_broadcaster_channel_id

# Optional overrides
RABBITMQ_URL=amqp://guest:guest@twitch_broadcaster:5672/
TOKEN_CACHE_PATH=/data/tokens.json
RABBITMQ_EXCHANGE=twitch_events
```

`REFRESH_TOKEN` is refreshed automatically on startup and every 80% of the token lifetime. The latest tokens are written to `TOKEN_CACHE_PATH`, which you can mount as a volume if you want to persist them. Make sure the refresh token carries the scopes required for all Twitch channel EventSub topics you want to consume (see the Twitch API reference for the current list).

## Running the stack

1. Build and start the services:
   ```
   docker compose up --build
   ```
   - `twitch_broadcaster` is a RabbitMQ broker with the management UI exposed on port `15672`.
   - `twitch-receiver` connects to Twitch EventSub over WebSockets, subscribes to all channel events for `CHANNEL_ID`, and publishes them to the `twitch_events` exchange in RabbitMQ.
   - A named Docker volume (`tokens`) is mounted at `/data` inside `twitch-receiver` so refreshed tokens persist across restarts.
   - `overlay_controller` consumes chat messages from RabbitMQ, serves the overlay assets from `overlay/`, and exposes a WebSocket endpoint at `/ws/overlay` for the overlay page. It also serves the admin page at `/admin` and Dabi's shop at `/shop`, and keeps what `/admin` manages (sounds, cosmetic art, the live cosmetic catalog, the shop's stock) in `./data`, mounted at `/data`.

2. (Optional) Tail the receiver logs:
   ```
   docker compose logs -f twitch-receiver
   ```

### Event names

Messages published to RabbitMQ use the Twitch EventSub type as the `properties.type` on each message (e.g., `channel.follow`, `channel.cheer`). This keeps event names meaningful and consistent with the Twitch API reference.

## Consuming events with `background.py`

`desktop_tools/background.py` is a lightweight example that binds a queue to the `twitch_events` fanout exchange. Like everything in `desktop_tools/` it runs on the streaming desktop rather than the Pi. It ships with helper handlers so you can pick which events to print:

- `handle_all_events` (commented out by default) for every Twitch event.
- `handle_channel_points_only` for only `channel.channel_points_custom_reward_redemption.add`.
- `handle_chat_only` for only `channel.chat.message` events.
- `handle_chat_and_rewards` (default) for both chat messages and channel point redemptions.

```
pip install pika
python desktop_tools/background.py
```

Environment variables you can override:

- `RABBITMQ_URL` (default `amqp://guest:guest@localhost:5672/`)
- `RABBITMQ_EXCHANGE` (default `twitch_events`)
- `QUEUE_NAME` (default `twitch_event_printer`)

With the Docker Compose stack running, `background.py` will start printing JSON payloads for each Twitch channel event.

## Chat overlay (OBS-friendly)

An overlay controller service streams `channel.chat.message` events to browser clients.

- Overlay assets live in `overlay/` (`index.html`, `overlay.css`, `overlay.js`). The CSS positions the chat box (top/left) so you can adjust placement without code changes.
- The Go backend lives in `overlay_controller/` and listens on `OVERLAY_HTTP_PORT` (default `8080`). The overlay is served at `/` and the WebSocket endpoint is `/ws/overlay`.
- When running via Docker Compose, the overlay is available at `http://localhost:${OVERLAY_HTTP_PORT}`. Add this URL as a browser source in OBS with a transparent background.
- Twitch emotes are expanded server-side; the overlay page also auto-loads BTTV/FFZ/7TV global + channel emotes for richer chat rendering.
- Every chat message is also saved to the `chat_log` MySQL table. Messages a mod deletes, or that a timeout/ban clears, are kept but flagged `deleted = TRUE`.
- Moderation reaches the stream too: when a mod deletes a message, times out or bans a chatter, or clears chat, those lines come off the on-screen chat, and any party TTS of them that hasn't finished is dropped, or cut off if it's playing. A `/clear` leaves the chat log unflagged, since it doesn't single anyone out.
- Environment variables for the overlay controller: `RABBITMQ_URL`, `RABBITMQ_EXCHANGE`, `OVERLAY_QUEUE`, `OVERLAY_HTTP_PORT`, `OVERLAY_STATIC_DIR`, `OVERLAY_DATA_DIR`, `ADMIN_PASSWORD`.

### Overlay geometry (1920×1080 canvas, all set in `overlay/overlay.css`)

- **Billboard (`.other-box`)**: 1160×148 px box at left 727, top 918. Padding (16px sides, 8px top/bottom) leaves a 1128×132 px content area; anything that doesn't fit is clipped (`overflow: hidden`), and text renders at 28px (`clamp(16px, 1.6vw, 28px)`) on a 1920-wide canvas.
- **Chat box (`#chat-box`)**: 330 px wide at top 20, left 20, max-height 64vh (~691 px at 1080p). Monospace 16px ⇒ lines wrap at roughly 34 characters, including the `username:` prefix.
- **Tavern walking strip (`#tavern-area`)**: bottom of screen, left 25% → right edge, 260 px tall. Backdrop is `overlay/assets/tavern_bg.png` (1440×260: a small tavern on the right behind a grass/dirt floor, transparent above), regenerated with `python3 tools/gen_tavern_bg.py` (needs Pillow). The billboard and party cards draw over it.
- **Party cards (`.party-cards`)**: anchored bottom-right (right 20, bottom 172), 116 px per card, growing leftward.

### Chat commands

- **`!other <text>`** (broadcaster/moderators): replaces the lower box with your message. Basic Markdown is rendered (e.g., `*italic*`, `**bold**`, lists, headings).
- **Channel point reward titled “announcement”**: when redeemed, the lower box shows the submitted text (Markdown supported) for five minutes.
- **Channel point rewards titled “daily login bonus” and “first login bonus”**: each redeem adds one to that chatter's count (`logins` / `first_logins` in the `chatters` table) and the bot replies in chat with the new total, e.g. “@name you've logged in first 3 times!”.
- **`!fire`** (broadcaster/moderators): clears any active announcement early and restores the last `!other` content.

### Sound commands

Any chatter can type `!<name>` to play a sound through the overlay: `!buhee` plays `buhee.wav`. Only the first word counts, so `!buhee lol` plays it too. **`!sounds`** (any chatter) lists them all in chat.

- Each sound has its own cooldown, shared by everyone: once `!buhee` plays, nobody can set it off again until its cooldown runs out (5 seconds unless you change it, 0 for none). Attempts during the cooldown are ignored without a reply.
- Each sound has a volume (0–100%), applied in the browser source. Sounds play straight away and can overlap each other and party TTS.
- The sounds are the files in `data/sounds/` (wav, mp3 or ogg; the file name is the command), with their settings in `data/sounds/sounds.json`. Manage them from the admin page. A file copied in by hand works too, on the default settings, since the folder is read fresh on every command.
- Names that a built-in command (`!roll`, `!give`, `!dabi`, …) or a `#a` command already answers can't be used.

### Admin page (`/admin`)

`http://dabi:8080/admin` manages sounds, cosmetics and Dabi's shop from a browser. Every change is live on stream straight away, with no restart.

- **Sounds**: drop in audio files (each becomes `!<file name>`), set cooldown and volume (saved as you leave the field), rename, swap in a better version of the file, play it on stream to check its level, delete.
- **Cosmetics**: drop in PNGs (each becomes a cosmetic named after its file; pick its slot), replace an item's art (everyone who owns it sees the new version), move it to another slot, delete it, and add, reorder or remove slots. Uploads are checked: a PNG, the same 500×600 shape as `Dabling.png`, with a transparent background.
- **Shop**: what each of the three boxes on Dabi's stall sells and its price in channel points, with a live preview of `/shop`. See "Dabi's shop" below.
- **Party guide**: a reminder of how the party game runs: the channel-point rewards, joining, exp and a level table, death and revival, and every command. Its numbers come from the controller, so it stays right when you tune them.

Everything it manages lives in `data/` on the Pi (`/data` in the container), not in the git checkout, so `git pull` never trips over it. Back up `data/` along with the MySQL volume. There's no login by default since it's meant for the LAN/Tailscale; set `ADMIN_PASSWORD` in `.env` and the browser asks for it (any username).

### Cosmetics (Dabling outfits)

Chatters can own several cosmetics and pick which ones their Dabling wears, one per slot (e.g. one shirt at a time). Worn items show on the tavern floor and on party cards; the Dabling's hue tint still applies to the body, never to the cosmetic.

- **`!give <name> <cosmetic>`** (broadcaster/moderators): grants an item from the catalog (anything else is rejected). If that slot is empty it's worn straight away. `!give <name> <number>` still grants money.
- **`!take <name> <cosmetic>`** (broadcaster/moderators): removes an item (and takes it off). Also works on old entries the catalog doesn't know.
- **`!wardrobe`** (any chatter): lists what you own and what you're wearing.
- **`!equip <cosmetic>`** / **`!unequip <cosmetic or slot>`** (any chatter): change your own outfit.

Add and edit cosmetics on the admin page. The live catalog is `data/cosmetics/cosmetics.json`. Cosmetics can also ship with the code, in the starter catalog (`overlay/assets/cosmetics/cosmetics.json`): any starter item the live catalog hasn't seen yet is merged into it, on first start and whenever a `git pull` changes the starter catalog, with no restart needed. The live catalog's `seeded` list remembers which starter items it has taken in, so one you delete or redraw in `/admin` stays that way. `slots` sets the draw order (first is drawn lowest), and each item names its slot and its image. Every image is a transparent PNG on the same 500×600 canvas as `overlay/assets/Dabling.png`, drawn in place over the body, so it lines up at any size. Uploaded images are stored in `data/cosmetics/` (served at `/cosmetics/`); the starter ones stay in `overlay/assets/cosmetics/`. Hand edits to the live catalog still work: the controller re-reads it when it changes, and a broken edit is logged while the previous catalog stays in use. Sprites are mirrored when a Dabling walks left, so any lettering on a cosmetic will read backwards half the time. `shirtA`/`shirtB`/`shirtC` are placeholder art.

**Automatic rewards.** Some items are handed out on their own when a chatter's counter reaches a milestone. Redeeming “first login bonus” for the first time grants `bronze_first` (a bronze shirt with a “1” medallion, drawn by `python3 tools/gen_bronze_first.py`), and the bot announces “@name you have received the bronze_first shirt!”. As with `!give`, the item is put on only if that slot is empty. Each reward lands once: an owned item is never re-granted. To add another, draw the item, add it to the starter catalog (`overlay/assets/cosmetics/cosmetics.json`, which reaches the live one on the Pi by itself), and add a line to `autoRewards` in `overlay_controller/main.go` naming the counter (`logins` or `first_logins`), the count it unlocks at, the item, and what the announcement calls it. A test checks that every reward's item is in the shipped catalog.

### Dabi's shop (`/shop`)

Dabi's market stall, where chatters buy cosmetics with channel points. It goes live with Dabi on 1 December. Built so far: the stall and what it sells. Still to come: the "buy item 1/2/3" channel-point rewards (on only while the shop is open), `!openshop` / `!closeshop` switching OBS to the Dabi-Shop scene with its music, and Dabi thanking buyers.

- `http://dabi:8080/shop/` is the browser source for the Dabi-Shop scene, at 1920×1080. It draws the stall with a base (untinted) Dabling in each box wearing that box's item, so chat can see how it looks on, and the price on the tag under it. It redraws as soon as the stock changes. Dabi sits on the stool, added on top of it in OBS.
- The Shop tab on `/admin` picks what each box sells, or leaves it empty, and sets prices. A new item costs 300 channel points (what Twitch gives for a follow), and an item keeps its price when it leaves the shop and comes back. One item can't be in two boxes, and deleting a cosmetic empties its box.
- The stock lives in `data/shop.json`. `/shop` loads it from `GET /shop/stock` and gets changes as `shop.update` on `/ws/overlay`.
- The stall art (`overlay/shop/stall.png`) is built in Blender by `tools/shop_stall.py`, the same way Dabi is built in vtuber-george: every piece comes from code, so changing a number and running it again gives a new render. Run `blender -b --factory-startup --python tools/shop_stall.py -- overlay/shop` from the repo root (Blender 5.2). It also writes `overlay/shop/layout.json`, where each box's backing, each price tag and the stool's seat landed in the render, which is how `/shop` lines the items and prices up with the art. Add `--transparent` after the folder for the stall alone, without the sky and grass.

## Tests

The game logic that guards persistent character state (level curve, exp/HP rules, party and tavern membership, cosmetic ownership and equip rules, DM-command authorization, chat HTML escaping, moderation taking messages off the chat and party TTS) is covered by `overlay_controller/main_test.go`; sound commands (names, per-sound cooldowns, the folder scan) by `sounds_test.go`; the shop's stock (restocking, prices, what it refuses, surviving a restart) by `shop_test.go`; and the admin page's guards (password, cross-site writes, upload checks, catalog edits, never deleting shipped art) and the party guide's numbers by `admin_test.go`. Run `go test ./...` from `overlay_controller/`. The plumbing (websockets, RabbitMQ, MySQL, the overlay page itself) is deliberately untested: it fails loudly, the math fails silently.

## Notes on long-running operation

- Tokens are refreshed on startup and then every 80% of their reported lifetime to keep the container running indefinitely (including resource-constrained devices such as Raspberry Pi).
- If Twitch requests a WebSocket reconnect, the receiver follows the provided `reconnect_url` and re-subscribes to all channel events automatically.
- New refresh tokens are persisted to `TOKEN_CACHE_PATH` so that subsequent restarts reuse the freshest token.
