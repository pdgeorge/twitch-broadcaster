import asyncio
import json
import logging
import os
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Dict, List

import aiohttp
import aio_pika
import backoff
from dotenv import load_dotenv

load_dotenv()

logging.basicConfig(level=logging.INFO, format="[%(asctime)s] %(levelname)s %(name)s: %(message)s")
LOGGER = logging.getLogger("twitch_receiver")

TWITCH_EVENTSUB_WS = "wss://eventsub.wss.twitch.tv/ws"
TWITCH_TOKEN_URL = "https://id.twitch.tv/oauth2/token"
TWITCH_EVENTSUB_URL = "https://api.twitch.tv/helix/eventsub/subscriptions"
TWITCH_CHAT_MESSAGES_URL = "https://api.twitch.tv/helix/chat/messages"


@dataclass
class OAuthTokens:
    access_token: str
    refresh_token: str
    expires_in: int


class TokenManager:
    def __init__(self, client_id: str, client_secret: str, refresh_token: str, cache_path: Path):
        self.client_id = client_id
        self.client_secret = client_secret
        self.refresh_token = refresh_token
        self.cache_path = cache_path
        self.access_token = ""
        self.expires_in = 0

    async def refresh(self, session: aiohttp.ClientSession) -> OAuthTokens:
        payload = {
            "client_id": self.client_id,
            "client_secret": self.client_secret,
            "grant_type": "refresh_token",
            "refresh_token": self.refresh_token,
        }
        LOGGER.info("Refreshing Twitch token")
        async with session.post(TWITCH_TOKEN_URL, data=payload) as resp:
            resp.raise_for_status()
            body = await resp.json()

        tokens = OAuthTokens(
            access_token=body["access_token"],
            refresh_token=body.get("refresh_token", self.refresh_token),
            expires_in=int(body.get("expires_in", 3600)),
        )
        self.access_token = tokens.access_token
        self.refresh_token = tokens.refresh_token
        self.expires_in = tokens.expires_in
        self._write_cache(tokens)
        LOGGER.info("Token refreshed; expires_in=%s", tokens.expires_in)
        return tokens

    def _write_cache(self, tokens: OAuthTokens) -> None:
        try:
            self.cache_path.parent.mkdir(parents=True, exist_ok=True)
            with self.cache_path.open("w", encoding="utf-8") as fh:
                json.dump(tokens.__dict__, fh)
        except Exception as exc:  # pragma: no cover - best-effort
            LOGGER.warning("Failed to write token cache: %s", exc)

    def load_cache(self) -> None:
        if not self.cache_path.exists():
            return
        try:
            with self.cache_path.open("r", encoding="utf-8") as fh:
                data = json.load(fh)
            self.refresh_token = data.get("refresh_token", self.refresh_token)
            self.access_token = data.get("access_token", "")
            self.expires_in = int(data.get("expires_in", 0))
            LOGGER.info("Loaded cached tokens from %s", self.cache_path)
        except Exception as exc:  # pragma: no cover - best-effort
            LOGGER.warning("Failed to load token cache: %s", exc)


class RabbitPublisher:
    def __init__(self, url: str, exchange_name: str):
        self.url = url
        self.exchange_name = exchange_name
        self.connection: aio_pika.RobustConnection | None = None
        self.channel: aio_pika.abc.AbstractRobustChannel | None = None
        self.exchange: aio_pika.Exchange | None = None

    async def connect(self) -> None:
        LOGGER.info("Connecting to RabbitMQ at %s", self.url)
        self.connection = await aio_pika.connect_robust(self.url)
        self.channel = await self.connection.channel()
        self.exchange = await self.channel.declare_exchange(self.exchange_name, aio_pika.ExchangeType.FANOUT, durable=True)
        LOGGER.info("RabbitMQ ready: exchange=%s", self.exchange_name)

    async def publish(self, event_type: str, payload: Dict[str, Any]) -> None:
        if not self.exchange:
            raise RuntimeError("Exchange not initialized")
        body = json.dumps(payload).encode()
        message = aio_pika.Message(body=body, type=event_type)
        await self.exchange.publish(message, routing_key="")
        LOGGER.debug("Published event %s", event_type)


class RabbitCommandConsumer:
    def __init__(self, url: str, exchange_name: str, queue_name: str):
        self.url = url
        self.exchange_name = exchange_name
        self.queue_name = queue_name
        self.connection: aio_pika.RobustConnection | None = None
        self.channel: aio_pika.abc.AbstractRobustChannel | None = None
        self.exchange: aio_pika.Exchange | None = None
        self.queue: aio_pika.Queue | None = None

    async def connect(self) -> None:
        LOGGER.info("Connecting command consumer to RabbitMQ at %s", self.url)
        self.connection = await aio_pika.connect_robust(self.url)
        self.channel = await self.connection.channel()
        await self.channel.set_qos(prefetch_count=10)
        self.exchange = await self.channel.declare_exchange(self.exchange_name, aio_pika.ExchangeType.FANOUT, durable=True)
        self.queue = await self.channel.declare_queue(self.queue_name, durable=True)
        await self.queue.bind(self.exchange, routing_key="")
        LOGGER.info("Command consumer ready: exchange=%s queue=%s", self.exchange_name, self.queue_name)

    async def consume(self, handler) -> None:
        if not self.queue:
            raise RuntimeError("Queue not initialized")
        await self.queue.consume(handler)


class TwitchEventSubClient:
    def __init__(self, tokens: TokenManager, channel_id: str, publisher: RabbitPublisher):
        self.tokens = tokens
        self.channel_id = channel_id
        self.publisher = publisher
        self.session: aiohttp.ClientSession | None = None

    async def _ensure_session(self) -> aiohttp.ClientSession:
        if not self.session:
            self.session = aiohttp.ClientSession()
        return self.session

    async def _auth_headers(self) -> Dict[str, str]:
        return {
            "Client-Id": self.tokens.client_id,
            "Authorization": f"Bearer {self.tokens.access_token}",
            "Content-Type": "application/json",
        }

    def _subscriptions(self) -> List[Dict[str, Any]]:
        broadcaster_condition = {"broadcaster_user_id": self.channel_id}
        moderator_condition = {
            "broadcaster_user_id": self.channel_id,
            "moderator_user_id": self.channel_id,
        }
        chat_condition = {
            "broadcaster_user_id": self.channel_id,
            "user_id": self.channel_id,
        }
        return [
            {"type": "channel.update", "version": "2", "condition": broadcaster_condition},
            {"type": "channel.follow", "version": "2", "condition": {"broadcaster_user_id": self.channel_id, "moderator_user_id": self.channel_id}},
            {"type": "channel.subscribe", "version": "1", "condition": broadcaster_condition},
            {"type": "channel.subscription.end", "version": "1", "condition": broadcaster_condition},
            {"type": "channel.subscription.gift", "version": "1", "condition": broadcaster_condition},
            {"type": "channel.subscription.message", "version": "1", "condition": broadcaster_condition},
            {"type": "channel.cheer", "version": "1", "condition": broadcaster_condition},
            {"type": "channel.raid", "version": "1", "condition": {"to_broadcaster_user_id": self.channel_id}},
            {"type": "channel.ban", "version": "1", "condition": moderator_condition},
            {"type": "channel.unban", "version": "1", "condition": moderator_condition},
            {"type": "channel.moderator.add", "version": "1", "condition": moderator_condition},
            {"type": "channel.moderator.remove", "version": "1", "condition": moderator_condition},
            {"type": "channel.channel_points_custom_reward.add", "version": "1", "condition": moderator_condition},
            {"type": "channel.channel_points_custom_reward.update", "version": "1", "condition": moderator_condition},
            {"type": "channel.channel_points_custom_reward.remove", "version": "1", "condition": moderator_condition},
            {"type": "channel.channel_points_custom_reward_redemption.add", "version": "1", "condition": moderator_condition},
            {"type": "channel.channel_points_custom_reward_redemption.update", "version": "1", "condition": moderator_condition},
            {"type": "channel.poll.begin", "version": "1", "condition": broadcaster_condition},
            {"type": "channel.poll.progress", "version": "1", "condition": broadcaster_condition},
            {"type": "channel.poll.end", "version": "1", "condition": broadcaster_condition},
            {"type": "channel.prediction.begin", "version": "1", "condition": broadcaster_condition},
            {"type": "channel.prediction.progress", "version": "1", "condition": broadcaster_condition},
            {"type": "channel.prediction.lock", "version": "1", "condition": broadcaster_condition},
            {"type": "channel.prediction.end", "version": "1", "condition": broadcaster_condition},
            {"type": "channel.charity_campaign.donate", "version": "1", "condition": broadcaster_condition},
            {"type": "channel.charity_campaign.start", "version": "1", "condition": broadcaster_condition},
            {"type": "channel.charity_campaign.progress", "version": "1", "condition": broadcaster_condition},
            {"type": "channel.charity_campaign.stop", "version": "1", "condition": broadcaster_condition},
            {"type": "channel.goal.begin", "version": "1", "condition": broadcaster_condition},
            {"type": "channel.goal.progress", "version": "1", "condition": broadcaster_condition},
            {"type": "channel.goal.end", "version": "1", "condition": broadcaster_condition},
            {"type": "channel.hype_train.begin", "version": "1", "condition": broadcaster_condition},
            {"type": "channel.hype_train.progress", "version": "1", "condition": broadcaster_condition},
            {"type": "channel.hype_train.end", "version": "1", "condition": broadcaster_condition},
            {"type": "channel.shield_mode.begin", "version": "1", "condition": moderator_condition},
            {"type": "channel.shield_mode.end", "version": "1", "condition": moderator_condition},
            {"type": "channel.shoutout.create", "version": "1", "condition": moderator_condition},
            {"type": "channel.shoutout.receive", "version": "1", "condition": moderator_condition},
            {"type": "channel.vip.add", "version": "1", "condition": moderator_condition},
            {"type": "channel.vip.remove", "version": "1", "condition": moderator_condition},
            {"type": "channel.chat.message", "version": "1", "condition": chat_condition},
        ]

    async def subscribe_all(self, session_id: str) -> None:
        session = await self._ensure_session()
        headers = await self._auth_headers()
        subscriptions = self._subscriptions()
        for subscription in subscriptions:
            body = {
                "type": subscription["type"],
                "version": subscription["version"],
                "condition": subscription["condition"],
                "transport": {"method": "websocket", "session_id": session_id},
                "cost": 1,
            }
            async with session.post(TWITCH_EVENTSUB_URL, headers=headers, json=body) as resp:
                if resp.status != 202:
                    detail = await resp.text()
                    LOGGER.error("Failed to subscribe to %s: %s", subscription["type"], detail)
                else:
                    LOGGER.info("Subscribed to %s", subscription["type"])

    async def send_chat(self, message: str, channel_id: str | None = None) -> None:
        channel = channel_id or self.channel_id
        if not channel:
            raise RuntimeError("Missing channel_id for chat send")
        if not message:
            raise RuntimeError("Missing message for chat send")

        session = await self._ensure_session()
        headers = await self._auth_headers()
        payload = {
            "broadcaster_id": channel,
            "sender_id": channel,
            "message": message,
        }
        async with session.post(TWITCH_CHAT_MESSAGES_URL, headers=headers, json=payload) as resp:
            if resp.status >= 300:
                detail = await resp.text()
                raise RuntimeError(f"Chat send failed: status {resp.status}: {detail}")

    async def _handle_notification(self, message: Dict[str, Any]) -> None:
        payload = message.get("payload", {})
        event_type = payload.get("subscription", {}).get("type", "unknown")
        await self.publisher.publish(
            event_type,
            {
                "event_type": event_type,
                "event_version": payload.get("subscription", {}).get("version"),
                "event": payload.get("event"),
                "metadata": payload.get("metadata"),
            },
        )

    async def _handle_session_welcome(self, message: Dict[str, Any]) -> None:
        session_id = message["payload"]["session"]["id"]
        LOGGER.info("WebSocket session established: %s", session_id)
        await self.subscribe_all(session_id)

    async def _handle_session_reconnect(self, message: Dict[str, Any]) -> str:
        reconnect_url = message["payload"]["session"]["reconnect_url"]
        LOGGER.warning("Twitch requested reconnect to %s", reconnect_url)
        return reconnect_url

    async def listen(self) -> None:
        backoff_gen = backoff.expo(base=2, factor=0.5, max_value=60)
        reconnect_url: str | None = None
        while True:
            try:
                session = await self._ensure_session()
                url = reconnect_url or TWITCH_EVENTSUB_WS
                async with session.ws_connect(url, heartbeat=20) as ws:
                    LOGGER.info("Connected to Twitch EventSub socket")
                    reconnect_url = None
                    # Connected — reset the backoff so the next failure starts small again
                    backoff_gen = backoff.expo(base=2, factor=0.5, max_value=60)
                    async for msg in ws:
                        if msg.type == aiohttp.WSMsgType.TEXT:
                            data = json.loads(msg.data)
                            message_type = data.get("metadata", {}).get("message_type")
                            if message_type == "session_welcome":
                                await self._handle_session_welcome(data)
                            elif message_type == "session_reconnect":
                                reconnect_url = await self._handle_session_reconnect(data)
                                break
                            elif message_type == "notification":
                                await self._handle_notification(data)
                            elif message_type == "revocation":
                                LOGGER.error("Subscription revoked: %s", data)
                        elif msg.type == aiohttp.WSMsgType.ERROR:
                            LOGGER.error("WebSocket error: %s", msg.data)
                            break
            except Exception as exc:
                delay = next(backoff_gen)
                LOGGER.error("WebSocket loop error: %s. Reconnecting in %.1fs", exc, delay)
                await asyncio.sleep(delay)


async def schedule_token_refresh(manager: TokenManager, session: aiohttp.ClientSession) -> None:
    retry_delay = 30
    while True:
        try:
            tokens = await manager.refresh(session)
        except Exception as exc:
            # A transient failure must not kill this task — tokens would
            # silently expire hours later. Retry with easing instead.
            LOGGER.error("Token refresh failed: %s. Retrying in %ds", exc, retry_delay)
            await asyncio.sleep(retry_delay)
            retry_delay = min(retry_delay * 2, 600)
            continue
        retry_delay = 30
        refresh_in = max(int(tokens.expires_in * 0.8), 300)
        await asyncio.sleep(refresh_in)


def required_env(name: str) -> str:
    value = os.getenv(name)
    if not value:
        raise RuntimeError(f"Missing required env var {name}")
    return value


def main() -> None:
    client_id = required_env("CLIENT_ID")
    client_secret = required_env("CLIENT_SECRET")
    refresh_token = required_env("REFRESH_TOKEN")
    channel_id = required_env("CHANNEL_ID")
    rabbitmq_url = os.getenv("RABBITMQ_URL", "amqp://guest:guest@twitch_broadcaster:5672/")
    exchange = os.getenv("RABBITMQ_EXCHANGE", "twitch_events")
    command_exchange = os.getenv("RABBITMQ_COMMAND_EXCHANGE", "twitch_commands")
    command_queue = os.getenv("RABBITMQ_COMMAND_QUEUE", "twitch_commands")
    token_cache_path = Path(os.getenv("TOKEN_CACHE_PATH", "/data/tokens.json"))

    manager = TokenManager(client_id, client_secret, refresh_token, token_cache_path)
    manager.load_cache()
    publisher = RabbitPublisher(rabbitmq_url, exchange)
    command_consumer = RabbitCommandConsumer(rabbitmq_url, command_exchange, command_queue)
    client = TwitchEventSubClient(manager, channel_id, publisher)

    async def runner() -> None:
        async with aiohttp.ClientSession() as session:
            await manager.refresh(session)
            asyncio.create_task(schedule_token_refresh(manager, session))
            await publisher.connect()
            await command_consumer.connect()

            async def handle_command_message(message: aio_pika.abc.AbstractIncomingMessage) -> None:
                async with message.process():
                    event_type = message.type or ""
                    try:
                        payload = json.loads(message.body) if message.body else {}
                    except json.JSONDecodeError as exc:
                        LOGGER.warning("Invalid command payload: %s", exc)
                        return

                    LOGGER.info("Command received: %s", event_type)

                    if event_type == "channel.command.send_chat":
                        channel = payload.get("channel_id")
                        chat_message = payload.get("message")
                        if not channel or not chat_message:
                            LOGGER.warning("send_chat missing channel_id or message")
                            return
                        try:
                            await client.send_chat(str(chat_message), str(channel))
                            LOGGER.info("Command completed: %s", event_type)
                        except Exception as exc:
                            LOGGER.error("Command failed: %s: %s", event_type, exc)
                    else:
                        LOGGER.warning("Unknown command type: %s", event_type)

            await command_consumer.consume(handle_command_message)
            await client.listen()

    asyncio.run(runner())


if __name__ == "__main__":
    main()
