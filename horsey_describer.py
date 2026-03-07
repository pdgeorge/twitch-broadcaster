"""
horsey_describer.py
-------------------
Hotkey-triggered AI describer for Horsey Game stream abominations.

Hotkeys (numpad):
  NUMPAD_1  →  Short & punchy   (1-2 sentences)
  NUMPAD_2  →  Medium rant      (4-6 sentences)
  NUMPAD_4  →  Full monologue   (go completely unhinged)

Pipeline:
  Hotkey → fullscreen screenshot → Anthropic vision API → speak_tts() → OBS jiggle

Requirements:
  pip install anthropic pillow pynput python-dotenv obs-websocket-py gTTS python-vlc numpy soundfile

.env keys needed:
  ANTHROPIC_API_KEY
  OBS_HOST            (default: localhost)
  OBS_PORT            (default: 4455)
  OBS_PASSWORD
  OBS_JIGGLE_SOURCE   (name of the OBS source to jiggle, e.g. "HorseIcon")

Depends on: OBS_Websocket.py (OBSWebsocketsManager) in the same directory.
"""

import base64
import io
import math
import os
import tempfile
import threading
import time

import anthropic
import vlc
import numpy as np
import soundfile as sf
import subprocess
from dotenv import load_dotenv
from gtts import gTTS
from PIL import ImageGrab
from pynput import keyboard
from tiktok_tts import tiktok_tts

from obswebsocket import requests as obs_requests
from OBS_Websocket import OBSWebsocketsManager

load_dotenv()

# ---------------------------------------------------------------------------
# Config
# ---------------------------------------------------------------------------
TIKTOK_TOKEN = os.getenv("TIKTOK_TOKEN")
ANTHROPIC_API_KEY = os.getenv("ANTHROPIC_API_KEY")
OBS_JIGGLE_SOURCE = os.getenv("OBS_JIGGLE_SOURCE", "HorseIcon")

# Primary monitor capture region — adjust these to taste
# Format: (left, top, right, bottom) in pixels
# Default: full 1920x1080 primary monitor
# Full Screen left screen example
# SCREENSHOT_REGION = (0, 0, 1920, 1080)
SCREENSHOT_REGION = (462, 305, 1533, 771)

# Screenshot save directory — saved before sending to Claude so you can
# quickly verify/adjust the region without waiting for API response
SCREENSHOT_DIR = "./temp/screenshots"

# ---------------------------------------------------------------------------
# Anthropic client
# ---------------------------------------------------------------------------
anthropic_client = anthropic.Anthropic(api_key=ANTHROPIC_API_KEY)

# ---------------------------------------------------------------------------
# Mode definitions  (soft-limit lives inside the prompt, not max_tokens)
# ---------------------------------------------------------------------------
MODES = {
    "short": {
        "label": "Short & Punchy",
        "prompt": (
            "You are an unhinged cryptid researcher who has just witnessed something "
            "that has shaken your entire understanding of biology. "
            "Describe what you see in the screenshot in ONE or TWO sentences MAX. "
            "Be brief, be alarmed, be weird. No more than two sentences."
        ),
    },
    "medium": {
        "label": "Medium Rant",
        "prompt": (
            "You are an unhinged cryptid researcher mid-breakdown at a conference. "
            "Describe the abomination in the screenshot in about four to six sentences. "
            "Reference its anatomy, speculate wildly about its origin, and end on a "
            "deeply unsettling observation. Keep it to roughly four to six sentences."
        ),
    },
    "monologue": {
        "label": "Full Unhinged Monologue",
        "prompt": (
            "You are an unhinged cryptid researcher who has completely lost it. "
            "This is your magnum opus. Describe the creature or abomination in the "
            "screenshot as if writing a field journal entry that will be found after "
            "you disappear. Cover its appearance, probable diet, spiritual implications, "
            "what government agency is covering it up, and why horses were a mistake. "
            "Go completely off the rails. No length limit — let it flow."
        ),
    },
}

# Numpad keys with numlock ON report as plain chars — map char to mode
NUMPAD_CHAR_MAP = {
    '0': "screenshot_only",
    '1': "short",
    '2': "medium",
    '3': "monologue",
}

# Guard against overlapping triggers
_busy = threading.Lock()

# ---------------------------------------------------------------------------
# Amplitude envelope extraction  —  ffmpeg → wav → numpy RMS
# ---------------------------------------------------------------------------
ENVELOPE_CHUNK_MS = 50  # analyse audio in 50 ms windows

def _extract_envelope(audio_path: str) -> list[tuple[float, float]]:
    """
    Convert mp3 to wav via ffmpeg, load with soundfile, compute RMS per chunk.
    Returns a list of (timestamp_ms, normalised_amplitude 0.0-1.0) tuples.
    """
    wav_path = audio_path.replace(".mp3", ".wav")
    try:
        subprocess.run(
            ["ffmpeg", "-y", "-i", audio_path, wav_path],
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
            check=True,
        )

        data, samplerate = sf.read(wav_path, dtype="float32")

        # Flatten to mono if stereo
        if data.ndim > 1:
            data = data.mean(axis=1)

        samples_per_chunk = int(samplerate * ENVELOPE_CHUNK_MS / 1000)
        chunks = [data[i:i + samples_per_chunk]
                  for i in range(0, len(data), samples_per_chunk)
                  if len(data[i:i + samples_per_chunk]) > 0]

        rms_values = [float(np.sqrt(np.mean(chunk ** 2))) for chunk in chunks]
        max_rms = max(rms_values) if rms_values else 1.0

        envelope = [
            (i * ENVELOPE_CHUNK_MS, rms / max_rms)
            for i, rms in enumerate(rms_values)
        ]
        return envelope

    finally:
        if os.path.exists(wav_path):
            os.unlink(wav_path)


# ---------------------------------------------------------------------------
# OBS jiggle  —  driven by live amplitude envelope + VLC playback clock
# ---------------------------------------------------------------------------
MAX_ROTATION = 15.0  # degrees at peak amplitude

def _obs_jiggle(envelope: list[tuple[float, float]], player: vlc.MediaPlayer) -> None:
    """
    Walk the amplitude envelope in sync with VLC's playback clock.
    Rotation angle = amplitude * MAX_ROTATION, direction alternates via sine.
    Runs until VLC reports playback has ended.
    """
    try:
        obs_mgr = OBSWebsocketsManager()
        response = obs_mgr.ws.call(obs_requests.GetCurrentProgramScene())
        scene_name = response.getSceneName()

        # Build a fast lookup: timestamp_ms → amplitude
        env_map = dict(envelope)
        total_duration_ms = envelope[-1][0] if envelope else 0

        print(f"[OBS] Jiggle starting — {len(envelope)} envelope points over {total_duration_ms/1000:.1f}s")

        while player.get_state() not in (vlc.State.Ended, vlc.State.Error, vlc.State.Stopped):
            pos_ms = player.get_time()  # current playback position in ms
            if pos_ms < 0:
                time.sleep(0.02)
                continue

            # Find the nearest envelope chunk
            nearest_ms = round(pos_ms / ENVELOPE_CHUNK_MS) * ENVELOPE_CHUNK_MS
            amplitude = env_map.get(nearest_ms, 0.0)

            # Sine wave gives smooth back-and-forth, amplitude scales the swing
            rot = amplitude * MAX_ROTATION * math.sin(12 * time.time())
            obs_mgr.shake(scene_name, OBS_JIGGLE_SOURCE, rot)
            time.sleep(0.02)

        # Reset to neutral
        obs_mgr.shake(scene_name, OBS_JIGGLE_SOURCE, 0)
        print("[OBS] Jiggle complete.")
    except Exception as e:
        print(f"[OBS] Jiggle failed (non-fatal): {e}")


# ---------------------------------------------------------------------------
# TTS wrapper  —  swap gTTS for anything else here later
# Returns amplitude envelope for OBS jiggle sync
# ---------------------------------------------------------------------------
def speak_tts(text: str) -> list[tuple[float, float]]:
    """
    Generate TTS audio, extract amplitude envelope, play audio, return envelope.
    Replace the internals to swap TTS engines (ElevenLabs, TikTok, etc.)
    Contract: always returns a list of (timestamp_ms, amplitude 0.0-1.0) tuples.
    """
    try:
        # gTTS implementation
        # tts = gTTS(text=text, lang="en", slow=False)
        # with tempfile.NamedTemporaryFile(suffix=".mp3", delete=False) as f:
        #     tmp_path = f.name
        # tts.save(tmp_path)

        # TikTokTextToSpeech Imeplementation
        with tempfile.NamedTemporaryFile(suffix=".mp3", delete=False) as f:
            tmp_path = f.name

        result_path, _ = tiktok_tts(
            session_id=TIKTOK_TOKEN,
            req_text=text,
            text_speaker="en_us_ghostface",
            filename=tmp_path,
        )

        if result_path is None:
            print("[TTS] TikTok TTS failed.")
            return [], None, None

        # Other implementation
        # Note: Save file to tmp_path with suffix as .mp3

        # Extract envelope before playback
        envelope = _extract_envelope(tmp_path)

        player = vlc.MediaPlayer(tmp_path)
        player.play()

        # Small buffer to let VLC actually start before jiggle thread reads get_time()
        time.sleep(0.15)

        return envelope, player, tmp_path

    except Exception as e:
        print(f"[TTS] Error generating audio: {e}")
        return [], None, None


# ---------------------------------------------------------------------------
# Core pipeline
# ---------------------------------------------------------------------------
def _screenshot_to_b64() -> str:
    """Capture primary monitor region, save to disk for easy region tuning, return as base64 JPEG."""
    os.makedirs(SCREENSHOT_DIR, exist_ok=True)

    img = ImageGrab.grab(bbox=SCREENSHOT_REGION)

    # Save a copy so you can quickly check/adjust the region
    timestamp = time.strftime("%Y%m%d_%H%M%S")
    save_path = os.path.join(SCREENSHOT_DIR, f"horsey_{timestamp}.jpg")
    img.save(save_path, format="JPEG", quality=85)
    print(f"[Horsey] Screenshot saved → {save_path}")

    buf = io.BytesIO()
    img.save(buf, format="JPEG", quality=85)
    return base64.standard_b64encode(buf.getvalue()).decode("utf-8")


def describe_abomination(mode_key: str) -> None:
    """Full pipeline: screenshot → Claude vision → TTS + OBS jiggle."""
    mode = MODES[mode_key]
    text_prompt = "Describe the abomination you see. It is riding on a tractor bed and it may be sleeping (denoted by having 'ZZZ' over its head)"
    print(f"\n[Horsey] Triggered: {mode['label']}")

    # 1. Screenshot
    print("[Horsey] Capturing screen...")
    img_b64 = _screenshot_to_b64()

    # 2. Claude vision
    print("[Horsey] Sending to Claude...")
    try:
        response = anthropic_client.messages.create(
            model="claude-sonnet-4-20250514",
            max_tokens=1000,
            system=mode["prompt"],
            messages=[
                {
                    "role": "user",
                    "content": [
                        {
                            "type": "image",
                            "source": {
                                "type": "base64",
                                "media_type": "image/jpeg",
                                "data": img_b64,
                            },
                        },
                        {
                            "type": "text",
                            "text": f"{text_prompt}",
                        },
                    ],
                }
            ],
        )
        description = response.content[0].text
    except Exception as e:
        print(f"[Horsey] Claude API error: {e}")
        return

    print(f"\n[Horsey] Description:\n{description}\n")

    # 3. Generate TTS audio + extract envelope (envelope ready before playback starts)
    envelope, player, tmp_path = speak_tts(description)

    if player is None:
        print("[Horsey] TTS failed, skipping jiggle.")
        return

    # 4. Jiggle in sync with live playback (non-blocking, shares player reference)
    jiggle_thread = threading.Thread(
        target=_obs_jiggle, args=(envelope, player), daemon=True
    )
    jiggle_thread.start()

    # 5. Wait for VLC to finish, then clean up
    while player.get_state() not in (vlc.State.Ended, vlc.State.Error, vlc.State.Stopped):
        time.sleep(0.1)
    player.release()
    if tmp_path:
        os.unlink(tmp_path)


def _trigger(mode_key: str) -> None:
    """Thread-safe trigger wrapper."""
    if mode_key == "screenshot_only":
        _screenshot_to_b64()  # saves to disk, no API call
        return
    if _busy.acquire(blocking=False):
        try:
            describe_abomination(mode_key)
        finally:
            _busy.release()
    else:
        print("[Horsey] Already running, ignoring trigger.")


# ---------------------------------------------------------------------------
# Hotkey listener
# ---------------------------------------------------------------------------
def _resolve_mode(key) -> str | None:
    char = getattr(key, 'char', None)
    if char:
        return NUMPAD_CHAR_MAP.get(char)
    return None


def on_press(key):
    mode_key = _resolve_mode(key)
    if mode_key:
        threading.Thread(target=_trigger, args=(mode_key,), daemon=True).start()


def main() -> None:
    print("=" * 50)
    print("  Horsey Describer — Cryptid Research Unit")
    print("=" * 50)
    print(f"  NUMPAD_0 → Screenshot only (no API call)")
    print(f"  NUMPAD_1 → Short & Punchy")
    print(f"  NUMPAD_2 → Medium Rant")
    print(f"  NUMPAD_3 → Full Monologue")
    print(f"  OBS source to jiggle: {OBS_JIGGLE_SOURCE}")
    print("  Press CTRL+C to exit.\n")

    with keyboard.Listener(on_press=on_press) as listener:
        listener.join()


if __name__ == "__main__":
    main()