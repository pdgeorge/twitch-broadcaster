# PLANS — future stream systems

Ideas from the 2026-07-12 brainstorm, seeded by the archived horsey describer (`archive/horsey_describer/`). Its reusable skeleton: **trigger → perceive stream state → AI persona transforms it → ceremony (voice + document + OBS motion)**. This stack adds a perception channel horsey never had: structured events (rolls, deaths, level-ups, redemptions) on the RabbitMQ bus, plus a MySQL row per chatter. The ideas below all lean on that.

Related repos: `../DabiReborn` (Dabi the derpy unicorn — stream brain, TTS, avatar), `~/Bucket/development/bob-twitch-chatter` (AI chatter that watches stream + chat and chats occasionally).

Dropped: the "Tavern Chronicler" (AI bard writing campaign history from screenshots) — West Marches sessions are mostly theatre of the mind, so there's nothing reliable on screen to perceive, and feeding it context manually (`!bard <characters> <info>`) makes the DM do the work the bot was supposed to save.

---

## YES: Chat logging → Chat on Trial

**Status: committed to, high priority. Chat logging is the prerequisite and has standalone value; the trial is the payoff.**

### Part 1 — log all chat messages

Twitch explicitly permits (encourages) broadcasters logging their own channel's chat. New MySQL table, written by `overlay_controller` — it already parses every `channel.chat.message` and owns MySQL, so this is one INSERT in `handleChatEvent`, no new service:

```sql
CREATE TABLE chat_log (
  id BIGINT AUTO_INCREMENT PRIMARY KEY,
  user_id BIGINT NOT NULL,
  user_login VARCHAR(64) NOT NULL,
  message TEXT NOT NULL,
  created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
  INDEX (user_id, created_at)
);
```

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
