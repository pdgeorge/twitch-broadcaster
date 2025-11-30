# Overlay controller

The overlay controller streams Twitch chat messages from RabbitMQ to a browser overlay via WebSockets. It is designed to pair with the `twitch_receiver` + `twitch_broadcaster` stack and expose a transparent chat overlay that can be added to OBS as a browser source.

## Running locally

```bash
# from repo root
cd overlay-controller
OVERLAY_HTTP_PORT=8080 \
OVERLAY_STATIC_DIR=../overlay \
RABBITMQ_URL=amqp://guest:guest@localhost:5672/ \
RABBITMQ_EXCHANGE=twitch_events \
OVERLAY_QUEUE=overlay_chat \
go run ./...
```

- The server hosts the overlay assets from `OVERLAY_STATIC_DIR` and serves them at `/`.
- WebSocket clients connect to `/ws/overlay` and receive JSON messages for `chat.message`.

## Configuration

- `RABBITMQ_URL` (default `amqp://guest:guest@twitch_broadcaster:5672/`)
- `RABBITMQ_EXCHANGE` (default `twitch_events`)
- `OVERLAY_QUEUE` (default `overlay_chat`)
- `OVERLAY_HTTP_PORT` (default `8080`)
- `OVERLAY_STATIC_DIR` (default `./overlay`)

## Message format

WebSocket payloads use these shapes:

### Chat

```json
{
  "type": "chat.message",
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

### Other box

Base/announcement updates:

```json
{
  "type": "other.update",
  "mode": "base|announcement|base_restore|force_restore",
  "html": "<h1>Headline</h1><p>Body</p>",
  "duration_seconds": 300
}
```

Pong animation:

```json
{ "type": "other.pong_start", "duration_seconds": 60 }
{ "type": "other.pong_frame", "html": "<pre>...frame...</pre>" }
{ "type": "other.pong_end" }
```

Commands and triggers:
- `!other <markdown>` from broadcaster/mod: sets the base Markdown content shown in the Other box.
- `!fire` from broadcaster/mod: cancels an active announcement and restores the base content.
- Channel point redeem titled `announcement`: temporarily overrides the Other box for 5 minutes with the redeem text (Markdown).
- Pong: starts automatically every 15 minutes or when any chat message contains "ping" (case-insensitive); runs 60 seconds, then restores the base content.
