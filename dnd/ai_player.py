"""
ai_player.py
------------
Base class for an AI D&D player.
Loads character config from a JSON file, handles:
  - Mic recording (toggle on/off)
  - Optional screenshot capture
  - Whisper transcription
  - Claude vision/text call
  - TikTok TTS playback
  - OBS jiggle sync

JSON schema (e.g. dabbert.json):
{
    "name":          "Dabbert",
    "personality":   "You are Dabbert, a chaotic neutral gnome rogue...",
    "tts_voice":     "en_us_ghostface",
    "context_limit": 100,
    "pop_count":     10
}
"""

import base64
import io
import json
import math
import sys
import os
import tempfile
import threading
import time

import anthropic
import numpy as np
import sounddevice as sd
import soundfile as sf
import vlc
import whisper
from dotenv import load_dotenv
from obswebsocket import requests as obs_requests
from PIL import ImageGrab
import subprocess

sys.path.insert(0, os.path.join(os.path.dirname(__file__), '..'))
from OBS_Websocket import OBSWebsocketsManager
from tiktok_tts import tiktok_tts

load_dotenv()

ANTHROPIC_API_KEY  = os.getenv("ANTHROPIC_API_KEY")
OBS_JIGGLE_SOURCE  = os.getenv("OBS_JIGGLE_SOURCE", "HorseIcon")
SCREENSHOT_REGION  = (0, 0, 1920, 1080)
SCREENSHOT_DIR     = "./temp/dnd_screenshots"
SAMPLE_RATE        = 16000   # Whisper expects 16kHz
ENVELOPE_CHUNK_MS  = 50
MAX_ROTATION       = 45.0

anthropic_client = anthropic.Anthropic(api_key=ANTHROPIC_API_KEY)

# Load Whisper once at import time — shared across all players
print("[Whisper] Loading model...")
_whisper_model = whisper.load_model("base")
print("[Whisper] Model ready.")


# ---------------------------------------------------------------------------
# Shared OBS jiggle (same pattern as horsey_describer.py)
# ---------------------------------------------------------------------------
def _extract_envelope(audio_path: str) -> list[tuple[float, float]]:
    wav_path = audio_path.replace(".mp3", "_env.wav")
    try:
        subprocess.run(
            ["ffmpeg", "-y", "-i", audio_path, wav_path],
            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=True,
        )
        data, samplerate = sf.read(wav_path, dtype="float32")
        if data.ndim > 1:
            data = data.mean(axis=1)
        spc = int(samplerate * ENVELOPE_CHUNK_MS / 1000)
        chunks = [data[i:i+spc] for i in range(0, len(data), spc) if len(data[i:i+spc]) > 0]
        rms = [float(np.sqrt(np.mean(c**2))) for c in chunks]
        mx = max(rms) if rms else 1.0
        return [(i * ENVELOPE_CHUNK_MS, r / mx) for i, r in enumerate(rms)]
    finally:
        if os.path.exists(wav_path):
            os.unlink(wav_path)


def _obs_jiggle(envelope: list[tuple[float, float]], player: vlc.MediaPlayer, source_name: str) -> None:
    try:
        obs_mgr = OBSWebsocketsManager()
        response = obs_mgr.ws.call(obs_requests.GetCurrentProgramScene())
        scene_name = response.getSceneName()
        env_map = dict(envelope)
        print(f"[OBS] Jiggle starting for source '{source_name}'")
        while player.get_state() not in (vlc.State.Ended, vlc.State.Error, vlc.State.Stopped):
            pos_ms = player.get_time()
            if pos_ms < 0:
                time.sleep(0.02)
                continue
            nearest_ms = round(pos_ms / ENVELOPE_CHUNK_MS) * ENVELOPE_CHUNK_MS
            amplitude = env_map.get(nearest_ms, 0.0)
            rot = amplitude * MAX_ROTATION * math.sin(12 * time.time())
            obs_mgr.shake(scene_name, source_name, rot)
            time.sleep(0.02)
        obs_mgr.shake(scene_name, source_name, 0)
        print(f"[OBS] Jiggle complete for '{source_name}'")
    except Exception as e:
        print(f"[OBS] Jiggle failed (non-fatal): {e}")


# ---------------------------------------------------------------------------
# AIPlayer base class
# ---------------------------------------------------------------------------
class AIPlayer:
    def __init__(self, config_path: str, session_log: list, screenshot_flag: "ScreenshotFlag") -> None:
        with open(config_path, "r", encoding="utf-8") as f:
            cfg = json.load(f)

        self.name:          str  = cfg["name"]
        self.personality:   str  = cfg["personality"]
        self.tts_voice:     str  = cfg["tts_voice"]
        self.context_limit: int  = cfg.get("context_limit", 100)
        self.pop_count:     int  = cfg.get("pop_count", 10)
        self.obs_source:    str  = cfg.get("obs_source", OBS_JIGGLE_SOURCE)

        self.session_log    = session_log       # shared reference
        self.screenshot_flag = screenshot_flag  # shared ScreenshotFlag object

        self._recording     = False
        self._audio_frames  = []
        self._record_lock   = threading.Lock()
        self._busy          = threading.Lock()

        print(f"[{self.name}] Loaded from {config_path}")

    # ------------------------------------------------------------------
    # Session log helpers
    # ------------------------------------------------------------------
    def _append_log(self, speaker: str, text: str) -> None:
        self.session_log.append({"speaker": speaker, "text": text})
        if len(self.session_log) > self.context_limit:
            removed = self.session_log[:self.pop_count]
            del self.session_log[:self.pop_count]
            print(f"[Session] Trimmed {self.pop_count} oldest entries from log.")
            for r in removed:
                print(f"  popped → [{r['speaker']}]: {r['text'][:60]}...")

    def _format_log(self) -> str:
        return "\n".join(f"[{e['speaker']}]: {e['text']}" for e in self.session_log)

    # ------------------------------------------------------------------
    # Screenshot
    # ------------------------------------------------------------------
    def _take_screenshot(self) -> str | None:
        """Capture screen, return base64 JPEG string or None."""
        os.makedirs(SCREENSHOT_DIR, exist_ok=True)
        img = ImageGrab.grab(bbox=SCREENSHOT_REGION)
        timestamp = time.strftime("%Y%m%d_%H%M%S")
        save_path = os.path.join(SCREENSHOT_DIR, f"dnd_{self.name}_{timestamp}.jpg")
        img.save(save_path, format="JPEG", quality=85)
        print(f"[{self.name}] Screenshot saved → {save_path}")
        buf = io.BytesIO()
        img.save(buf, format="JPEG", quality=85)
        return base64.standard_b64encode(buf.getvalue()).decode("utf-8")

    # ------------------------------------------------------------------
    # Mic recording
    # ------------------------------------------------------------------
    def on_hotkey(self) -> None:
        """Called each time the player's hotkey is pressed."""
        with self._record_lock:
            if not self._recording:
                self._start_recording()
            else:
                threading.Thread(target=self._stop_and_process, daemon=True).start()

    def _start_recording(self) -> None:
        self._audio_frames = []
        self._recording = True

        # Take screenshot on first press if flag is on
        self._pending_screenshot = self._take_screenshot() if self.screenshot_flag.enabled else None
        if self.screenshot_flag.enabled:
            print(f"[{self.name}] Screenshot captured.")
        else:
            print(f"[{self.name}] Screenshot: OFF (skipped)")

        self._stream = sd.InputStream(
            samplerate=SAMPLE_RATE,
            channels=1,
            dtype="float32",
            callback=self._audio_callback,
        )
        self._stream.start()
        print(f"[{self.name}] 🎙  Listening... (press hotkey again to stop)")

    def _audio_callback(self, indata, frames, time_info, status):
        if self._recording:
            self._audio_frames.append(indata.copy())

    def _stop_and_process(self) -> None:
        with self._record_lock:
            self._recording = False
            self._stream.stop()
            self._stream.close()

        print(f"[{self.name}] Recording stopped. Transcribing...")

        if not self._audio_frames:
            print(f"[{self.name}] No audio captured.")
            return

        audio_data = np.concatenate(self._audio_frames, axis=0).squeeze()
        result = _whisper_model.transcribe(audio_data, fp16=False)
        transcription = result["text"].strip()
        print(f"[{self.name}] Transcription: \"{transcription}\"")

        self._append_log("DM", transcription)
        self._run_pipeline(transcription, self._pending_screenshot)

    # ------------------------------------------------------------------
    # Claude call
    # ------------------------------------------------------------------
    def _build_prompt(self, transcription: str) -> str:
        log_text = self._format_log()
        prompt = (
            f"=== SESSION LOG ===\n{log_text}\n\n"
            f"=== CURRENT SITUATION (DM) ===\n{transcription}\n\n"
            f"What do you do? Respond in character as {self.name}. "
            f"Be concise — 2 to 4 sentences spoken aloud at the table."
        )
        return prompt

    def _claude_call(self, transcription: str, img_b64: str | None) -> str | None:
        prompt = self._build_prompt(transcription)

        print(f"\n[{self.name}] ── SENDING TO CLAUDE ──────────────────────────")
        print(f"  System: {self.personality[:120]}{'...' if len(self.personality) > 120 else ''}")
        print(f"  Session log entries: {len(self.session_log)}")
        print(f"  Screenshot attached: {'YES' if img_b64 else 'NO'}")
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
            response = anthropic_client.messages.create(
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
    # TTS + jiggle
    # ------------------------------------------------------------------
    def _speak(self, text: str) -> None:
        print(f"[{self.name}] Speaking: \"{text}\"")
        with tempfile.NamedTemporaryFile(suffix=".mp3", delete=False) as f:
            tmp_path = f.name

        result_path, _ = tiktok_tts(
            session_id=os.getenv("TIKTOK_TOKEN"),
            req_text=text,
            text_speaker=self.tts_voice,
            filename=tmp_path,
        )
        if result_path is None:
            print(f"[{self.name}] TTS failed.")
            if os.path.exists(tmp_path):
                os.unlink(tmp_path)
            return

        envelope = _extract_envelope(tmp_path)
        player = vlc.MediaPlayer(tmp_path)
        player.play()
        time.sleep(0.15)

        jiggle_thread = threading.Thread(
            target=_obs_jiggle, args=(envelope, player, self.obs_source), daemon=True
        )
        jiggle_thread.start()

        while player.get_state() not in (vlc.State.Ended, vlc.State.Error, vlc.State.Stopped):
            time.sleep(0.1)
        player.release()
        if os.path.exists(tmp_path):
            os.unlink(tmp_path)

    # ------------------------------------------------------------------
    # Full pipeline
    # ------------------------------------------------------------------
    def _run_pipeline(self, transcription: str, img_b64: str | None) -> None:
        if not self._busy.acquire(blocking=False):
            print(f"[{self.name}] Already processing, ignoring trigger.")
            return
        try:
            response = self._claude_call(transcription, img_b64)
            if not response:
                return
            print(f"[{self.name}] Response: \"{response}\"")
            self._append_log(self.name, response)
            self._speak(response)
        finally:
            self._busy.release()


# ---------------------------------------------------------------------------
# Simple flag object passed by reference to all players
# ---------------------------------------------------------------------------
class ScreenshotFlag:
    def __init__(self, enabled: bool = True) -> None:
        self.enabled = enabled

    def toggle(self) -> None:
        self.enabled = not self.enabled
        print(f"[Session] Screenshot: {'ON ✓' if self.enabled else 'OFF ✗'}")