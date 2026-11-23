# PLANS — future stream systems

Ideas from the 2026-07-12 brainstorm, seeded by the archived horsey describer (`archive/horsey_describer/`). Its reusable skeleton: **trigger → perceive stream state → AI persona transforms it → ceremony (voice + document + OBS motion)**. This stack adds a perception channel horsey never had: structured events (rolls, deaths, level-ups, redemptions) on the RabbitMQ bus, plus a MySQL row per chatter. The ideas below all lean on that.

Related repos: `../DabiReborn` (Dabi the derpy unicorn — stream brain, TTS, avatar), `~/Bucket/development/bob-twitch-chatter` (AI chatter that watches stream + chat and chats occasionally).

Dropped: the "Tavern Chronicler" (AI bard writing campaign history from screenshots) — West Marches sessions are mostly theatre of the mind, so there's nothing reliable on screen to perceive, and feeding it context manually (`!bard <characters> <info>`) makes the DM do the work the bot was supposed to save.

---

## YES: Chat logging → Chat on Trial

**Status: committed to, high priority. Chat logging is the prerequisite and has standalone value; the trial is the payoff.**

### Part 1 — log all chat messages ✅ done

Twitch explicitly permits (encourages) broadcasters logging their own channel's chat. `overlay_controller` writes every `channel.chat.message` to the `chat_log` MySQL table (`chatLogStore` in `main.go`) — every message, commands and bots included; filter at read time. Columns: `message_id` (Twitch's id, unique so redeliveries are ignored), `user_id`, `user_login`, `user_name`, `message`, `deleted`, `created_at`.

Moderated messages are never removed — they did send them. Instead `deleted` is flipped to TRUE when a mod deletes a message (`channel.chat.message_delete`) or a timeout/ban clears a user's messages (`channel.chat.clear_user_messages`, which flags that user's last 24h). Anything that shows chat history on screen should hide `deleted` rows by default.

Keep it lean: text only, no fragments/emote metadata (re-derivable if ever needed). At hobby scale this is tiny; keep forever. Courtesy rules: honor "delete my data" requests, and consider purging rows for banned bots. Standalone value even before the trial: grep-able chat history, data for the Tavern Keeper and personality systems below.

### Part 2 — the trial

Chat on Trial: the AI prosecutor indicts a chatter using their **real chat history** as evidence, the streamer acts as defense counsel, chat votes guilty/innocent.

Flow sketch:
1. **Indictment**: DM runs `!trial <name>` (or a channel-point redeem "put someone on trial" for chat-instigated chaos). A new `court_service` builds the case: pull the accused's `chat_log` rows (recent + random sample + simple "greatest hits" heuristics like most-repeated phrases), send to the Anthropic API as a pompous prosecutor persona, get back a formal accusation with quoted evidence.
2. **Presentation**: accusation renders on the overlay (billboard takeover or a courtroom panel) and is spoken through the existing TTS pipeline (prosecutor gets a fixed voice). Streamer defends live — no tech needed, that's the show.
3. **Verdict**: a Twitch poll. The receiver **already subscribes** to `channel.poll.begin/progress/end`, so the DM creates the poll from the dashboard and `court_service`/overlay consume the result — zero new EventSub work. (Later: create the poll via Helix API automatically.)
4. **Sentence**: plugs straight into the West Marches systems — a `CONVICTED` cosmetic via the existing `!give` path, an exp fine via `!smite`-style deduction, or (best visual) the guilty dude rendered in stocks in a corner of the tavern for the rest of stream.

How it maps onto this system: `court_service` is a small Python service on the Pi next to `tts_service` — it binds its own queue to the `twitch_events` fanout (the DabiReborn `dabi-stream-brain` pattern, exactly), watches for the `!trial` command itself, has docker-network access to MySQL for evidence, and publishes `court.*` events back onto the bus for the overlay to render. `overlay_controller` needs no LLM dependency and barely any changes (render the `court.*` payloads; the chat_log INSERT).

Open decisions: trial cadence (rare = better, it's a ceremony), whether chat can instigate via redeem or DM-only, sentence severity, prosecutor personality (this is a great fit for the evolving-personality pattern below — the prosecutor gets more unhinged as his conviction rate drops).

---

## YES: Dabling breaks — Derby first, then Expedition

**Status: committed to (2026-11-23). Derby first; it also builds the shared intermission frame the Expedition reuses.**

Mid-stream breaks of 3–5 minutes where chat's Dablings do the playing and chat watches, cheers and pokes at it. Dabi and the Dablings are the cast, not the LLM: nothing here needs Dabi's model, though he can host (see "Dabi's part").

The two games reward different people on purpose:

- **Derby is for everyone.** Mostly luck, with catch-up for the back of the pack, so someone who chatted for the first time ten minutes ago can win. Level barely matters.
- **Expedition is for the regulars.** Level decides how deep a Dabling gets, so long-time chatters feel their exp pay off.

If level drove both, the same few people would win everything and new viewers would stop joining.

### The intermission frame (built with the Derby, shared by both)

Same shape as Dabi's shop, which already proved it: a broadcaster command switches the OBS scene and the music through pd-streamdeck, a browser source draws the scene, the controller owns the state, and closing puts back what was on before.

1. **Start (broadcaster only)**: `!race` or `!dungeon`. Scene and music switch, chat is told how to join.
2. **Join window (~60 s)**: anyone with a Dabling types `!join`. Free, so everyone plays. The scene shows Dablings lining up as they join.
3. **Run**: the controller simulates the whole thing in Go from the `chatters` rows (deterministic from a seed, so a run can be replayed or tested), and streams it to the browser source over the overlay websocket as a timeline the page plays back as a cartoon. Chat's inputs (boosts, votes) feed into the simulation while it runs.
4. **Results card**: placings, who got what, then payouts are written to the `chatters` row.
5. **Back**: scene and music return to whatever was on.

Rendering reuses what the tavern floor already draws: tinted Dablings wearing their equipped cosmetics, growing with level.

### Dabling Derby

Dablings in little cars, driving laps.

- **Look**: one car sprite with the Dabling sat in it, wearing its hat and tint, so it reads even with 30 racers. Side view or top-down track, whichever reads better at that count.
- **Race**: a fixed number of laps. Each car's speed wobbles at random, with small events (spin-outs, a shortcut, a pit stop), and cars at the back get a little catch-up so the race stays open until the end.
- **`!boost`**: a racer types it to get a small burst of speed. Limited (e.g. a few per race, with a cooldown) so spamming doesn't win, but typing at the right moment helps. Shows as a puff on screen so chat sees it happen.
- **Rewards**: everyone who finishes gets a participation reward, placings get a bit more (see "Rewards").

### Dabling Expedition

Dablings go on a dungeon crawl, fight enemies and pick up loot on the way. The higher a Dabling's level, the further it's likely to get.

- **Paths by vote**: at forks chat votes which way the party goes (left door / right door, the bridge or the cave). A quick in-chat vote (`!1` / `!2` for 15 s) rather than a Twitch poll, so it stays fast.
- **Level**: a Dabling's level sets how much it can take. Higher levels go deeper, lower ones drop out earlier but still get something.
- **Getting knocked out ends the run, not the character**: separate from Echoes of Chat's HP, death and `!revive`. A break should be fun, not cost anyone progress.
- **Loot**: what you find depends on how deep you got and the path chat picked.
- Roles/classes from the D&D side could plug in later (fighters in front, clerics keeping others up), but that's paused, so the Expedition works on level alone.

### Rewards

- **Participation cosmetics** from both: everyone who joins gets something, so showing up is what counts.
- **Dated or event-marked items** ("Derby, Dec 2026", "Reached floor 5") so they become a record of having been there.
- **Kept apart from the shop's stock** (event-only items, or a trophy/badge slot) so drops don't undercut what Dabi sells.
- Gold can come out of these too, once there's something to spend it on.

### Dabi's part (optional, no model needed)

Dabi hosts: race marshal on a stool by the finish line, or quest-giver at the dungeon door, using `dabi.body` the way the shop does. Canned lines work. An LLM line at the start or the end is optional, since his ~10–15 s reply time only matters mid-run.

### Open decisions

- `!boost` free with a limit, or costing gold? Gold only comes from `!give` today, so free-with-a-limit is the likely start.
- How many laps / how long a race runs, and how often a break happens (rare enough to feel like an event).
- Side view or top-down track for the Derby.
- Which cosmetics to make for the first Derby, and whether there's a trophy slot or they use the existing slots.
- Whether chat can bet on the Derby, once gold has a use.

---

## MAYBE: Tavern Keeper

An NPC who *knows* the regulars, because the DB already does: `chatters` has level, logins, deaths, cosmetics — and once chat logging lands, what they talk about.

The reframe that earned the MAYBE: not a new redeem, a **coat-of-paint swap on existing ones**. "Ask Dabi a question" becomes (or gets a sibling) "Ask the Tavern Keeper"; the "daily login bonus" redeem keeps its logins-increment mechanics but the keeper greets you when you redeem it, sheet-aware ("back again, dabihatesyou? that's 47 visits, and you've died twice this month — the usual?").

How it maps: the redeem already flows through the bus; DabiReborn's `dabi-stream-brain` handles "Ask Dabi a question" today via a title-matched `channel_point.py` handler. The keeper is either a second persona in `dabi-stream-brain` (another `LLMService` instance with a keeper system prompt — the class supports it, it's constructed per-instance) or a WM-native handler in a service on the Pi. Deciding factor: whose voice/avatar responds. If the keeper speaks through the tavern overlay with a tavern voice, WM-side owns it; if it's Dabi wearing an apron, DabiReborn owns it. Context per greeting = the chatter's DB row + a few recent chat_log lines; needs a per-chatter cooldown so he's a presence, not a spambot.

---

## MAYBE: Full-stream transcription → recap short (own repo)

Post-stream, produce a 1–5 minute "previously on..." summary short, possibly delivered by Dabi as an AI vtuber (DabiReborn already has the TTS + Live2D avatar + stream_client pipeline to present it).

Why its own repo: it's desktop-bound (GPU for Whisper, access to stream audio), runs on a different lifecycle (post-stream batch, not live services), and is useful beyond West Marches streams. `bob-twitch-chatter` watches the stream but doesn't transcribe the full thing — this repo would own transcription as a first-class artifact that bob (and anything else) could consume later.

Sketch for the new repo:
1. **Capture**: desktop records or taps stream audio (OBS multi-track recording is the easy path — game/mic tracks already separated), Whisper transcribes locally on the GPU, chunked, → timestamped transcript.
2. **Structured beats**: a tiny consumer tees `twitch_events` to a JSONL session log (rolls, deaths, level-ups, redeems, raids — `desktop_tools/background.py` is already 80% of this consumer). Timestamps let beats align with transcript moments.
3. **Condense**: LLM pass over transcript + beats → recap script with the big moments, written for a persona.
4. **Deliver**: script → DabiReborn's TTS/avatar pipeline for a Dabi-presented short, and/or plays as the cold open of the next stream via browser source.

The transcript is also the missing input that would resurrect the dropped Chronicler idea without the theatre-of-the-mind problem — the bard could chronicle what was *said*, not what was on screen. Worth remembering if this repo happens.

---

## Pattern note: evolving personality (the DougDoug joke-bot mechanic)

The mechanic: chat submits material, the bot performs it, chat's reaction adjusts the bot's personality over time. DougDoug adjusted the personality by hand; here's how it automates in this stack.

**Personality-as-data**: DabiReborn's `LLMService` already loads its system prompt from a JSON file (`shared/dabi.json`) — the personality is *already* data, it just never changes. The evolving version stores the personality doc in MySQL (or a versioned file), and after each performance runs an **editor pass**: a second LLM call taking (current personality + the joke/performance + the measured reaction) → an amended personality, with hard bounds ("stay under N words", schema-checked) so it can't bloat or derail. Every version is kept, so `!personality show` / `!personality revert <n>` gives the streamer a rollback lever when chat inevitably trains it into a monster.

**Measuring reaction with what's already on the bus**: channel-point "rate this" redeems, a quick Twitch poll (`channel.poll.*` already subscribed), or dumbest-and-honest: emote/message volume in the 30s after the performance, straight out of `chat_log`.

Applies to: a joke bot if that stream happens, the trial prosecutor (personality drifts with conviction rate), the Tavern Keeper (develops grudges and favorites), or Dabi himself. It's a pattern, not a project — build it the first time one of the above needs it, in `shared/llm_service.py` where the personality already lives.
