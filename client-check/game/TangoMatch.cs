using System;
using System.Collections.Generic;
using System.Linq;
using Newtonsoft.Json.Linq;

namespace UnoGame.Online
{
    /// <summary>One line of the server's scoreboard.</summary>
    public sealed class TangoResult
    {
        public int Seat, Rank, Points, Trophies, Level, Team;
        public string UserId = "", Name = "";
        public bool Winner, Removed;
        public long Coins;
    }

    /// <summary>
    /// The game as the server sends it, turned into the shapes the table already draws:
    /// <see cref="OnlineRoomSnapshot"/> / <see cref="MatchView"/> plus a running list of <see cref="MatchEvent"/>s for the animations.
    /// The server decides everything; this class only translates. It has no Unity dependency so it can be tested on its own.
    /// </summary>
    public sealed class TangoMatch
    {
        // Server → app op codes (see the backend's docs/CLIENT_PROTOCOL.md).
        public const long OpSnapshot = 100, OpLobby = 101, OpGameStart = 102, OpTurn = 103, OpCardPlayed = 104, OpCardsDrawn = 105,
            OpColorChosen = 106, OpTango = 107, OpHammer = 108, OpChallenge = 109, OpDrawnChoice = 110, OpPlayerStatus = 111,
            OpEmote = 112, OpGameOver = 113, OpRematch = 114, OpError = 115;
        // App → server.
        public const long SendPlay = 1, SendDraw = 2, SendKeepOrPlay = 3, SendTango = 4, SendHammer = 5, SendChallenge = 6,
            SendEmote = 7, SendRematch = 8, SendSnapshot = 9, SendForfeit = 10, SendChooseColor = 11, SendStart = 12;
        const int EventLimit = 96;

        public string MatchId { get; private set; } = "";
        public JObject State { get; private set; }
        public int Revision { get; private set; } = -1;
        public long ServerTimeMs { get; private set; }
        public int WinnerSeat { get; private set; } = -1;
        public List<TangoResult> Results { get; private set; } = new List<TangoResult>();
        public bool RematchAllowed { get; private set; }
        public int RematchVotes { get; private set; }
        public int RematchNeeded { get; private set; }
        /// <summary>The last refusal from the server (e.g. NOT_YOUR_TURN), as the player should read it; cleared by <see cref="TakeError"/>.</summary>
        public string Error { get; private set; } = "";
        public string Status => (string)State?["status"] ?? "";
        public int MySeat => (int?)State?["you"] ?? -1;

        readonly List<MatchEvent> events = new List<MatchEvent>();
        readonly Queue<RoomEmote> emotes = new Queue<RoomEmote>();
        int nextEventId = 1, turnId, round = -1;
        long nextEmoteId;
        int lastDir = 1, lastPlusSeat = -1, plusPendingSeat = -1, lastTurnSeat = -1;
        string lastTurnPhase = "";

        public TangoMatch(string matchId) { MatchId = matchId ?? ""; }

        public RoomEmote TakeEmote() => emotes.Count > 0 ? emotes.Dequeue() : null;
        public string TakeError() { string e = Error; Error = ""; return e; }

        /// <summary>Applies one message. Returns true when the table changed.</summary>
        public bool Apply(long op, string json)
        {
            JObject m;
            try { m = JObject.Parse(string.IsNullOrEmpty(json) ? "{}" : json); } catch (Exception) { return false; }
            long now = (long?)m["server_time_ms"] ?? 0;
            if (now > 0) ServerTimeMs = now;
            string type = (string)m["type"] ?? "";
            int seat = (int?)m["seat"] ?? -1;
            bool changed = false;
            switch (op)
            {
                case OpGameStart:
                    events.Clear();
                    lastPlusSeat = plusPendingSeat = lastTurnSeat = -1; lastTurnPhase = ""; WinnerSeat = -1; Results = new List<TangoResult>();
                    Emit("shuffle", -1, -1, 108);
                    var startState = m["state"] as JObject ?? State;
                    int seats = (startState?["players"] as JArray)?.Count ?? 0;
                    for (int i = 0; i < seats; i++) Emit("deal", -1, i, 7);
                    changed = true;
                    break;
                case OpTurn:
                    if (type == "skipped") { Emit("skip", (int?)m["by"] ?? -1, seat, 1); changed = true; break; }
                    {
                        string phase = (string)m["phase"] ?? "turn";
                        int phaseSeat = (int?)m["phase_seat"] ?? seat;
                        int pending = (int?)m["pending"] ?? 0;
                        if (pending > 0 && plusPendingSeat >= 0 && plusPendingSeat != phaseSeat)
                            Emit("stack", plusPendingSeat, phaseSeat, pending, (string)m["color"]);
                        plusPendingSeat = -1;
                        if (phaseSeat != lastTurnSeat || phase != lastTurnPhase) { turnId++; lastTurnSeat = phaseSeat; lastTurnPhase = phase; }
                        if (phase == "turn") Emit("turn", phaseSeat, -1, 0);
                        lastDir = (int?)m["dir"] ?? lastDir;
                    }
                    changed = true;
                    break;
                case OpCardPlayed:
                    {
                        var card = ToCard(m["card"] as JObject);
                        if (card != null)
                        {
                            Emit("play", seat, -1, 1, card.Color, card);
                            if (card.Kind == "reverse") Emit("reverse", seat, -1, -lastDir);
                            if (card.Kind == "draw2" || card.Kind == "draw4" || card.Kind == "draw6" || card.Kind == "draw10") { plusPendingSeat = seat; lastPlusSeat = seat; }
                        }
                    }
                    changed = true;
                    break;
                case OpCardsDrawn:
                    if (type == "reshuffle") Emit("shuffle", -1, -1, (int?)m["draw_count"] ?? 0);
                    else if (seat >= 0) Emit("draw", seat, seat, (int?)m["count"] ?? 1);
                    changed = true;
                    break;
                case OpColorChosen:
                    Emit("color", seat, -1, 0, (string)m["color"]);
                    changed = true;
                    break;
                case OpTango:
                    Emit("lastCard", seat, -1, 0);
                    changed = true;
                    break;
                case OpHammer:
                    if ((bool?)m["hit"] == true) Emit("catch", seat, (int?)m["target"] ?? -1, (int?)m["count"] ?? 2);
                    changed = true;
                    break;
                case OpChallenge:
                    Emit((bool?)m["success"] == true ? "challengeWon" : "challengeLost", seat, (int?)m["target"] ?? -1,
                        (int?)m["count"] ?? 0, (string)m["color_before"]);
                    changed = true;
                    break;
                case OpDrawnChoice:
                    changed = true;
                    break;
                case OpPlayerStatus:
                    if (State?["players"] is JArray list && seat >= 0 && seat < list.Count && list[seat] is JObject p)
                    {
                        if (type == "player_connected") p["connected"] = true;
                        else if (type == "player_disconnected") p["connected"] = false;
                        else if (type == "player_removed") { p["removed"] = true; Emit("eliminate", seat, seat, (int?)p["hand_count"] ?? 0); }
                    }
                    changed = true;
                    break;
                case OpEmote:
                    {
                        long local = DateTimeOffset.UtcNow.ToUnixTimeMilliseconds();
                        string user = "";
                        if (State?["players"] is JArray ps && seat >= 0 && seat < ps.Count) user = (string)ps[seat]?["user_id"] ?? "";
                        emotes.Enqueue(new RoomEmote { Id = ++nextEmoteId, Seat = seat, PlayerId = user, Emoji = (string)m["id"] ?? "",
                            SentUnixMs = local, ExpiresUnixMs = local + EmoteRules.LifetimeMs });
                    }
                    break;
                case OpGameOver:
                    WinnerSeat = (int?)m["winner_seat"] ?? -1;
                    RematchAllowed = (bool?)m["rematch_allowed"] ?? false;
                    Results = ParseResults(m["results"] as JArray);
                    Emit("roundWin", WinnerSeat, -1, Results.Where(r => !r.Winner).Sum(r => r.Points));
                    changed = true;
                    break;
                case OpRematch:
                    RematchVotes = (m["votes"] as JArray)?.Count ?? 0;
                    RematchNeeded = (int?)m["needed"] ?? 0;
                    changed = true;
                    break;
                case OpError:
                    Error = Friendly((string)m["code"]);
                    changed = true;
                    break;
                case OpSnapshot:
                case OpLobby:
                    changed = true;
                    break;
            }
            if (m["state"] is JObject state)
            {
                int rev = (int?)state["rev"] ?? 0;
                int stateRound = (int?)state["round"] ?? 0;
                // A rematch starts a new round with fresh revisions.
                if (rev >= Revision || stateRound != round || op == OpSnapshot)
                {
                    if (stateRound != round) { round = stateRound; if (op != OpGameStart) events.Clear(); }
                    State = state; Revision = rev;
                    MatchId = (string)state["match_id"] ?? MatchId;
                    lastDir = (int?)state["game"]?["dir"] ?? lastDir;
                    if (string.Equals((string)state["status"], "OVER", StringComparison.OrdinalIgnoreCase) && Results.Count == 0)
                        Results = ParseResults(state["results"] as JArray);
                    changed = true;
                }
            }
            return changed;
        }

        // ------------------------------------------------------------------ views
        public bool Started => Status == "PLAYING" || Status == "OVER";
        public bool IsTeams => IsTeamFormat((string)State?["format"]);
        public static bool IsTeamFormat(string format) => format == "2v2" || format == "4v4";
        public static string PresetOf(string mode, bool teams) => mode == "classic" ? (teams ? "team" : "classic") : "house";

        public OnlineRoomSnapshot Snapshot()
        {
            var snap = new OnlineRoomSnapshot();
            if (State == null) return snap;
            var players = State["players"] as JArray ?? new JArray();
            int me = MySeat;
            bool teams = IsTeams;
            snap.RoomCode = (string)State["room_code"] ?? "";
            snap.Capacity = (int?)State["max_players"] ?? players.Count;
            snap.IsHost = me >= 0 && me < players.Count && ((bool?)players[me]?["is_host"] ?? false);
            snap.Started = Started;
            snap.Teams = teams;
            snap.Preset = PresetOf((string)State["mode"], teams);
            snap.ServerTimeUnixMs = ServerTimeMs;
            snap.TurnDeadlineUnixMs = Started && Status == "PLAYING" ? (long?)State["game"]?["deadline_ms"] ?? 0 : 0;
            snap.TurnSeconds = (int?)State["settings"]?["turn_seconds"] ?? 15;
            foreach (var p in players.OfType<JObject>()) snap.Players.Add(PlayerOf(p));
            snap.View = Started ? View() : null;
            return snap;
        }

        PlayerView PlayerOf(JObject p)
        {
            int seat = (int?)p["seat"] ?? 0;
            var result = Results.FirstOrDefault(r => r.Seat == seat);
            return new PlayerView
            {
                Id = (string)p["user_id"] ?? "", Name = (string)p["name"] ?? "Player", IsBot = false, Difficulty = "",
                Team = (int?)p["team"] ?? seat, Count = (int?)p["hand_count"] ?? 0, Score = result?.Points ?? 0,
                Away = !((bool?)p["connected"] ?? true) || ((bool?)p["removed"] ?? false),
                UnoDeclared = (bool?)p["tango"] ?? false, Eliminated = (bool?)p["removed"] ?? false, Trophies = (int?)p["trophies"] ?? 0,
                Look = "a=" + Strip((string)p["avatar"], "avatar_") + ";f=" + Strip((string)p["frame"], "frame_") +
                       ";b=" + Strip((string)p["card_back"], "back_") + ";g=" + Strip((string)p["badge"], "badge_")
            };
        }

        public MatchView View()
        {
            if (State == null) return null;
            var players = State["players"] as JArray ?? new JArray();
            var g = State["game"] as JObject ?? new JObject();
            int me = MySeat;
            bool over = Status == "OVER" || ((bool?)g["over"] ?? false);
            string phase = (string)g["phase"] ?? "turn";
            int phaseSeat = (int?)g["phase_seat"] ?? (int?)g["turn"] ?? 0;
            bool mine = !over && me >= 0 && phaseSeat == me;
            JObject self = me >= 0 && me < players.Count ? players[me] as JObject : null;
            var hand = Cards(self?["hand"] as JArray);
            bool vulnerable = (bool?)self?["vulnerable"] ?? false, tango = (bool?)self?["tango"] ?? false, removed = (bool?)self?["removed"] ?? false;
            bool teams = IsTeams;
            string format = (string)State["format"] ?? "1v1";
            var v = new MatchView
            {
                MatchId = MatchId + ":" + ((int?)State["round"] ?? 1), RoomCode = (string)State["room_code"] ?? "",
                Mode = teams ? "duo" : players.Count == 2 ? "duel" : players.Count == 4 ? "solo4" : "room",
                Preset = PresetOf((string)State["mode"], teams),
                Phase = over ? "matchOver" : phase == "keep" ? "drawnWild" : phase == "color" ? "chooseColor" : "playing",
                ActiveColor = (string)g["color"] ?? "", Message = "", Revision = Revision, CurrentSeat = phaseSeat, TurnId = turnId,
                LocalSeat = Math.Max(0, me), Direction = (int?)g["dir"] ?? 1, WinnerSeat = WinnerSeat,
                RoundPoints = Results.Where(r => !r.Winner).Sum(r => r.Points), TargetScore = 0,
                PendingDraw = (int?)g["pending"] ?? 0, TopCard = ToCard(g["top"] as JObject), Hand = hand,
                CanDraw = mine && phase == "turn" && !removed,
                CanPass = mine && phase == "keep",
                CanChooseColor = mine && (phase == "keep" || phase == "color"),
                CanChallenge = mine && ((bool?)g["can_challenge"] ?? false),
                EndsAtUnixMs = 0, TimeUp = false
            };
            v.ChallengeSeat = v.CanChallenge ? lastPlusSeat : -1;
            v.CanCallUno = !over && !tango && !removed && ((mine && phase == "turn" && hand.Count == 2) || (hand.Count == 1 && vulnerable));
            if (mine && phase == "turn" && g["playable"] is JArray playable)
                foreach (var id in playable) v.PlayableCardIds.Add(((int)id).ToString());
            long localNow = DateTimeOffset.UtcNow.ToUnixTimeMilliseconds();
            for (int i = 0; i < players.Count; i++)
            {
                var p = players[i] as JObject;
                if (p == null) continue;
                v.Players.Add(PlayerOf(p));
                bool theirRemoved = (bool?)p["removed"] ?? false;
                if (!over && i != me && !removed && !theirRemoved && ((bool?)p["vulnerable"] ?? false))
                {
                    v.CatchTargets.Add(i);
                    v.LastCardCalls.Add(new LastCardCall { Id = i + 1, Seat = i, ExpiresAtUnixMs = localNow + 8000 });
                }
                if (teams && i != me && p["hand"] is JArray allyCards && !theirRemoved)
                    v.AllyHands.Add(new AllyHand { Seat = i, Cards = Cards(allyCards) });
            }
            v.CanCatchUno = v.CatchTargets.Count > 0;
            foreach (var e in events) v.Events.Add(e.Copy());
            return v;
        }

        /// <summary>The server message for one of the table's actions; null when there is nothing to send.</summary>
        public (long op, object payload)? Intent(GameAction action)
        {
            if (action == null) return null;
            var g = State?["game"] as JObject;
            string phase = (string)g?["phase"] ?? "turn";
            int drawnId = (int?)g?["drawn"]?["id"] ?? -1;
            switch ((action.Type ?? "").ToLowerInvariant())
            {
                case "play":
                    if (!int.TryParse(action.CardId ?? "", out int id)) return null;
                    if (phase == "keep" && id == drawnId)
                        return (SendKeepOrPlay, new Dictionary<string, object> { { "play", true }, { "color", action.Color ?? "" } });
                    var play = new Dictionary<string, object> { { "card_id", id } };
                    if (!string.IsNullOrEmpty(action.Color)) play["color"] = action.Color;
                    return (SendPlay, play);
                case "color":
                    if (phase == "keep") return (SendKeepOrPlay, new Dictionary<string, object> { { "play", true }, { "color", action.Color ?? "" } });
                    return (SendChooseColor, new Dictionary<string, object> { { "color", action.Color ?? "" } });
                case "pass": return (SendKeepOrPlay, new Dictionary<string, object> { { "play", false } });
                case "draw": return (SendDraw, null);
                case "uno": return (SendTango, null);
                case "catch": return (SendHammer, new Dictionary<string, object> { { "seat", action.TargetSeat } });
                case "challenge": return (SendChallenge, null);
                default: return null;
            }
        }

        // ------------------------------------------------------------------ helpers
        void Emit(string type, int actor, int target, int amount, string color = null, Card card = null)
        {
            events.Add(new MatchEvent { Id = nextEventId++, Type = type, ActorSeat = actor, TargetSeat = target, Amount = amount, Color = color, Card = card });
            if (events.Count > EventLimit) events.RemoveRange(0, events.Count - EventLimit);
        }

        static List<TangoResult> ParseResults(JArray list)
        {
            var results = new List<TangoResult>();
            if (list == null) return results;
            foreach (var r in list.OfType<JObject>())
                results.Add(new TangoResult
                {
                    Seat = (int?)r["seat"] ?? -1, Rank = (int?)r["rank"] ?? 0, Points = (int?)r["points"] ?? 0, Trophies = (int?)r["trophies"] ?? 0,
                    Level = (int?)r["level"] ?? 1, Team = (int?)r["team"] ?? 0, UserId = (string)r["user_id"] ?? "", Name = (string)r["name"] ?? "",
                    Winner = (bool?)r["winner"] ?? false, Removed = (bool?)r["removed"] ?? false, Coins = (long?)r["coins"] ?? 0
                });
            return results;
        }

        static List<Card> Cards(JArray list) => list == null ? new List<Card>() : list.OfType<JObject>().Select(ToCard).Where(c => c != null).ToList();

        /// <summary>Server card → the table's card (black plus cards are "draw4/6/10" with colour "wild").</summary>
        public static Card ToCard(JObject c)
        {
            if (c == null || c["id"] == null) return null;
            string kind = (string)c["kind"] ?? "number";
            switch (kind)
            {
                case "wild4": kind = "draw4"; break;
                case "wild6": kind = "draw6"; break;
                case "wild10": kind = "draw10"; break;
            }
            return new Card { Id = ((int)c["id"]).ToString(), Color = (string)c["color"] ?? "wild", Kind = kind, Number = (int?)c["value"] ?? 0 };
        }

        static string Strip(string id, string prefix) => string.IsNullOrEmpty(id) ? "" : id.StartsWith(prefix) ? id.Substring(prefix.Length) : id;

        public static string Friendly(string code)
        {
            switch (code ?? "")
            {
                case "NOT_YOUR_TURN": return "Please wait for your turn.";
                case "ILLEGAL_CARD": return "That card does not match the pile.";
                case "MUST_STACK_OR_DRAW": return "Answer the + card with another one, or take the cards.";
                case "CANNOT_CHALLENGE": return "There is nothing to challenge right now.";
                case "TOO_MANY_CARDS": return "TANGO! works when you have two cards or fewer.";
                case "BAD_COLOR": return "Choose red, yellow, green or blue.";
                case "EMOTES_OFF": return "Emotes are switched off at this table.";
                case "GAME_OVER": return "The match has finished.";
                case "NOT_IN_GAME": return "You are no longer in this match.";
                case "": return "";
                default: return code.Replace('_', ' ').ToLowerInvariant();
            }
        }
    }
}
