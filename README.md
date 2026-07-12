# Twitch broadcaster stack

This repository defines a Twitch EventSub receiver that republishes channel events to RabbitMQ so they can be consumed by any downstream service. A helper `background.py` example shows how to listen for events and print them to the terminal.

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
   - `overlay_controller` consumes chat messages from RabbitMQ, serves the overlay assets from `overlay/`, and exposes a WebSocket endpoint at `/ws/overlay` for the overlay page.

2. (Optional) Tail the receiver logs:
   ```
   docker compose logs -f twitch-receiver
   ```

### Event names

Messages published to RabbitMQ use the Twitch EventSub type as the `properties.type` on each message (e.g., `channel.follow`, `channel.cheer`). This keeps event names meaningful and consistent with the Twitch API reference.

## Consuming events with `background.py`

`background.py` is a lightweight example that binds a queue to the `twitch_events` fanout exchange. It ships with helper handlers so you can pick which events to print:

- `handle_all_events` (commented out by default) for every Twitch event.
- `handle_channel_points_only` for only `channel.channel_points_custom_reward_redemption.add`.
- `handle_chat_only` for only `channel.chat.message` events.
- `handle_chat_and_rewards` (default) for both chat messages and channel point redemptions.

```
pip install pika
python background.py
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
- Environment variables for the overlay controller: `RABBITMQ_URL`, `RABBITMQ_EXCHANGE`, `OVERLAY_QUEUE`, `OVERLAY_HTTP_PORT`, `OVERLAY_STATIC_DIR`.

### Overlay geometry (1920×1080 canvas, all set in `overlay/overlay.css`)

- **Billboard (`.other-box`)**: 1160×148 px box at left 727, top 918. Padding (16px sides, 8px top/bottom) leaves a 1128×132 px content area; anything that doesn't fit is clipped (`overflow: hidden`), and text renders at 28px (`clamp(16px, 1.6vw, 28px)`) on a 1920-wide canvas.
- **Chat box (`#chat-box`)**: 330 px wide at top 20, left 20, max-height 64vh (~691 px at 1080p). Monospace 16px ⇒ lines wrap at roughly 34 characters, including the `username:` prefix.
- **Tavern walking strip (`#tavern-area`)**: bottom of screen, left 25% → right edge, 260 px tall (placeholder until real background art).
- **Party cards (`.party-cards`)**: anchored bottom-right (right 20, bottom 172), 116 px per card, growing leftward.

### Chat commands

- **`ping`** (any chatter): starts a brief Pong animation in the lower “other” box for about a minute.
- **`!other <text>`** (broadcaster/moderators): replaces the lower box with your message. Basic Markdown is rendered (e.g., `*italic*`, `**bold**`, lists, headings).
- **Channel point reward titled “announcement”**: when redeemed, the lower box shows the submitted text (Markdown supported) for five minutes.
- **`!fire`** (broadcaster/moderators): clears any active announcement early and restores the last `!other` content.

## Notes on long-running operation

- Tokens are refreshed on startup and then every 80% of their reported lifetime to keep the container running indefinitely (including resource-constrained devices such as Raspberry Pi).
- If Twitch requests a WebSocket reconnect, the receiver follows the provided `reconnect_url` and re-subscribes to all channel events automatically.
- New refresh tokens are persisted to `TOKEN_CACHE_PATH` so that subsequent restarts reuse the freshest token.
