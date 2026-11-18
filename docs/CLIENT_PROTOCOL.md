# Tango Cards: client ↔ server protocol

The server is authoritative: the Unity app sends **intents** ("play card 42",
"draw", "press TANGO!") and renders the **state** the server sends back. Every
rule (legal cards, stacking, challenges, the hammer, payouts, bots, timers) is
decided on the server; nothing in the app needs to know the rules to stay
correct.

The server speaks the Nakama protocol, so the app uses the official
**Nakama Unity SDK** (`com.heroiclabs.nakama-unity`) for sign-in, RPCs and the
realtime socket.

## Connection

| Setting | Value |
|---|---|
| Scheme | `https` |
| Host | `tango-backend.sssss.co.in` (nonprod) |
| Port | `9443` (HTTPS) |
| Server key | shown in the pipeline summary (`tango-…`) |

The Unity game reads these from `Assets/Resources/TangoServer.json`.

```csharp
var client  = new Nakama.Client("https", host, 443, serverKey);
var session = await client.AuthenticateDeviceAsync(SystemInfo.deviceUniqueIdentifier);
var socket  = client.NewSocket();
await socket.ConnectAsync(session, appearOnline: true);
socket.ReceivedMatchState += OnMatchState;   // all game messages arrive here
```

Google sign-in (an ID token from Credential Manager, issued to the game's OAuth
Web client ID):

* `POST /v2/account/link/google` `{"token": idToken}` (Bearer session): adds
  Google to the signed-in guest account, so its coins and items are kept.
  `409 AlreadyExists` means that Google account is another player's: then
* `POST /v2/account/authenticate/google?create=true` `{"token": idToken}`
  (Basic server key) opens that account instead (a new phone, a reinstall).

The server accepts tokens only for the client IDs in
`config/aws-targets.json` → `google_client_ids` (passed to the node config).

## RPCs

Call with `client.RpcAsync(session, id, jsonPayload)`. Every response is JSON
with `"success": true` or `"success": false, "error": "<CODE>"`.

| RPC id | Payload | Returns |
|---|---|---|
| `tango.profile.get` | `{}` | `profile` (name, avatar, frame, badge, card_back, table, trophies, wins, losses, games), `level`, `wallet {coins, diamonds}`, `player_id` (the permanent 8-character public ID, made with the account). Call first after sign-in: creates the profile, the player ID and the starting wallet (2,000 coins, 20 diamonds) once. |
| `tango.profile.update` | `{"name":"Robin"}` and/or `{"tutorial_done":true}` | `profile`, `wallet`, `charged`, `name_change_cost`. Names 3–16 letters/digits/space/`_.-`. The first chosen name is free; every later change costs `name_change_cost` coins (default 1,000; `INSUFFICIENT_COINS` when short). `profile.name_changes` counts the names chosen. |
| `tango.wallet.get` | `{}` | `wallet` |
| `tango.shop.catalog` | `{}` | `items[] {id, category, name, price{coins\|diamonds}, rarity, owned}`, `iap_products[]` |
| `tango.shop.purchase` | `{"item_id":"frame_gold"}` | `wallet`. Errors: `INSUFFICIENT_FUNDS`, `ALREADY_OWNED`, `UNKNOWN_ITEM` |
| `tango.inventory.get` | `{}` | `owned[]`, `equipped {avatar, frame, badge, card_back, table}` |
| `tango.inventory.equip` | `{"category":"frame","item_id":"frame_sakura"}` | `profile`. Categories: avatar, frame, badge, card_back, table, effect. Item ids are `<kind>_<item>` as in the game's shop: `avatar_kai`, `frame_sakura`, `badge_aurora`, `back_frost`, `table_moonlit`, `emote_party`, `effect_flame` |
| `tango.ads.claim_reward` | `{}` after a completed rewarded ad | `coins` (250–300), `remaining_today`, `wallet`. Limits: 10/day, 30 s apart |
| `tango.iap.verify_google` | `{"receipt":"<purchase JSON from Play Billing>"}` | `credited`, `wallet` (enable with `GOOGLE_IAP_ENABLED`) |
| `tango.dev.grant` | `{}` | +10,000 coins, +1,000 diamonds. **Non-prod only** (the tap-coins-three-times shortcut) |
| `tango.room.find_opponents` | `{"match_id":"…","fee":200}` | Party leader only: the friends room (still waiting) becomes the public waiting table for its mode/format at that fee. Strangers who quick-join that table sit down; the party keeps its teams; bots fill the rest 15 s later. Refused with `INSUFFICIENT_COINS` (`message` names who can't pay) or `NOT_ALLOWED` |
| `tango.lobby.stats` | `{"mode":"classic"}` | `online`, `in_game` (players seen in the last 3 min) and `waiting`: `{"2v2_400": 1, …}` players waiting at each public table of the mode |
| `tango.match.quick_join` | `{"mode":"classic"\|"no_mercy","format":"1v1"\|"2v2"\|"4v4"\|"4p"\|"8p","fee":200}` | `match_id` → join it on the socket. Fees: 200, 400, 1000, 2000, 5000. `4p`/`8p`: everyone for themselves |
| `tango.room.create` | `{"mode","format","fee":0,"settings":{"turn_seconds":15,"emotes":true,"spectators":false,"mic":false,"jump_in":false,"seven_o":false}}` | `room_code` (6 digits), `match_id`. House rules (private rooms only): `jump_in` lets a player who is not on turn play a card identical to the top one (same colour and number/symbol, no black cards, not into a stack; play continues from them, effect `jump_in`); `seven_o` turns on 7 swap / 0 pass in Classic (No Mercy always has it). The game view carries `game.jump_in` and `game.seven_o`; out of turn, `game.playable` lists the jump-in cards. |
| `tango.room.join` | `{"room_code":"123456"}` | `match_id`. Errors: `ROOM_NOT_FOUND`, `ROOM_STARTED`, `ROOM_FULL`. Join the match with metadata `{"team_with":"<inviter user_id>"}` after accepting an invite to sit on the inviter's team (2v2/4v4) |
| `tango.match.active` | `{}` | `match_id` of a game still running (rejoin after a crash), or `null` |
| `tango.match.forfeit` | `{}` | leaves the running game for good |
| `tango.friends.list` | `{}` | friends (name, avatar, frame, trophies, level, status online/in_game/offline, last_seen), requests, your 8-character **friend code** |
| `tango.friends.find` | `{"query":"AB12CD34"}` | `player` (name, avatar, trophies, status) without sending anything; `not_found`, `self` |
| `tango.friends.add` | `{"query":"AB12CD34"}` | sends a request (friend code or username) |
| `tango.friends.action` | `{"user_id":"…","action":"accept"\|"decline"\|"cancel"\|"remove"\|"block"\|"unblock"}` | |
| `tango.friends.invite` | `{"user_id":"…","room_code":"123456","mode":"classic","format":"2v2"}` | invite shows in the friend's invites for 15 minutes; friends only (`not_friends`) |
| `tango.friends.invites` | `{}` | `invites[] {from{user_id, display_name, avatar, trophies}, room_code, mode, format, sent_at, expires_at}`; the game polls this every few seconds off the table |
| `tango.friends.invite_reply` | `{"user_id":"<inviter>"}` | removes the invite (joined or declined) |
| `tango.leaderboard.get` | `{"id":"trophies"\|"weekly_trophies"\|"wins"\|"friends","limit":50}` | `records[] {rank, user_id, name, avatar, frame, level, score}`, `me {rank, score}`. `weekly_trophies` starts again every Monday; `friends` ranks you and your friends by trophies |
| `tango.spin.status` | `{}` | `prizes[] {coins, diamonds}` (the wheel's slices in order), `free_left`, `ad_left`, `ad_wait_ms`, `next_reset_ms` |
| `tango.spin.play` | `{}` (free spin) or `{"ad":true}` (after a rewarded ad) | `slice` the wheel stops on, `coins`, `diamonds`, `wallet`, plus the status fields. 1 free spin a day, 3 ad spins 30 s apart; the day starts at midnight India time. Errors: `NO_FREE_SPIN`, `DAILY_LIMIT`, `TOO_SOON` |
| `tango.app.config` | `{}` | entry fees, ad limits, turn-time options, maintenance message |
| `tango.account.delete` | `{"confirm":true}` | deletes the account (Play Store requirement) |

## Playing a match

1. `tango.match.quick_join` (or `tango.room.create` / `tango.room.join`) → `match_id`
2. `await socket.JoinMatchAsync(matchId)`
3. Receive `lobby` messages; public matches fill empty seats after 15 s;
   private rooms start when full or when the host sends `START`.
4. `game_start`, then game events, each carrying `state` (see below).
5. `game_over` with the scoreboard. Public: back to lobby. Private: `REMATCH`.

Send with `socket.SendMatchStateAsync(matchId, opCode, json)`.

### App → server

| Op | Name | Payload |
|---|---|---|
| 1 | PLAY | `{"card_id":42,"color":"red","tango":true}` — `color` for black cards (or send CHOOSE_COLOR next); `tango:true` presses TANGO! in the same message when going down to one card |
| 2 | DRAW | `{}` — draws one card, or takes the whole pending stack |
| 3 | KEEP_OR_PLAY | `{"play":true,"color":"blue"}` — answer for a drawn black plus |
| 4 | TANGO | `{}` — press TANGO! (allowed at two cards or fewer) |
| 5 | HAMMER | `{"seat":2}` — tap the hammer on a player who forgot TANGO! (a miss costs nothing) |
| 6 | CHALLENGE | `{}` — challenge the first black plus of a stack |
| 7 | EMOTE | `{"id":"emote_laugh"}` |
| 8 | REMATCH | `{}` — private rooms after the game |
| 9 | SNAPSHOT | `{}` — ask for the full state again |
| 10 | FORFEIT | `{}` |
| 11 | CHOOSE_COLOR | `{"color":"green"}` |
| 12 | START | `{}` — private room host: start now (empty seats get bots) |
| 13 | SWAP | `{"seat":2}` — No Mercy 7: whose hand to swap with (phase `swap`) |

### Server → app

Every message is JSON with `type` and usually `seat`. The **last message of
each batch carries `state`**: replace your whole view with it; use the events
before it for animations.

| Op | type | Fields |
|---|---|---|
| 100 | `snapshot` | `state` |
| 101 | `lobby` | `state` (players so far, `fill_at_ms`, `start_at_ms`) |
| 102 | `game_start` | `state` |
| 103 | `turn` / `skipped` | `phase`, `phase_seat`, `deadline_ms`, `pending`, `color`, `dir`, `can_challenge` |
| 104 | `card_played` | `card`, `auto` (drawn card played by itself), `hand_count`, `vulnerable` |
| 105 | `cards_drawn` | `count`, `reason` (`draw`, `stack`, `hammer`, `challenge`), `cards` (**only for you and your teammates**) / `reshuffle` |
| 106 | `color_chosen` | `color` |
| 107 | `tango` | seat pressed TANGO! (play the voice) |
| 108 | `hammer` | `target`, `hit`, `count` |
| 109 | `challenge` | `target`, `success`, `drawer`, `count` |
| 110 | `drawn_choice` | `cards` (the drawn black plus), `deadline_ms` — show Keep / Play |
| 111 | `player_connected` / `player_disconnected` / `player_removed` | |
| 112 | `emote` | `seat`, `id` |
| 113 | `game_over` | `results[] {seat, rank, name, points, winner, coins, trophies, level}`, `winner_team`, `rematch_allowed` |
| 114 | `rematch` | `votes[]`, `needed` |
| 115 | `error` | `code` (e.g. `NOT_YOUR_TURN`, `ILLEGAL_CARD`, `MUST_STACK_OR_DRAW`, `CANNOT_CHALLENGE`, `TOO_MANY_CARDS`) |
| 116 | `skip_all` / `discard_all` / `rotate` / `swap` / `time_up` | No Mercy effects (`skipped`; `count`, `color`; `dir`; `target`) and the match clock ending (`seat` = winner, `points`) |

All times are server milliseconds; every message has `server_time_ms` to
correct the device clock for countdowns.

### `state`

```json
{
  "match_id": "…", "mode": "no_mercy", "format": "2v2", "fee": 400, "private": false,
  "status": "PLAYING", "round": 1, "you": 0, "max_players": 4, "rev": 57,
  "settings": {"turn_seconds": 15, "emotes": true, "spectators": false, "mic": false},
  "players": [
    {"seat": 0, "user_id": "…", "name": "Robin", "avatar": "avatar_01", "frame": "frame_gold",
     "badge": "badge_basic", "card_back": "back_classic", "trophies": 340, "level": 4, "team": 0,
     "connected": true, "removed": false, "hand_count": 5, "tango": false, "vulnerable": false,
     "hand": [{"id": 12, "color": "red", "kind": "number", "value": 7}]}
  ],
  "game": {
    "top": {"id": 88, "color": "wild", "kind": "wild4"}, "color": "green", "turn": 1, "dir": 1,
    "pending": 4, "phase": "turn", "phase_seat": 1, "deadline_ms": 1763280000000,
    "draw_count": 61, "discard_count": 14, "recent": [], "playable": [12, 19], "can_challenge": false
  }
}
```

* `hand` is present only for you and, in 2v2/4v4, your teammates.
* `playable` lists the card ids you may play right now: make those glow.
* Card kinds: `number` (with `value`), `skip`, `reverse`, `draw2`, `wild`,
  `wild4` (Classic); No Mercy uses coloured `draw4`, `skip_all`,
  `discard_all` and the black `wild_rev4` (Wild Reverse +4), `wild6`, `wild10`
  and `roulette` (Wild Color Roulette). Black cards have `color: "wild"`; the
  colour in play is `game.color`.
* No Mercy follows the official rules with the 168-card deck: a player who
  can't play draws until a playable card turns up (it is played for them; a
  black draw card offers keep or play); stacking takes any draw card of equal
  or higher value, any colour; there is no challenge; Wild Reverse +4 turns
  play round (`effect` `reverse`) and the next player in the new direction
  draws 4; Color Roulette (`effect` `roulette`, `target`) puts the next player
  in phase `roulette`: they send CHOOSE_COLOR and turn cards until that colour
  shows (`cards_drawn` `reason: "roulette"`), then lose the turn. 0 passes
  every hand in the direction of play, 7 swaps hands with a chosen player, and
  a player who reaches 25 cards is out (`player_removed` with
  `reason: "mercy"`); the last player or team standing wins. A player out on
  the 25-card rule may reconnect and watch the end.
* Classic keeps its house rules: a black plus answers any stack, a coloured
  +2 only in the colour in play, and the first black plus of a stack can be
  challenged (`game.challenge_color` is the colour it was played over).
* Match clock: `game.ends_at_ms` (Classic 3 min, No Mercy 5 min, or the room's
  `settings.match_minutes` 3/5/10). When it runs out the fewest points win
  (`game.over_reason: "time_up"`). Turns are 10 s by default; three missed
  turns in a row remove a player. Everyone who finishes a match earns 3
  diamonds (`results[].diamonds`).
* Phases: `turn` (play/draw, or stack/draw/challenge when `pending > 0`),
  `color` (choose a colour), `keep` (keep or play the drawn black plus),
  `swap` (No Mercy 7: pick a seat from `game.swap_targets`), `roulette`
  (No Mercy: the player in `phase_seat` names a colour), `over`.
* Teams sit alternately (seats 0/2 vs 1/3 in 2v2).
* `rev` only grows; ignore any state with a lower `rev` than you have.

## Reconnecting

The socket can drop (network change, server deploy). Reconnect, then call
`tango.match.active`; if it returns a `match_id`, join it again and you get a
`snapshot` with your seat and cards. A disconnected player's turns are played
automatically with a short timer; after three missed turns they are out of
the game and lose the full entry fee.
