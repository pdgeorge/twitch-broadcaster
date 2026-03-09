"""
chat_player.py
--------------
Extends AIPlayer for a Twitch-chat-driven D&D player.

Instead of mic recording, pressing the hotkey once:
  1. Takes a screenshot (if enabled)
  2. Opens a RabbitMQ consumer and buffers chat messages for `listen_seconds`
  3. Prints each message as it arrives
  4. Once time is up (or min_messages met + time elapsed), synthesises chat intent
  5. Sends screenshot + session log + chat summary to Claude as the character

Additional JSON fields (on top of AIPlayer fields):
{
    "listen_seconds": 30,
    "min_messages":   3
}

Requires the same RabbitMQ setup as background.py.
"""

import json
import os
import threading
import time

import pika
from dotenv import load_dotenv

from ai_player import AIPlayer, ScreenshotFlag

load_dotenv()

RABBITMQ_URL = os.getenv("RABBITMQ_URL")
EXCHANGE     = os.getenv("RABBITMQ_EXCHANGE", "twitch_events")


class ChatPlayer(AIPlayer):
    def __init__(self, config_path: str, session_log: list, screenshot_flag: ScreenshotFlag) -> None:
        super().__init__(config_path, session_log, screenshot_flag)

        with open(config_path, "r", encoding="utf-8") as f:
            cfg = json.load(f)

        self.listen_seconds: int = cfg.get("listen_seconds", 30)
        self.min_messages:   int = cfg.get("min_messages", 3)

        self._chat_busy = threading.Lock()
        print(f"[{self.name}] Chat mode — listen window: {self.listen_seconds}s, min messages: {self.min_messages}")

    # ------------------------------------------------------------------
    # Override hotkey — single press triggers the chat window
    # ------------------------------------------------------------------
    def on_hotkey(self) -> None:
        if not self._chat_busy.acquire(blocking=False):
            print(f"[{self.name}] Already listening to chat, ignoring trigger.")
            return
        threading.Thread(target=self._chat_pipeline, daemon=True).start()

    # ------------------------------------------------------------------
    # Chat collection via RabbitMQ
    # ------------------------------------------------------------------
    def _collect_chat(self) -> list[dict]:
        """
        Open a temporary exclusive queue on the fanout exchange,
        collect chat messages for listen_seconds, return list of
        {"user": ..., "text": ...} dicts.
        """
        collected = []
        stop_event = threading.Event()

        def _consume():
            try:
                params = pika.URLParameters(RABBITMQ_URL)
                conn = pika.BlockingConnection(params)
                ch = conn.channel()
                ch.exchange_declare(exchange=EXCHANGE, exchange_type="fanout", durable=True)

                # Exclusive auto-delete queue — gone when we disconnect
                result = ch.queue_declare(queue="", exclusive=True, auto_delete=True)
                queue_name = result.method.queue
                ch.queue_bind(exchange=EXCHANGE, queue=queue_name)

                def callback(ch_inner, method, properties, body):
                    if stop_event.is_set():
                        ch_inner.stop_consuming()
                        return
                    try:
                        payload = json.loads(body.decode())
                    except json.JSONDecodeError:
                        return
                    event_type = properties.type or "unknown"
                    if event_type != "channel.chat.message":
                        return

                    # Extract chatter name + message text from Twitch EventSub payload
                    try:
                        user = payload["event"]["chatter_user_name"]
                        text = payload["event"]["message"]["text"]
                    except (KeyError, TypeError):
                        # Fallback: just dump raw payload keys
                        user = "unknown"
                        text = str(payload)

                    entry = {"user": user, "text": text}
                    collected.append(entry)
                    print(f"[{self.name}] 💬 {user}: {text}")

                ch.basic_consume(queue=queue_name, on_message_callback=callback, auto_ack=True)

                # Poll so we can honour stop_event without blocking forever
                while not stop_event.is_set():
                    conn.process_data_events(time_limit=0.5)

                conn.close()
            except Exception as e:
                print(f"[{self.name}] RabbitMQ error: {e}")

        consumer_thread = threading.Thread(target=_consume, daemon=True)
        consumer_thread.start()

        # Countdown
        deadline = time.time() + self.listen_seconds
        while time.time() < deadline:
            remaining = int(deadline - time.time())
            print(f"[{self.name}] Chat window open — {remaining}s remaining, {len(collected)} messages so far...", end="\r")
            time.sleep(1)

        print()  # newline after the \r countdown
        stop_event.set()
        consumer_thread.join(timeout=3)

        return collected

    # ------------------------------------------------------------------
    # Build the chat synthesis prompt
    # ------------------------------------------------------------------
    def _build_chat_prompt(self, transcription: str, chat_messages: list[dict]) -> str:
        log_text = self._format_log()

        chat_block = "\n".join(f"  {m['user']}: {m['text']}" for m in chat_messages) or "  (no messages)"

        prompt = (
            f"=== SESSION LOG ===\n{log_text}\n\n"
            f"=== CURRENT SITUATION (DM) ===\n{transcription}\n\n"
            f"=== TWITCH CHAT WANTS ===\n{chat_block}\n\n"
            f"You are {self.name}. You are controlled by Twitch chat — their collective will "
            f"shapes your decisions, but you still have your own personality and voice. "
            f"Interpret what chat wants, weigh the options, and decide what to do. "
            f"Respond in character in 2 to 4 sentences spoken aloud at the table. "
            f"You may acknowledge conflicting chat opinions if it's funny or fitting."
        )
        return prompt

    # ------------------------------------------------------------------
    # Override Claude call to use chat prompt
    # ------------------------------------------------------------------
    def _claude_call_chat(self, transcription: str, chat_messages: list[dict], img_b64) -> str | None:
        prompt = self._build_chat_prompt(transcription, chat_messages)

        print(f"\n[{self.name}] ── SENDING TO CLAUDE ──────────────────────────")
        print(f"  System: {self.personality[:120]}{'...' if len(self.personality) > 120 else ''}")
        print(f"  Session log entries: {len(self.session_log)}")
        print(f"  Screenshot attached: {'YES' if img_b64 else 'NO'}")
        print(f"  Chat messages collected: {len(chat_messages)}")
        print(f"  Prompt:\n{prompt}")
        print(f"[{self.name}] ───────────────────────────────────────────────\n")

        content = []
        if img_b64:
            content.append({
                "type": "image",
                "source": {"type": "base64", "media_type": "image/jpeg", "data": img_b64},
            })
        content.append({"type": "text", "text": prompt})

        try:
            import anthropic as _anthropic
            client = _anthropic.Anthropic(api_key=os.getenv("ANTHROPIC_API_KEY"))
            response = client.messages.create(
                model="claude-sonnet-4-20250514",
                max_tokens=300,
                system=self.personality,
                messages=[{"role": "user", "content": content}],
            )
            return response.content[0].text.strip()
        except Exception as e:
            print(f"[{self.name}] Claude API error: {e}")
            return None

    # ------------------------------------------------------------------
    # Full chat pipeline
    # ------------------------------------------------------------------
    def _chat_pipeline(self) -> None:
        try:
            # 1. Screenshot
            img_b64 = self._take_screenshot() if self.screenshot_flag.enabled else None
            if self.screenshot_flag.enabled:
                print(f"[{self.name}] Screenshot captured.")
            else:
                print(f"[{self.name}] Screenshot: OFF (skipped)")

            # 2. Read last DM entry from session log as "current situation"
            dm_entries = [e for e in self.session_log if e["speaker"] == "DM"]
            transcription = dm_entries[-1]["text"] if dm_entries else "(No DM context yet)"
            print(f"[{self.name}] Using last DM entry as context: \"{transcription}\"")

            # 3. Collect chat
            print(f"[{self.name}] 🟢 Chat window OPEN — collecting for {self.listen_seconds}s...")
            chat_messages = self._collect_chat()
            print(f"[{self.name}] Chat window CLOSED. Collected {len(chat_messages)} message(s).")

            if len(chat_messages) < self.min_messages:
                print(f"[{self.name}] Only {len(chat_messages)} message(s) — below min_messages ({self.min_messages}). "
                      f"Proceeding anyway with what we have.")

            # 4. Claude call
            response = self._claude_call_chat(transcription, chat_messages, img_b64)
            if not response:
                return

            print(f"[{self.name}] Response: \"{response}\"")
            self._append_log(self.name, response)

            # 5. TTS + jiggle
            self._speak(response)

        finally:
            self._chat_busy.release()