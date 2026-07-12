# Desktop tools

Generic standalone scripts that run on the streaming desktop, not the Pi — they need things the Pi doesn't have (local network position, OBS, a screen).

- `background.py` — example RabbitMQ consumer: binds a queue to the `twitch_events` fanout exchange and prints events to the terminal. See the "Consuming events" section of the root README.
- `OBS_Websocket.py` — generic OBS websocket controller (`OBSWebsocketsManager`): scene/source manipulation over obs-websocket. Reusable; the archived horsey project was its only consumer so far.
