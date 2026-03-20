"""
dnd_session.py
--------------
Main entry point for the D&D AI player system.

Hotkeys:
  1          → Hivemind (ChatPlayer): mic + chat window simultaneously for listen_seconds
  2          → Dabbert (AIPlayer):  first press = start mic + screenshot,
                                    second press = stop + transcribe + send
  0          → Screenshot only (debug, no API call)
  S          → Toggle screenshot mode ON/OFF (any time)
  Ctrl+C     → Exit

Setup:
  - Place dabbert.json and chat.json in the dnd/ directory
  - Fill in .env (see below)

.env keys:
  ANTHROPIC_API_KEY
  TIKTOK_TOKEN
  RABBITMQ_URL
  RABBITMQ_EXCHANGE   (default: twitch_events)
  OBS_HOST            (default: localhost)
  OBS_PORT            (default: 4455)
  OBS_PASSWORD
  OBS_JIGGLE_SOURCE   (default: HorseIcon)
"""

import os
import threading
import time
from pynput import keyboard
from dotenv import load_dotenv

from ai_player import AIPlayer, ScreenshotFlag
from chat_player import ChatPlayer

load_dotenv()

# ---------------------------------------------------------------------------
# Shared state
# ---------------------------------------------------------------------------
session_log     = []
screenshot_flag = ScreenshotFlag(enabled=True)

# ---------------------------------------------------------------------------
# Load players
# ---------------------------------------------------------------------------
print("=" * 55)
print("  D&D AI Player System")
print("=" * 55)

dabbert  = AIPlayer("dnd/dabbert.json", session_log, screenshot_flag)
hivemind = ChatPlayer("dnd/chat.json", session_log, screenshot_flag)

# ---------------------------------------------------------------------------
# Hotkey map
# ---------------------------------------------------------------------------
CHAR_MAP = {
    '1': hivemind,
    '2': dabbert,
}

def _screenshot_only():
    """Debug: screenshot with no API call."""
    import base64, io, time
    from PIL import ImageGrab
    from ai_player import SCREENSHOT_DIR, SCREENSHOT_REGION
    os.makedirs(SCREENSHOT_DIR, exist_ok=True)
    img = ImageGrab.grab(bbox=SCREENSHOT_REGION)
    ts = time.strftime("%Y%m%d_%H%M%S")
    path = os.path.join(SCREENSHOT_DIR, f"debug_{ts}.jpg")
    img.save(path, format="JPEG", quality=85)
    print(f"[Debug] Screenshot saved → {path}")

def on_press(key):
    char = getattr(key, 'char', None)

    if char == '0':
        threading.Thread(target=_screenshot_only, daemon=True).start()
        return

    if char and char.lower() == 's':
        screenshot_flag.toggle()
        return

    player = CHAR_MAP.get(char)
    if player:
        player.on_hotkey()

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------
def main():
    print()
    print(f"  Hotkeys:")
    print(f"    1        → {hivemind.name} (mic + chat window, {hivemind.listen_seconds}s)")
    print(f"    2        → {dabbert.name} (mic toggle)")
    print(f"    0        → Screenshot only (debug)")
    print(f"    S        → Toggle screenshot ON/OFF")
    print(f"    Ctrl+C   → Exit")
    print()
    print(f"  Screenshot: {'ON' if screenshot_flag.enabled else 'OFF'}")
    print(f"  Session log limit: {dabbert.context_limit} entries (pop {dabbert.pop_count} when full)")
    print(f"  Log files → {os.path.join(os.path.dirname(__file__), 'logs')}/")
    print()
    print("  Ready. Waiting for hotkeys...\n")

    with keyboard.Listener(on_press=on_press) as listener:
        listener.join()

if __name__ == "__main__":
    main()