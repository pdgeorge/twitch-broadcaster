# TODO

## DnD Project (`dnd/`)
- [ ] Test full pipeline end to end (mic → whisper → claude → tts → obs jiggle)
- [ ] Test ChatPlayer RabbitMQ consumer + chat window countdown
- [ ] Flesh out Dabbert and Hivemind personalities in `characters/dabbert.json` and `characters/chat.json`
- [ ] Decide on OBS sources for Dabbert vs Hivemind jiggle (currently both default to `OBS_JIGGLE_SOURCE`)
- [ ] Consider a "DM adds context manually" mode (press a key to type a note into session log without mic)
- [ ] Session log persistence — save to disk so a crash doesn't wipe the whole session history

## Refactor / Tech Debt
- [ ] Create `lib/` folder and move shared files into it:
  - `OBS_Websocket.py`
  - `tiktok_tts.py`
- [ ] Once `lib/` exists, update horsey imports to use `lib/`
- [ ] Move horsey files (`horsey_describer.py`, `report_renderer.py`, `report.css`, `region_finder.py`) into `horsey/`
- [ ] Move Pi services (`docker-compose.yml`, `echoes_of_chat/`, `overlay/`, `overlay_controller/`, `twitch_receiver/`) into `pi/`
- [ ] Consider `pyproject.toml` at root to make `lib/` a proper installable local package

## General
- [ ] Remember to tick off these as they are completed (Long term)