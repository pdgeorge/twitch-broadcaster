import json
import os
import pika

RABBITMQ_URL = os.getenv("RABBITMQ_URL", "amqp://guest:guest@localhost:5672/")
EXCHANGE = os.getenv("RABBITMQ_EXCHANGE", "twitch_events")
QUEUE_NAME = os.getenv("QUEUE_NAME", "twitch_event_printer")


def _print_event(event_type: str, payload: dict) -> None:
    print(f"\nEvent type: {event_type}\nPayload: {json.dumps(payload, indent=2)}\n")


def handle_all_events(event_type: str, payload: dict) -> None:
    """Example handler: print every event Twitch emits."""
    _print_event(event_type, payload)


def handle_channel_points_only(event_type: str, payload: dict) -> None:
    """Print only channel point redemptions."""
    if event_type == "channel.channel_points_custom_reward_redemption.add":
        _print_event(event_type, payload)


def handle_chat_only(event_type: str, payload: dict) -> None:
    """Print only chat messages."""
    if event_type == "channel.chat.message":
        _print_event(event_type, payload)


def handle_chat_and_rewards(event_type: str, payload: dict) -> None:
    """Print chat messages and channel point redemptions."""
    allowed = {
        "channel.chat.message",
        "channel.channel_points_custom_reward_redemption.add",
    }
    if event_type in allowed:
        _print_event(event_type, payload)


def main() -> None:
    params = pika.URLParameters(RABBITMQ_URL)
    connection = pika.BlockingConnection(params)
    channel = connection.channel()
    channel.exchange_declare(exchange=EXCHANGE, exchange_type="fanout", durable=True)
    channel.queue_declare(queue=QUEUE_NAME, durable=True)
    channel.queue_bind(exchange=EXCHANGE, queue=QUEUE_NAME)

    print(f" [*] Waiting for events on exchange '{EXCHANGE}'. Press CTRL+C to exit.")

    # Uncomment to receive every Twitch event on the exchange.
    # event_handler = handle_all_events
    event_handler = handle_chat_and_rewards
    # event_handler = handle_channel_points_only
    # event_handler = handle_chat_only

    def callback(ch, method, properties, body):
        try:
            payload = json.loads(body.decode())
        except json.JSONDecodeError:
            payload = {"raw": body.decode()}
        event_type = properties.type or "unknown"
        event_handler(event_type, payload)

    channel.basic_consume(queue=QUEUE_NAME, on_message_callback=callback, auto_ack=True)
    channel.start_consuming()


if __name__ == "__main__":
    main()
