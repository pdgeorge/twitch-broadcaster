"""
chat_player.py
--------------
Extends AIPlayer for a Twitch-chat-driven D&D player.

Pressing the hotkey once:
  1. Sends a Twitch chat message announcing the listen window
  2. Takes a screenshot (if enabled)
  3. Starts mic recording AND opens RabbitMQ chat consumer simultaneously
  4. Both run for `listen_seconds`
  5. Mic stops → Whisper transcribes → logged as DM context
  6. Chat messages collected and printed as they arrive
  7. Sends screenshot + session log + transcription + chat to Claude
  8. TTS + OBS jiggle

Additional JSON fields (on top of AIPlayer fields):
{
    "listen_seconds": 30
}

Requires the same RabbitMQ setup as background.py.
"""

import json
import os
import threading
import time

import numpy as np
import pika
import sounddevice as sd
from dotenv import load_dotenv

from ai_player import AIPlayer, ScreenshotFlag, _whisper_model, SAMPLE_RATE, LOG_DIR

load_dotenv()

RABBITMQ_URL    = os.getenv("RABBITMQ_URL")
EXCHANGE        = os.getenv("RABBITMQ_EXCHANGE", "twitch_events")
COMMAND_EXCHANGE = os.getenv("RABBITMQ_COMMAND_EXCHANGE", "twitch_commands")
BROADCASTER_ID  = os.getenv("BROADCASTER_ID")


class ChatPlayer(AIPlayer):
    def __init__(self, config_path: str, session_log: list, screenshot_flag: ScreenshotFlag) -> None:
        super().__init__(config_path, session_log, screenshot_flag)

        with open(config_path, "r", encoding="utf-8") as f:
            cfg = json.load(f)

        self.listen_seconds: int = cfg.get("listen_seconds", 30)

        self._chat_busy = threading.Lock()
        print(f"[{self.name}] Chat mode — listen window: {self.listen_seconds}s")

    # ------------------------------------------------------------------
    # Twitch chat announcement
    # ------------------------------------------------------------------
    def _send_chat_message(self, message: str) -> None:
        """Publish a message to Twitch chat via the overlay_controller exchange."""
        try:
            print(f"in _send_chat_message: {message=}")
            params = pika.URLParameters(RABBITMQ_URL)
            conn = pika.BlockingConnection(params)
            ch = conn.channel()
            ch.basic_publish(
                exchange=COMMAND_EXCHANGE,
                routing_key="",
                body=json.dumps({
                    "channel_id": BROADCASTER_ID,
                    "message":    message,
                }),
                properties=pika.BasicProperties(type="channel.command.send_chat"),
            )
            conn.close()
            print(f"[{self.name}] 📣 Sent to Twitch chat: \"{message}\"")
        except Exception as e:
            print(f"[{self.name}] Failed to send Twitch chat message (non-fatal): {e}")

    # ------------------------------------------------------------------
    # Override hotkey — single press triggers mic + chat window together
    # ------------------------------------------------------------------
    def on_hotkey(self) -> None:
        if not self._chat_busy.acquire(blocking=False):
            print(f"[{self.name}] Already listening, ignoring trigger.")
            return
        threading.Thread(target=self._chat_pipeline, daemon=True).start()

    # ------------------------------------------------------------------
    # Mic recording for the listen window (timer-based, not toggle)
    # ------------------------------------------------------------------
    def _record_for_duration(self) -> str:
        """Record mic for listen_seconds, transcribe, return transcription string."""
        audio_frames = []

        def _callback(indata, frames, time_info, status):
            audio_frames.append(indata.copy())

        stream = sd.InputStream(
            samplerate=SAMPLE_RATE,
            channels=1,
            dtype="float32",
            callback=_callback,
        )
        stream.start()
        print(f"[{self.name}] 🎙  Mic recording for {self.listen_seconds}s...")
        for remaining in range(self.listen_seconds, 0, -1):
            time.sleep(1)
            if remaining % 10 == 0:
                print(f"[{self.name}] ⏱  {remaining}s remaining...")
        stream.stop()
        stream.close()
        print(f"[{self.name}] 🎙  Mic stopped. Transcribing...")

        if not audio_frames:
            print(f"[{self.name}] No audio captured.")
            return "(No DM audio captured)"

        audio_data = np.concatenate(audio_frames, axis=0).squeeze()
        result = _whisper_model.transcribe(audio_data, fp16=False)
        transcription = result["text"].strip()
        print(f"[{self.name}] DM Transcription: \"{transcription}\"")
        return transcription

    # ------------------------------------------------------------------
    # Chat collection via RabbitMQ
    # ------------------------------------------------------------------
    def _collect_chat(self, stop_event: threading.Event) -> list[dict]:
        """
        Open a temporary exclusive queue on the fanout exchange,
        collect chat messages until stop_event is set.
        Returns a live-filling list of {"user": ..., "text": ...} dicts.
        """
        collected = []

        def _consume():
            try:
                params = pika.URLParameters(RABBITMQ_URL)
                conn = pika.BlockingConnection(params)
                ch = conn.channel()
                ch.exchange_declare(exchange=EXCHANGE, exchange_type="fanout", durable=True)

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
                    try:
                        user = payload["event"]["chatter_user_name"]
                        text = payload["event"]["message"]["text"]
                    except (KeyError, TypeError):
                        user = "unknown"
                        text = str(payload)

                    entry = {"user": user, "text": text}
                    collected.append(entry)
                    print(f"[{self.name}] 💬 {user}: {text}")

                ch.basic_consume(queue=queue_name, on_message_callback=callback, auto_ack=True)

                while not stop_event.is_set():
                    conn.process_data_events(time_limit=0.5)

                conn.close()
            except Exception as e:
                print(f"[{self.name}] RabbitMQ error: {e}")

        consumer_thread = threading.Thread(target=_consume, daemon=True)
        consumer_thread.start()
        return collected

    # ------------------------------------------------------------------
    # Build the chat synthesis prompt
    # ------------------------------------------------------------------
    def _build_chat_prompt(self, transcription: str, chat_messages: list[dict]) -> str:
        log_text = self._format_log()
        chat_block = "\n".join(f"  {m['text']}" for m in chat_messages) or "  (no messages)"

        prompt = (
            f"=== SESSION LOG ===\n{log_text}\n\n"
            f"=== CURRENT SITUATION (DM) ===\n{transcription}\n\n"
            f"=== TWITCH CHAT WANTS ===\n{chat_block}\n\n"
            f"You are {self.name}. You are controlled by Twitch chat — their collective will "
            f"shapes your decisions, but you still have your own personality and voice. "
            f"Interpret what chat wants, weigh the options, and decide what to do. "
            f"Respond in character in 2 to 4 sentences spoken aloud at the table. "
            f"You may acknowledge conflicting chat opinions if it's funny or fitting. "
            f"CRITICAL: Do NOT use asterisks or action descriptions like *does something*. "
            f"Spoken words only. No stage directions, no emotes, no asterisks whatsoever."
        )
        return prompt

    # ------------------------------------------------------------------
    # Claude call (chat variant)
    # ------------------------------------------------------------------
    def _claude_call_chat(self, transcription: str, chat_messages: list[dict], img_b64) -> str | None:
        import anthropic as _anthropic
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
            client = _anthropic.Anthropic(api_key=os.getenv("ANTHROPIC_API_KEY"))
            response = client.messages.create(
                model="claude-sonnet-4-20250514",
                max_tokens=300,
                system=self.personality,
                messages=[{"role": "user", "content": content}],
            )
            text = response.content[0].text.strip()
            self._write_log_file(self.personality, prompt, text)
            return text
        except Exception as e:
            print(f"[{self.name}] Claude API error: {e}")
            return None

    # ------------------------------------------------------------------
    # Full chat pipeline
    # ------------------------------------------------------------------
    def _chat_pipeline(self) -> None:
        try:
            # 1. Announce to Twitch chat
            self._send_chat_message(
                f"{self.name} is now listening to you! "
                f"Anything you say for the next {self.listen_seconds} seconds "
                f"tells {self.name} what to do!"
            )
            time.sleep(1)

            # 2. Screenshot
            img_b64 = self._take_screenshot() if self.screenshot_flag.enabled else None
            if not self.screenshot_flag.enabled:
                print(f"[{self.name}] Screenshot: OFF (skipped)")

            # 3. Start chat collection (background thread, live-filling list)
            stop_event = threading.Event()
            print(f"[{self.name}] 🟢 Chat window OPEN — collecting for {self.listen_seconds}s...")
            chat_messages = self._collect_chat(stop_event)

            # 4. Record mic for the same duration (blocks for listen_seconds)
            transcription = self._record_for_duration()

            # 5. Stop chat consumer
            stop_event.set()
            print(f"[{self.name}] Chat window CLOSED. Collected {len(chat_messages)} message(s).")

            # 6. Log DM transcription
            self._append_log("DM", transcription)

            # 7. Claude call
            response = self._claude_call_chat(transcription, chat_messages, img_b64)
            if not response:
                return

            print(f"[{self.name}] Response: \"{response}\"")
            self._append_log(self.name, response)

            # 8. TTS + jiggle
            self._speak(response)

        finally:
            self._chat_busy.release()