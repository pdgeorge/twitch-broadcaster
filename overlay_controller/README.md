# Overlay controller

The overlay controller streams Twitch chat messages from RabbitMQ to a browser overlay via WebSockets. It is designed to pair with the `twitch_receiver` + `twitch_broadcaster` stack and expose a transparent chat overlay that can be added to OBS as a browser source.

## Running locally

```bash
# from repo root
cd overlay_controller
OVERLAY_HTTP_PORT=8080 \
OVERLAY_STATIC_DIR=../overlay \
OVERLAY_DATA_DIR=../data \
RABBITMQ_URL=amqp://guest:guest@localhost:5672/ \
RABBITMQ_EXCHANGE=twitch_events \
OVERLAY_QUEUE=overlay_chat \
go run ./...
```

- The server hosts the overlay assets from `OVERLAY_STATIC_DIR` and serves them at `/`.
- WebSocket clients connect to `/ws/overlay` and receive JSON messages: `chat.message` for each line of chat, and the rest of what the overlay draws (party, tavern, dice, sounds, TTS). `FEATURES.md` in the repo root lists them all.
- The admin page (`/admin`, with its JSON API under `/admin/api/`) manages sound commands, the cosmetic catalog and what Dabi's shop sells, and has a guide to the party game. What it manages lives in `OVERLAY_DATA_DIR`: sounds in `sounds/` (served at `/sounds/`), uploaded cosmetic art and the live catalog in `cosmetics/` (served at `/cosmetics/`), and the shop's stock in `shop.json`.
- Dabi's shop is a separate page at `/shop/`. It loads the stock from `/shop/stock` and redraws on `shop.update` over the same websocket.

## Configuration

- `RABBITMQ_URL` (default `amqp://guest:guest@twitch_broadcaster:5672/`)
- `RABBITMQ_EXCHANGE` (default `twitch_events`)
- `OVERLAY_QUEUE` (default `overlay_chat`)
- `OVERLAY_HTTP_PORT` (default `8080`)
- `OVERLAY_STATIC_DIR` (default `./overlay`)
- `OVERLAY_DATA_DIR` (default `./data`; `/data` in Docker): where sounds, uploaded cosmetic art, the live cosmetic catalog and the shop's stock are kept
- `ADMIN_PASSWORD` (optional): when set, `/admin` asks for it via basic auth (any username)
- `DECK_URL` (Docker default `http://pd-streamdeck:8095`) and `DECK_TOKEN`: pd-streamdeck, which `!openshop` / `!closeshop` use to switch the OBS scene and the music. With no `DECK_URL`, the shop leaves OBS alone.
- `SHOP_SCENE` (default `Dabi-Shop`) and `SHOP_MOOD` (default `shop`): the OBS scene and pd-streamdeck music mood the shop switches to
- `DABI_EXCHANGE` (default `dabi_events`): where a sale in Dabi's shop asks Dabi to thank the buyer (`dabi.shop.purchase`, picked up by DabiReborn's stream brain). It expires after two minutes, and the Shop tab on `/admin` can switch it off.
- `LOGIN_DB_PATH` (default `/data/login_counts.db`)
- `TOKEN_CACHE_PATH` (default `/data/tokens.json`)
- `CLIENT_ID` (your Twitch application client ID)
- `CHANNEL_ID` (broadcaster ID for sending chat messages)

## Getting a broadcaster refresh token with chat scopes

Run the helper outside Docker to generate a refresh token that includes `chat:edit` and `chat:read` (plus any extra scopes you pass):

```bash
python tools/auth_code_server.py --client-id "$CLIENT_ID" --client-secret "$CLIENT_SECRET" \
  --redirect-uri "http://localhost:17563/callback" --scopes chat:edit chat:read
```

Open the printed URL in your browser, authorize as the broadcaster, and the script will exchange the code for tokens. Copy the `refresh_token` into your `.env` as `REFRESH_TOKEN` so the stack can refresh it automatically.

If you want to request a "kitchen sink" token, add `--all-scopes` to reuse the helper's curated list of Twitch scopes (the list omits retired/invalid scopes such as `channel:manage:whispers` to avoid 400 errors):

```bash
python tools/auth_code_server.py --client-id "$CLIENT_ID" --client-secret "$CLIENT_SECRET" \
  --redirect-uri "http://localhost:17563/callback" --all-scopes
```

## Message format

WebSocket payloads use this shape:

```json
{
  "type": "chat.message",
  "message_id": "b2f6c6c8-…",
  "user_id": "67890",
  "username": "example_user",
  "channel_id": "12345",
  "channel_login": "example_user",
  "fragments": [
    { "type": "text", "text": "hello" },
    { "type": "emote", "text": "Kappa", "emote_url": "https://static-cdn.jtvnw.net/emoticons/v2/25/static/dark/2.0" }
  ],
  "message": "hello Kappa",
  "message_html": "<span class='username'>example_user</span>: hello <img class='emote' src='...'>"
}
```

- `message_html` is pre-rendered with Twitch emotes for compatibility.
- `fragments` and `message` allow the overlay JS to swap in BTTV/FFZ/7TV emotes client-side while reusing Twitch emote URLs.
- `message_id` and `user_id` let the overlay take lines back off screen. When a mod deletes a message it gets `{"type": "chat.delete", "message_id": …}`, when a timeout or ban clears a chatter's messages `{"type": "chat.clear_user", "user_id": …}`, and on a `/clear` `{"type": "chat.clear"}`. `tts.play` carries the same two ids, so a party member's line still waiting to be read out is dropped too.
