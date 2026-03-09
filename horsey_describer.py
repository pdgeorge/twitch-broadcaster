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

from obswebsocket import requests as obs_requests
from OBS_Websocket import OBSWebsocketsManager
from tiktok_tts import tiktok_tts
from report_renderer import render_report

load_dotenv()

# ---------------------------------------------------------------------------
# Config
# ---------------------------------------------------------------------------
ANTHROPIC_API_KEY    = os.getenv("ANTHROPIC_API_KEY")
OBS_JIGGLE_SOURCE    = os.getenv("OBS_JIGGLE_SOURCE", "HorseIcon")
OBS_REPORT_SOURCE    = os.getenv("OBS_REPORT_SOURCE", "HorseReport")
TIKTOK_TOKEN         = os.getenv("TIKTOK_TOKEN")
REPORTS_DIR          = os.getenv("REPORTS_DIR", "./temp/reports")
REPORT_DISPLAY_SECS  = int(os.getenv("REPORT_DISPLAY_SECONDS", 30))

# Primary monitor capture region — adjust these to taste
# Format: (left, top, right, bottom) in pixels
# Default: full 1920x1080 primary monitor
SCREENSHOT_REGION = (0, 0, 1920, 1080)

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
    "report": {
        "label": "Full Field Report",
        "prompt_report": (
            "You are an unhinged cryptid researcher writing an official classified field report. "
            "Write a structured report in markdown about the creature or abomination in the screenshot. "
            "Use markdown headings (##) for sections. Include sections for: Physical Description, "
            "Dietary Preferences, Spiritual Implications, Government Cover-Up, and Why Horses Were A Mistake. "
            "Be detailed, paranoid, and scientifically unhinged. "
            "Do NOT use asterisks for bullet points — use actual markdown list syntax (- item). "
            "Do NOT write anything that would sound weird read aloud — this is a written document only."
        ),
        "prompt_summary": (
            "You are an unhinged cryptid researcher giving a live spoken summary of your latest field report. "
            "Summarise the key findings in exactly THREE to FOUR sentences, spoken word only. "
            "No markdown, no formatting, no bullet points, no asterisks. "
            "Be alarmed, be weird, end on something deeply unsettling. "
            "Speak as if you are addressing a live audience who cannot see the report."
        ),
    },
}

# Numpad keys with numlock ON report as plain chars — map char to mode
NUMPAD_CHAR_MAP = {
    '0': "screenshot_only",
    '1': "short",
    '2': "medium",
    '3': "report",
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
MAX_ROTATION = 45.0  # degrees at peak amplitude

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


def _claude_vision_call(system_prompt: str, img_b64: str, user_text: str, max_tokens: int = 1000) -> str | None:
    """Single Claude vision call. Returns response text or None on error."""
    try:
        response = anthropic_client.messages.create(
            model="claude-sonnet-4-20250514",
            max_tokens=max_tokens,
            system=system_prompt,
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
                        {"type": "text", "text": user_text},
                    ],
                }
            ],
        )
        return response.content[0].text
    except Exception as e:
        print(f"[Horsey] Claude API error: {e}")
        return None


def _run_report_pipeline(img_b64: str) -> None:
    """
    Mode 3 pipeline:
      1. Claude call 1 → full markdown report → saved as .md + rendered to current_report.html
      2. OBS displays report via temp_display_report (you implement this in OBSWebsocketsManager)
      3. Claude call 2 → 3-4 sentence spoken summary → TTS + jiggle
    """
    mode = MODES["report"]

    # 1. Generate full report
    print("[Horsey] Generating field report...")
    report_md = _claude_vision_call(
        system_prompt=mode["prompt_report"],
        img_b64=img_b64,
        user_text="Write the field report for this abomination.",
        max_tokens=2000,
    )
    if not report_md:
        print("[Horsey] Report generation failed.")
        return

    print(f"[Horsey] Report generated ({len(report_md)} chars)")

    # 2. Render HTML and display in OBS
    html_path = render_report(report_md, reports_dir=REPORTS_DIR)
    threading.Thread(
        target=_obs_display_report,
        args=(html_path,),
        daemon=True
    ).start()

    # 3. Generate spoken summary (separate Claude call)
    print("[Horsey] Generating spoken summary...")
    summary = _claude_vision_call(
        system_prompt=mode["prompt_summary"],
        img_b64=img_b64,
        user_text="Summarise your field report findings for the live audience.",
        max_tokens=300,
    )
    if not summary:
        print("[Horsey] Summary generation failed.")
        return

    print(f"[Horsey] Summary:\n{summary}\n")

    # 4. TTS + jiggle on summary
    envelope, player, tmp_path = speak_tts(summary)
    if player is None:
        print("[Horsey] TTS failed.")
        return

    jiggle_thread = threading.Thread(
        target=_obs_jiggle, args=(envelope, player), daemon=True
    )
    jiggle_thread.start()

    while player.get_state() not in (vlc.State.Ended, vlc.State.Error, vlc.State.Stopped):
        time.sleep(0.1)
    player.release()
    if tmp_path:
        os.unlink(tmp_path)


def _obs_display_report(html_path: str) -> None:
    """
    Tell OBS to refresh the Browser Source and display it for REPORT_DISPLAY_SECS.
    Calls temp_display_report() on OBSWebsocketsManager — implement that in OBS_Websocket.py.
    """
    try:
        import asyncio
        obs_mgr = OBSWebsocketsManager()
        asyncio.run(obs_mgr.temp_display_report(OBS_REPORT_SOURCE, REPORT_DISPLAY_SECS))
    except Exception as e:
        print(f"[OBS] Report display failed (non-fatal): {e}")


def describe_abomination(mode_key: str) -> None:
    """Full pipeline: screenshot → Claude vision → TTS + OBS jiggle."""
    mode = MODES[mode_key]
    print(f"\n[Horsey] Triggered: {mode['label']}")

    # 1. Screenshot
    print("[Horsey] Capturing screen...")
    img_b64 = _screenshot_to_b64()

    # 2. Report mode has its own pipeline
    if mode_key == "report":
        _run_report_pipeline(img_b64)
        return

    # 3. Standard pipeline — single Claude call
    print("[Horsey] Sending to Claude...")
    description = _claude_vision_call(
        system_prompt=mode["prompt"],
        img_b64=img_b64,
        user_text="Describe this abomination.",
    )
    if not description:
        return

    print(f"\n[Horsey] Description:\n{description}\n")

    # 4. TTS + envelope
    envelope, player, tmp_path = speak_tts(description)
    if player is None:
        print("[Horsey] TTS failed, skipping jiggle.")
        return

    # 5. Jiggle in sync with live playback
    jiggle_thread = threading.Thread(
        target=_obs_jiggle, args=(envelope, player), daemon=True
    )
    jiggle_thread.start()

    # 6. Wait for VLC to finish, then clean up
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
    print(f"  NUMPAD_3 → Full Field Report (HTML + spoken summary)")
    print(f"  OBS jiggle source:  {OBS_JIGGLE_SOURCE}")
    print(f"  OBS report source:  {OBS_REPORT_SOURCE}")
    print(f"  Reports directory:  {REPORTS_DIR}")
    print("  Press CTRL+C to exit.\n")

    with keyboard.Listener(on_press=on_press) as listener:
        listener.join()


if __name__ == "__main__":
    main()