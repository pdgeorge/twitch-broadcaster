# Features at a glance

A one-page summary of what this stack does on stream, what the overlay is made of, and what's known to be missing. `README.md` has the details, and `PLANS.md` has the ideas that aren't built yet.

## On stream

- **Chat overlay**: Twitch chat in the top left, styled in `overlay/overlay.css`. Twitch, BTTV, FFZ and 7TV emotes all render. When a mod deletes a message, times out or bans someone, or clears chat, those lines come off the overlay too, along with any party TTS of them that hasn't finished.
- **Dablings**: anyone who chats gets a Dabling on the tavern floor at the bottom of the screen. It wanders around until its chatter has been quiet for 30 minutes. Each chatter gets their own tint, and Dablings grow with level and wear their owner's cosmetics.
- **Cosmetics**: chatters own items and wear one per slot (`!wardrobe`, `!equip`, `!unequip`). Mods hand them out and take them back with `!give` and `!take`. Add and edit them on `/admin`.
- **Sound commands**: `!buhee` and friends, each with its own cooldown shared by the whole chat. `!sounds` lists them. Managed on `/admin`.
- **Login redeems**: "daily login bonus" and "first login bonus" each keep a count per chatter, and the bot replies with the new total. A chatter's first "first login bonus" also gives them the `bronze_first` shirt, the only automatic reward so far.
- **Custom text commands**: mods add one with `#a !discord <reply>` and remove it with `#d !discord`. Anyone who types exactly `!discord` gets the reply.
- **Chat log**: every message is saved to MySQL (`chat_log`) permanently. Messages a mod deletes, or that a timeout or ban clears, are marked deleted but kept. This is the groundwork for Chat on Trial in `PLANS.md`.
- **`/admin`**: `http://dabi:8080/admin`, with Sounds, Cosmetics, Shop and Party guide tabs.

## The party game

The Party guide tab on `/admin` covers all of this, with the numbers taken from the controller.

- **Joining**: the "join the party" channel-point redeem, 4 seats. Party members get a card at the bottom right, and what they type in chat is read aloud in TikTok voices. Each chatter keeps the same voice, and more voices unlock at 10 and 30 daily logins.
- **Exp and levels**: chatting earns exp, at most once every 45 seconds (5 + √logins per message). Levelling up raises max HP, refills HP and makes the Dabling bigger.
- **Death**: at 0 HP a character dies, leaving the party and the floor until `!revive`, which costs a level by default.
- **`!roll`** (anyone): a d20 plus their level, with fanfare and confetti on a nat 20 and a sad trombone on a nat 1.
- **DM commands** (mods and the broadcaster):
  - `!grant <name> exp|hp <n>`, `!smite <name> <n>`, `!bless <name> <n>`
  - `!revive <name> [levels]`
  - `!give <name> <gold|cosmetic>`, `!take <name> <cosmetic>`
  - `!kick <name>`, `!newparty` (empties the party)
  - `!sheet <name>`: shows that character's level, HP, exp and gold in the billboard box for 30 seconds. This is separate from the party cards.
- **Gold** exists, but there's nothing to spend it on yet.

## The billboard (dormant)

The white text box in the middle of the tavern floor. It's invisible when empty, and nothing fills it on its own any more: Pong is gone, and the channel has no "announcement" redeem. The code is kept in case it comes back:

- `!other <text>` (mods) sets its text. Markdown works.
- An "announcement" redeem, if one is added back, shows the viewer's text for 5 minutes. `!fire` ends it early and puts the `!other` text back.
- `!sheet` (above) also draws here.

## Dabi's shop (from 1 December)

A market stall where chatters buy cosmetics with channel points. It isn't on stream yet: it opens with Dabi.

- **Built**: `/shop` (`http://dabi:8080/shop/`), the 1920×1080 browser source for the Dabi-Shop scene. It draws the stall with a Dabling in each of its three boxes, wearing what that box sells, and the prices, and redraws on `shop.update`. The Shop tab on `/admin` sets what's in each box and its price, 300 channel points to start. The stall art is a Blender render made by `tools/shop_stall.py`.
- **Buying, built**: `!openshop` / `!closeshop` (the broadcaster only) open and close it and tell chat. While it's open, chat buys with the "Buy item 1/2/3" rewards, which the bot makes itself and switches on only for boxes with something in them. A purchase goes into the buyer's wardrobe, unworn. Doubles are fine (`!wardrobe` shows `shirtA ×2`), and each reward allows a chatter 3 a stream. The points go back if the box is empty, the shop is closed or the price just changed. Needs the `channel:manage:redemptions` scope on the Twitch token.
- **Still to come**: `!openshop` / `!closeshop` also switching to the Dabi-Shop scene and its music. Dabi thanking buyers, with a switch to turn that off.

## Next door: Dabi

DabiReborn listens to the same Twitch events from its own repo. Dabi answers the "Ask Dabi a question" redeem and can reply to chat in batches, which `!dabichat` turns on and off. `!dabireset` wipes his memory, `!dabi x, y` moves his avatar, and he resets and greets chat when the stream goes live. He's off stream until the 1 December redebut.

## Overlay pieces

For rebuilding the overlay: what's on screen, where it sits on the 1920×1080 canvas (all set in `overlay/overlay.css`), and which `/ws/overlay` message drives it. A rebuilt overlay needs to handle these messages.

| Piece | Where | Message |
|---|---|---|
| Chat (`#chat-box`) | top left (left 20, top 20), 330 px wide | `chat.message`, and `chat.delete`, `chat.clear_user`, `chat.clear` to take lines off |
| Tavern floor (`#tavern-area`) | bottom strip, from 25% across to the right edge, 260 px tall | `tavern.roster` |
| Party cards (`#party-cards`) | bottom right, above the floor (right 20, bottom 172), growing leftward | `party.update` |
| Billboard (`#other-box`) | middle of the floor (left 727, top 918, 1160×148) | `other.update` |
| Dice (`#roll-box`) | centred, about a third of the way down | `roll.result` |
| Party voices | audio only. Party members talk over each other, but each one's own lines play in turn. Dropped if a mod removes the message | `tts.play` |
| Sound commands | audio only, played straight away, can overlap | `sound.play` |

The controller also sends `tavern.possess`, `tavern.return` and `tavern.death` when someone joins the party, leaves it or dies, but nothing draws them yet.

## Known issues

- **A freshly loaded overlay starts blank.** The controller doesn't send the current state to a page when it connects. After a browser-source refresh, the party cards stay empty until a party member chats or the party changes, and the billboard stays empty until the next `!other`. The tavern floor refills within a minute.
- **The party is only kept in memory.** Every controller restart or deploy empties it, and clears the `!other` text too.
- **No automatic refunds.** "Join the party" redeems refused because the party is full or the character is dead have to be refunded by hand.
- **Custom text commands can't be listed**, except by looking in MySQL (`bot_commands`).
- **No alerts.** Follows, subs, raids and cheers all reach the event bus, but nothing reacts to them.

## Not built yet

- Walk-to-door and death animations, from the `tavern.*` messages above.
- Dablings falling asleep when their chatter goes quiet.
- A level-up spin.
- HP regenerating when the stream starts.
