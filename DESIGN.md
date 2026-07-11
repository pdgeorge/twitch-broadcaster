# Tavern & West Marches Overlay — Design Document

Status: v3 (2026-07-09), supersedes v2. M1 implemented, untested by the
streamer yet; v3 finalizes the exp/level curve (was a placeholder in v2) and
adds two forward-looking specs for M2/stretch: level-scaled tavern dude size,
and the `1d20+level` encounter check. This doc is the spec for extending `twitch-broadcaster`
(receiver → RabbitMQ fanout → overlay_controller) with a persistent West
Marches-style campaign driven by chat, plus a "tavern" ambient HUD. It
assumes the existing architecture: `main.py` (twitch_receiver) publishes
EventSub events to the `twitch_events` fanout exchange and consumes
`twitch_commands`; `main.go` (overlay_controller) consumes events, owns
MySQL, and pushes typed JSON to the overlay browser source over the
`/ws/overlay` websocket hub.

## 1. Concept

A West Marches campaign for a small community. There is no fixed party: chatters'
characters "plop in" (possession) for a limited time, stronger or weaker based on
their real history with the channel. The world persists in MySQL across streams.
Inspiration: DougDoug's ChatGodApp for the possession mechanic (grab chatter, they
act through the stream); this project adds the persistent character sheet and
earned power that ChatGod lacks.

Core loop: chatter redeems possession → controller grabs their character sheet
from MySQL → character joins the active party (max 4) → DM (the streamer)
runs the encounter, adjusting sheets with DM-only commands → results persist.

Non-goal (v2 change): there is no long-term campaign/season tracking for now
(see §8) — dropped for scope, may return later.

## 2. Screen layout (from streamer's sketch)

1920×1080 OBS canvas. All new elements live in the bottom "ground strip";
the large upper-right region stays free for game/content.

```
+------+------------------------------------------------+
| C    |                                                |
| H    |                                    [party cards]|
| A    |            GAME / CONTENT AREA                 |
| T    |                                                |
+------+--------+-----------------+---------------------+
|   CAM         |  !OTHER         |   TAVERN            |
|  (480x300)    |  billboard      |   (dudes live here) |
+---------------+-----------------+---------------------+
```

Chat rail: left edge, full height above the cam. Cam: bottom-left, ~480×300.
Billboard: immediately right of cam — this is the existing `!other` /
announcement pipeline rendered as a wooden town notice board. Tavern: right of
the billboard to the right edge. Party cards: ride above the tavern roof.
The streamer will draw a background; the overlay renders on transparency, so
geometry must match the drawing (final coordinates TBD once art exists).

**v2 status**: party cards are live now (`overlay/overlay.css` `.party-cards`),
anchored bottom-right with a placeholder position since the current billboard
box (`left:727 width:1160`) already spans most of the strip — real geometry
still waits on the streamer's art. `overlay/assets/dude.svg` (mini avatar,
tinted via CSS `hue-rotate`) and `overlay/assets/tavern.svg` (crude building
placeholder, not wired into `index.html` yet) exist so there's something to
iterate against.

Removed (still pending, not part of M1): the pong game — delete
`startPongTicker`, the `"ping"` trigger in `handleChatEvent`, and the pong
methods/fields on `otherManager`. Kept: the full `!other` markdown pipeline,
the announcement redemption with 5-minute timer, and `!fire` cancel —
unchanged backend, new billboard styling client-side is still TODO.

## 3. Progression rules

Exp source: chat messages during stream. Flat exp per message with a 45s
per-chatter cooldown (memory-only, lost on controller restart — that's fine,
it just re-opens). `exp = 10 + logins/10` per successful grant, written to
the `chatters` row immediately (not batched). `logins` is the same counter
the existing "daily login bonus" redemption increments.

There is deliberately NO taming curve: high-login veterans are allowed to be
"gods among men."

**Level curve (v3, finalized)**: a triangular curve anchored at "level 2 costs
10 exp." Cost to go from level *L* to *L+1* grows linearly (10, 20, 30, ...),
so cumulative exp grows quadratically:

```
total_exp(level) = 5 * level * (level - 1)
```

| level | total exp | exp for *this* level |
|------:|----------:|----------------------:|
| 1     | 0         | —                      |
| 2     | 10        | 10                     |
| 3     | 30        | 20                     |
| 5     | 100       | 40                     |
| 10    | 450       | 90                     |
| 20    | 1900      | 190                    |
| 30    | 4350      | 290                    |

Rejected alternatives: a log curve would make each level *cheaper* than the
last, which runs backwards from "leveling should feel earned"; a compounding
exponential curve (e.g. `1.5^level`) explodes so fast that levels past ~15-20
become practically unreachable, undercutting the "gods among men" idea by
making it unattainable rather than rare. Triangular sits in between — fast,
rewarding early levels, a real grind later, no hard wall.

Implemented as `totalExpForLevel`/`levelForExp` in `overlay_controller/main.go`
(inverts the formula via the quadratic equation, `int(math.Floor(...))`).
`max_hp = 10 + level*4`; level-up restores HP to the new max.

**Known pacing caveat**: at `10 + logins/10` exp/message on a 45s cooldown, a
very active chatter can reach level ~20+ within a single long stream purely
from message volume, largely independent of their login history. If level
should track veteran status specifically rather than single-session grinding,
that's a knob to revisit (e.g. weight exp gain more heavily by `logins`, or
cap session exp) — not solved here, flagged for playtesting.

Login-count unlock thresholds (10/30/60 logins → ability slot / resurrection
token / prestige class+TTS) are **not implemented yet** — deferred past M1,
tracked for a later milestone (see §7).

Death is real: `alive=0` persists. Whether death also resets `logins` is
still an open DM decision — default: it does not.

Catch-up flavor (optional, post-MVP): exp gain +50% while below the party's
median level.

## 4. Data model (MySQL)

```sql
ALTER TABLE chatters
  ADD COLUMN IF NOT EXISTS level  INT  NOT NULL DEFAULT 1,
  ADD COLUMN IF NOT EXISTS hp     INT  NOT NULL DEFAULT 14,
  ADD COLUMN IF NOT EXISTS max_hp INT  NOT NULL DEFAULT 14,
  ADD COLUMN IF NOT EXISTS alive  TINYINT(1) NOT NULL DEFAULT 1,
  ADD COLUMN IF NOT EXISTS sheet  JSON NULL;  -- reserved, unused: class/abilities/inventory/unlocks
```

Implemented in `characterStore.init` (`overlay_controller/main.go`), run at
startup alongside the existing `chatters` table creation.

`cosmetics` JSON (pre-existing column) is now live: `!give <name> <string>`
appends a cosmetic tag to it directly — no cosmetics table yet. Long-term:
a `cosmetics` reference table to validate `!give` against ("does this exist?
yes → grant, no → error"), purchasable with `money`. Until a chatter has
cosmetics, their tavern sprite variant is `hash(username) % 9` — deterministic,
zero storage, stable across streams.

**v2: no `campaign` table.** The season/win-condition system from v1 is cut
entirely for now (see §8) — not deferred, removed from scope.

## 5. Events and message flow

Inbound (twitch_events exchange → overlay_controller):
`channel.chat.message` additionally drives exp-on-message;
`channel.channel_points_custom_reward_redemption.add` gains a new reward
title `"join the party"` (possession) alongside the existing
`"daily login bonus"` and `"announcement"`. **The "join the party" reward
must be created manually in the Twitch dashboard before this works** —
reward matching is an exact (case-insensitive) title string, same as the
existing rewards.

Websocket messages (hub → browser), implemented:

```jsonc
{"type": "party.update", "members": [
  {"name":"dave","level":3,"hp":12,"max_hp":20,"exp":340,"exp_next":400,
   "variant":7,"status":"possessed"}]}

{"type": "tavern.possess", "name":"dave"}   // sent, not yet consumed client-side (M2)
{"type": "tavern.return",  "name":"dave"}   // sent, not yet consumed client-side (M2)
{"type": "tavern.death",   "name":"dave"}   // sent, not yet consumed client-side (M2)
{"type": "other.update", ...}               // unchanged (billboard)
```

`tavern.enter` / `tavern.chat` / `tavern.levelup` from v1 are not sent yet —
they belong to the tavern scene itself (M2), which doesn't exist client-side.

**v2: no expiry.** Possession has no timer, no `expires_at`, and is never
auto-yanked. A character stays in the party until `!kick <name>` (single
member) or `!newparty` (whole party, and only way to reopen slots once full)
— removing the "yanked mid-fight" mechanic from v1 entirely for now.

Possession lifecycle (as implemented in `handlePossessionRedeem`): redemption
→ load-or-create sheet from MySQL → refuse if `alive=0` (no resurrection-token
check yet — that unlock doesn't exist, see §3) or if the party is already at 4
→ add to party, `tavern.possess` + `party.update`. Refusals are chat-only,
points are **not** auto-refunded — no Twitch API call for that exists; the
streamer refunds manually if desired. This is intentional (see §8).

## 6. DM commands (broadcaster/mod only)

Gated with the existing `isAuthorizedForOther` badge/broadcaster check, in
`handleDMCommand`.

```
!grant <name> <exp|hp> <n>    exp/hp adjustment (+/- allowed); hp<=0 kills
!give  <name> <x>             integer x -> money; non-integer x -> cosmetic tag
!smite <name> <n>             damage; hp<=0 => death handling
!bless <name> <n>             heal, capped at max_hp
!kick  <name>                 eject one party member (no timer to override anymore)
!newparty                     eject the whole party, reopen all 4 slots
!sheet <name>                 broadcast a character's sheet to the billboard (30s)
!fire                         (existing) cancel active announcement
!other <markdown>              (existing) unchanged, still the base billboard content
```

**v2 removals**: `!extend` (no timer exists to extend) and `!season` (no
campaign table, see §8) are gone, not just unimplemented.

`money` has no source yet other than `!give` — same as cosmetics, both are
streamer-granted for now, no automatic sink/faucet.

## 7. Tavern HUD behaviors (client)

**Implemented (M1)**: party cards only — mini avatar swatch (crude SVG dude,
tinted via `hue-rotate(variant*40deg)`), name, level, HP bar with numbers, exp
bar with numbers, permanent glow (everyone in `party.members` is possessed by
definition now that there's no idle/wander state). Cards are the only element
allowed to sit above the ground strip, anchored bottom-right.

**Not implemented (M2, tavern scene)**: enter animation, chat speech-bubble +
hop, idle wander/wiggle, sleep after ~20 min silence, level-up spin, the
walk-to-door possess/return animation. None of this exists client-side yet —
`tavern.possess`/`tavern.return`/`tavern.death` are sent by the backend
already so M2 can consume them without backend changes.

**Death (v2, open question)**: `tavern.death` fires and `alive` persists as
0, but there's no agreed-on visual yet. v1 had "card flashes, 💀 message,
no respawn this stream" — streamer floated a graveyard as a maybe. Left open;
revisit once M2 tavern scene work starts.

Avatars MVP: implemented as a single SVG (`overlay/assets/dude.svg`), tinted
into 9 variants via CSS `hue-rotate`, selected by `hash(username) % 9`.
Stretch: per-chatter cosmetics from the `cosmetics` JSON column — the hash
stays the fallback for chatters without cosmetics.

**Ambient dude size scales with level (v3, spec for M2, not implemented)**:
in the tavern scene proper (the ambient population of chatters who are
*wandering*, not possessed — a feature that doesn't exist client-side yet),
each dude's rendered size scales with their character's level. The point is
to make level legible outside the party too, so someone who'd rather vibe in
chat than redeem "join the party" still gets to visibly be a big deal in the
tavern crowd — level isn't only a party-combat stat. Proposed formula, tune
on sight once there's an actual tavern to look at:

```
scale = min(1 + (level - 1) * 0.08, 2.5)   // 64px base -> ~160px cap around level ~19+
```

Capped so a max-level veteran reads as "notably huge," not "bigger than the
building." This requires the tavern's ambient roster (who's present, their
level) which is separate state from `partyManager` — the possessed party is
a subset of, not the same as, everyone who's chatted recently. Out of scope
until the tavern scene itself is built; party cards (M1) stay a fixed size
regardless of level, since they're a UI card, not the ambient dude.

## 7a. Encounter resolution (DM ruling, v3)

Simple check mechanic for the DM to adjudicate possessed-party encounters:
**`1d20 + level` vs. a challenge number**, success/fail, no other modifiers.
This is a manual DM ruling for now — roll and decide narratively, same as
any `!smite`/`!bless` adjustment. A `!check <name> <dc>` command that rolls
the d20 and replies success/fail would be a small, self-contained addition
whenever it's wanted (rand + compare + chat reply, no new state) — listed as
a stretch candidate in §10, not built yet since it wasn't asked for.

## 8. Explicit non-goals / decisions log

No log-curve exp multiplier (veterans are allowed to dominate — community
lore). Level curve is triangular, not exponential — chosen deliberately over
a compounding curve so high levels stay hard, not impossible (v3, §3). No
server-side animation — browser owns all tavern idle life once M2 exists. No
possession timer (v2 change from v1 — party only changes via
`!kick`/`!newparty`). No campaign/season table or `!season` command (v2
change from v1 — cut entirely, not deferred; may come back as its own design
pass later). No automated redemption refunds (manual by design — "forces me
to address it"). No pong (unrelated cleanup, still pending, tracked
separately from this milestone). No automated encounter checks yet — `1d20 +
level` vs. challenge is a DM manual ruling until/unless `!check` gets built
(v3, §7a). Memory of this design lives in this file, not in chat.

## 9. TTS (post-MVP, unchanged from v1)

Possessed characters' messages get TTS with a voice tier tied to logins.
`tiktok_tts.py` already exists in the repo with a voice-name registry and
handles arbitrary-length text via chunking — implementing this is mostly
wiring, not building TTS from scratch. Implemented as another consumer on
`twitch_events` (same pattern as `background.py`), publishing audio locally
on the streaming PC — keeps the Pi build clean.

## 10. Milestones

- **M1 (schema + possession + plain party cards)** — **implemented, untested.**
  `characterStore` schema migration, `partyManager` (max 4, join/kick/newparty,
  no timer), "join the party" redemption, exp-on-message with 45s cooldown,
  finalized triangular level curve (§3), `!grant`/`!give`/`!smite`/`!bless`/
  `!kick`/`!newparty`/`!sheet`, `party.update` broadcast, party card rendering
  in the overlay.
- **M2 (tavern scene)**: real tavern backdrop art + geometry, ambient roster of
  present-but-not-possessed chatters (new state, distinct from `partyManager`),
  enter/chat/sleep/wander/possess/return animations consuming the already-sent
  `tavern.*` events, level-up spin, dude size scaling with level (§7).
- **M3 (billboard + pong removal)**: billboard styling for `other.update`,
  delete pong code paths.
- **M4 (open)**: unlock thresholds (10/30/60 logins), death/graveyard visual,
  whatever replaces the campaign/season concept if it comes back.
- **Stretch**: TTS voices, cosmetics shop + validation table, catch-up exp,
  courier animation, prestige classes, `!check <name> <dc>` encounter command
  (§7a).
