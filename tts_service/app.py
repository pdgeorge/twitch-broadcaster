"""HTTP wrapper around tiktok_tts for the overlay pipeline (design doc §9).

POST /tts  {"text": "...", "voice": "en_us_002"}  ->  audio/mpeg bytes
GET  /health                                      ->  200 ok

overlay_controller calls this service, caches the mp3, and tells the browser
source to play it — audio never touches the streaming PC directly.
"""

import json
import os
import tempfile
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from tiktok_tts import TIKTOK_VOICES, tiktok_tts

SESSION_ID = os.environ.get("TIKTOK_SESSION_ID", "")
PORT = int(os.environ.get("TTS_PORT", "8081"))
DEFAULT_VOICE = "en_us_002"


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path == "/health":
            self.send_response(200)
            self.end_headers()
            self.wfile.write(b"ok")
        else:
            self.send_error(404)

    def do_POST(self):
        if self.path != "/tts":
            self.send_error(404)
            return
        if not SESSION_ID:
            self.send_error(503, "TIKTOK_SESSION_ID not configured")
            return
        try:
            length = int(self.headers.get("Content-Length", 0))
            payload = json.loads(self.rfile.read(length))
            text = str(payload["text"]).strip()
        except (ValueError, KeyError, json.JSONDecodeError):
            self.send_error(400, "expected a JSON body with a text field")
            return
        if not text:
            self.send_error(400, "empty text")
            return
        voice = payload.get("voice") or DEFAULT_VOICE
        if voice not in TIKTOK_VOICES:
            voice = DEFAULT_VOICE

        with tempfile.NamedTemporaryFile(suffix=".mp3", delete=False) as f:
            out_path = f.name
        try:
            result, duration = tiktok_tts(
                session_id=SESSION_ID,
                req_text=text,
                text_speaker=voice,
                filename=out_path,
            )
            if result is None:
                self.send_error(502, "tiktok tts failed (expired session id?)")
                return
            with open(out_path, "rb") as f:
                audio = f.read()
        finally:
            if os.path.exists(out_path):
                os.unlink(out_path)

        self.send_response(200)
        self.send_header("Content-Type", "audio/mpeg")
        self.send_header("Content-Length", str(len(audio)))
        self.send_header("X-Duration-Seconds", str(duration))
        self.end_headers()
        self.wfile.write(audio)

    def log_message(self, fmt, *args):
        print("[tts_service]", fmt % args)


if __name__ == "__main__":
    print(f"[tts_service] listening on :{PORT} ({len(TIKTOK_VOICES)} voices)")
    ThreadingHTTPServer(("", PORT), Handler).serve_forever()
