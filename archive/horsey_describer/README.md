# Horsey describer (ARCHIVED)

A one/two-stream experiment that seemed interesting and then wasn't: hotkey-triggered AI describer for Horsey Game stream abominations. Numpad hotkey → fullscreen screenshot → Anthropic vision API describes the abomination → TTS speaks it while OBS jiggles a source → a rendered markdown report shows in a browser source. Retired; kept for parts.

Everything here ran on the streaming desktop, never the Pi — it needs the screen to screenshot, hotkeys, local OBS websocket access, and audio out.

Contents: `horsey_describer.py` (the main hotkey/vision/TTS loop), `region_finder.py` (helper to find screen coordinates), `report_renderer.py` + `report.css` (markdown → styled HTML report for the OBS browser source), `temp/` (its screenshot/report output dirs).

If ever revived: it imports `OBSWebsocketsManager` from `OBS_Websocket.py`, which now lives in `desktop_tools/` — run with `PYTHONPATH=../../desktop_tools` from this directory (or copy the file back next to it), and note the `./temp/...` output paths are relative to the working directory.
