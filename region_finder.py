"""
pick_region.py
--------------
Click top-left corner of your target region, then bottom-right.
Prints the SCREENSHOT_REGION tuple and exits.
Press CTRL+C to cancel.
"""
from pynput import mouse

clicks = []

def on_click(x, y, button, pressed):
    if not pressed:
        return
    clicks.append((x, y))
    if len(clicks) == 1:
        print(f"  Top-left:     ({x}, {y})")
        print(f"  Now click bottom-right corner...")
    elif len(clicks) == 2:
        x1, y1 = clicks[0]
        x2, y2 = clicks[1]
        print(f"  Bottom-right: ({x2}, {y2})")
        print(f"\nSCREENSHOT_REGION = ({x1}, {y1}, {x2}, {y2})")
        return False  # stop listener

print("Click top-left corner of the region you want to capture...")
with mouse.Listener(on_click=on_click) as listener:
    listener.join()