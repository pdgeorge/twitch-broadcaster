import json
import os
import pika

RABBITMQ_URL = os.getenv("RABBITMQ_URL", "amqp://guest:guest@localhost:5672/")
EXCHANGE = os.getenv("RABBITMQ_EXCHANGE", "twitch_events")
QUEUE_NAME = os.getenv("QUEUE_NAME", "twitch_event_printer")


def main() -> None:
    params = pika.URLParameters(RABBITMQ_URL)
    connection = pika.BlockingConnection(params)
    channel = connection.channel()
    channel.exchange_declare(exchange=EXCHANGE, exchange_type="fanout", durable=True)
    channel.queue_declare(queue=QUEUE_NAME, durable=True)
    channel.queue_bind(exchange=EXCHANGE, queue=QUEUE_NAME)

    print(f" [*] Waiting for events on exchange '{EXCHANGE}'. Press CTRL+C to exit.")

    def callback(ch, method, properties, body):
        try:
            payload = json.loads(body.decode())
        except json.JSONDecodeError:
            payload = {"raw": body.decode()}
        print(f"\nEvent type: {properties.type}\nPayload: {json.dumps(payload, indent=2)}\n")

    channel.basic_consume(queue=QUEUE_NAME, on_message_callback=callback, auto_ack=True)
    channel.start_consuming()


if __name__ == "__main__":
    main()
