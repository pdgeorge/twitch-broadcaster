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

WebSocket payloads use this shape:

```json
{
  "type": "chat.message",
  "badges": ["broadcaster"],
  "username": "example_user",
  "message": "hello world",
  "message_html": "<span class='badges'>[broadcaster]</span> <span class='username'>example_user</span>: hello world"
}
```

Badges are optional and derived from the EventSub payload; message and username are HTML-escaped for display safety.
