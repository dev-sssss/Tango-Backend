using System;
using System.Collections.Generic;
using System.Linq;
using System.Threading;
using System.Threading.Tasks;
using Newtonsoft.Json.Linq;

namespace UnoGame.Online
{
    /// <summary>
    /// Everything the game asks of the Tango Cards server: sign-in, the player's profile and wallet, the shop,
    /// matchmaking, private rooms, playing, emotes and friends. All rules, coins and trophies are decided by the server.
    /// Call <see cref="Pump"/> once per frame on the main thread; it applies queued match messages and raises <see cref="Changed"/>.
    /// No Unity types here, so the same code is exercised by the console test harness.
    /// </summary>
    public sealed class TangoGameClient
    {
        public readonly TangoNet Net;
        readonly string deviceId;
        readonly Func<string, string> load;
        readonly Action<string, string> save;
        readonly object pumpLock = new object();
        Task initialization;
        volatile bool socketLost;

        public TangoMatch Match { get; private set; }
        public CosmeticProfile Profile { get; private set; }
        public string PlayerId => Net.Session?.UserId ?? "";
        public bool SignedIn => Net.Session != null && !string.IsNullOrEmpty(Net.Session.Token);
        public bool InMatch => Match != null;
        public string LastError { get; private set; } = "";
        /// <summary>The table changed (raised from <see cref="Pump"/>).</summary>
        public event Action Changed;
        /// <summary>A finished match: the coins and trophies are already in the wallet on the server.</summary>
        public event Action<TangoMatch> GameOver;

        const string TokenKey = "Tango.Session.Token", RefreshKey = "Tango.Session.Refresh", MatchKey = "Tango.Match";

        public TangoGameClient(TangoServerConfig config, string deviceId, Func<string, string> load, Action<string, string> save)
        {
            Net = new TangoNet(config);
            this.deviceId = deviceId;
            this.load = load ?? (_ => "");
            this.save = save ?? ((_, __) => { });
            Net.SocketClosed += _ => socketLost = true;
        }

        // ------------------------------------------------------------------ sign-in
        public Task Initialize()
        {
            if (initialization == null || initialization.IsFaulted || initialization.IsCanceled) initialization = SignIn();
            return initialization;
        }

        async Task SignIn()
        {
            string token = load(TokenKey), refresh = load(RefreshKey);
            bool restored = false;
            if (!string.IsNullOrEmpty(refresh))
            {
                Net.Restore(token, refresh);
                try { await Net.EnsureSession(); restored = true; }
                catch (TangoHttpException) { restored = false; }
            }
            if (!restored) await Net.AuthenticateDevice(deviceId);
            save(TokenKey, Net.Session.Token); save(RefreshKey, Net.Session.RefreshToken);
            await RefreshProfile();
            await Net.ConnectSocket();
            socketLost = false;
        }

        /// <summary>Reconnects a dropped socket and rejoins the match in progress. Safe to call often.</summary>
        public async Task<bool> EnsureConnected()
        {
            if (!SignedIn) return false;
            if (Net.SocketOpen && !socketLost) return true;
            await Net.EnsureSession();
            save(TokenKey, Net.Session.Token); save(RefreshKey, Net.Session.RefreshToken);
            await Net.ConnectSocket();
            socketLost = false;
            if (Match != null)
            {
                try { await Net.JoinMatch(Match.MatchId); await Net.SendMatch(Match.MatchId, TangoMatch.SendSnapshot); }
                catch (Exception) { await Resume(); }
            }
            return true;
        }

        // ------------------------------------------------------------------ profile, wallet, shop
        public async Task<CosmeticProfile> RefreshProfile()
        {
            var p = await Call("tango.profile.get");
            var inv = await Call("tango.inventory.get");
            var profile = p["profile"] as JObject ?? new JObject();
            var wallet = p["wallet"] as JObject ?? new JObject();
            var result = new CosmeticProfile
            {
                Coins = (int?)wallet["coins"] ?? 0, Diamonds = (int?)wallet["diamonds"] ?? 0,
                Name = (string)profile["name"] ?? "", Trophies = (int?)profile["trophies"] ?? 0, Level = (int?)p["level"] ?? 1,
                Wins = (int?)profile["wins"] ?? 0, Games = (int?)profile["games"] ?? 0, TutorialDone = (bool?)profile["tutorial_done"] ?? false,
                MatchRewardCoins = 0
            };
            foreach (var id in (inv["owned"] as JArray ?? new JArray()).Select(x => (string)x))
            {
                var key = ClientKey(id);
                if (key != null && !result.Owned.Contains(key)) result.Owned.Add(key);
            }
            if (inv["equipped"] is JObject eq)
                foreach (var pair in eq)
                {
                    var key = ClientKey((string)pair.Value);
                    if (key != null) result.Equipped[key.Substring(0, key.IndexOf(':'))] = key.Substring(key.IndexOf(':') + 1);
                }
            Profile = result;
            return result;
        }

        public async Task<CosmeticProfile> Purchase(string category, string item)
        {
            await Call("tango.shop.purchase", new JObject { ["item_id"] = ServerItemId(category, item) });
            return await RefreshProfile();
        }

        public async Task<CosmeticProfile> Equip(string category, string item)
        {
            await Call("tango.inventory.equip", new JObject { ["category"] = category == "back" ? "card_back" : category, ["item_id"] = ServerItemId(category, item) });
            return await RefreshProfile();
        }

        public async Task<CosmeticProfile> SetName(string name)
        {
            await Call("tango.profile.update", new JObject { ["name"] = name });
            return await RefreshProfile();
        }

        public Task MarkTutorialDone() => Call("tango.profile.update", new JObject { ["tutorial_done"] = true });

        /// <summary>Non-prod developer top-up (+10,000 coins, +1,000 diamonds).</summary>
        public async Task<CosmeticProfile> DevGrant() { await Call("tango.dev.grant"); return await RefreshProfile(); }

        /// <summary>Free coins after a rewarded break; returns the coins given.</summary>
        public async Task<int> ClaimAdReward()
        {
            var r = await Call("tango.ads.claim_reward");
            await RefreshProfile();
            return (int?)r["coins"] ?? 0;
        }

        public Task<JObject> AppConfig() => Call("tango.app.config");

        // ------------------------------------------------------------------ matchmaking and rooms
        /// <summary>Public table: mode "classic"/"no_mercy", format "1v1"/"2v2"/"4v4"/"4p"/"8p", entry fee.</summary>
        public async Task<OnlineRoomSnapshot> QuickMatch(string mode, string format, int fee)
        {
            var r = await Call("tango.match.quick_join", new JObject { ["mode"] = mode, ["format"] = format, ["fee"] = fee });
            return await JoinAndWait((string)r["match_id"]);
        }

        public async Task<OnlineRoomSnapshot> CreateRoom(string mode, string format, int turnSeconds, bool emotes = true)
        {
            var r = await Call("tango.room.create", new JObject
            {
                ["mode"] = mode, ["format"] = format, ["fee"] = 0,
                ["settings"] = new JObject { ["turn_seconds"] = turnSeconds, ["emotes"] = emotes, ["spectators"] = false, ["mic"] = false }
            });
            return await JoinAndWait((string)r["match_id"]);
        }

        public async Task<OnlineRoomSnapshot> JoinRoom(string code)
        {
            string digits = new string((code ?? "").Where(char.IsDigit).ToArray());
            var r = await Call("tango.room.join", new JObject { ["room_code"] = digits });
            return await JoinAndWait((string)r["match_id"]);
        }

        /// <summary>Rejoins a game that is still running for this player (after a crash or a lost connection).</summary>
        public async Task<OnlineRoomSnapshot> Resume()
        {
            var r = await Call("tango.match.active");
            string id = (string)r["match_id"];
            if (string.IsNullOrEmpty(id)) { ForgetMatch(); return null; }
            return await JoinAndWait(id);
        }

        async Task<OnlineRoomSnapshot> JoinAndWait(string matchId)
        {
            if (string.IsNullOrEmpty(matchId)) throw new InvalidOperationException("The server did not return a table.");
            await EnsureConnected();
            lock (pumpLock) Match = new TangoMatch(matchId);
            save(MatchKey, matchId);
            await Net.JoinMatch(matchId);
            var until = DateTime.UtcNow.AddSeconds(10);
            while (true)
            {
                Pump();
                if (Match?.State != null) return Match.Snapshot();
                if (DateTime.UtcNow > until) { await Net.SendMatch(matchId, TangoMatch.SendSnapshot); until = DateTime.UtcNow.AddSeconds(10); }
                await Task.Delay(40);
                if (Match == null) throw new InvalidOperationException("Left the table.");
            }
        }

        public OnlineRoomSnapshot Snapshot() => Match?.Snapshot();

        /// <summary>Sends one of the table's actions to the server.</summary>
        public Task Act(GameAction action)
        {
            var m = Match;
            if (m == null) return Task.CompletedTask;
            var intent = m.Intent(action);
            return intent == null ? Task.CompletedTask : Net.SendMatch(m.MatchId, intent.Value.op, intent.Value.payload);
        }

        public Task StartRoom() => Match == null ? Task.CompletedTask : Net.SendMatch(Match.MatchId, TangoMatch.SendStart);
        public Task Rematch() => Match == null ? Task.CompletedTask : Net.SendMatch(Match.MatchId, TangoMatch.SendRematch);
        public Task SendEmote(string id) => Match == null ? Task.CompletedTask : Net.SendMatch(Match.MatchId, TangoMatch.SendEmote, new JObject { ["id"] = id });

        /// <summary>Leaves the table. In a running game this forfeits (the entry fee is lost, as the rules say).</summary>
        public async Task Leave()
        {
            var m = Match;
            lock (pumpLock) Match = null;
            ForgetMatch();
            if (m == null || !Net.SocketOpen) return;
            try
            {
                if (m.Status == "PLAYING") await Net.SendMatch(m.MatchId, TangoMatch.SendForfeit);
                await Net.LeaveMatch(m.MatchId);
            }
            catch (Exception) { }
        }

        void ForgetMatch() => save(MatchKey, "");

        /// <summary>Applies the match messages that arrived since the last call. Main thread only.</summary>
        public void Pump()
        {
            bool changed = false; TangoMatch finished = null;
            lock (pumpLock)
            {
                while (Net.TryDequeue(out var msg))
                {
                    var m = Match;
                    if (m == null || (msg.MatchId != m.MatchId && !string.IsNullOrEmpty(msg.MatchId))) continue;
                    if (m.Apply(msg.OpCode, msg.Json)) changed = true;
                    if (msg.OpCode == TangoMatch.OpGameOver) finished = m;
                    string err = m.Error;
                    if (!string.IsNullOrEmpty(err)) LastError = err;
                }
            }
            if (finished != null) GameOver?.Invoke(finished);
            if (changed) Changed?.Invoke();
        }

        // ------------------------------------------------------------------ friends
        public Task<JObject> Friends() => Call("tango.friends.list");
        public Task<JObject> AddFriend(string query) => Call("tango.friends.add", new JObject { ["query"] = query });
        public Task<JObject> FindFriend(string query) => Call("tango.friends.find", new JObject { ["query"] = query });
        public Task<JObject> FriendAction(string userId, string action) => Call("tango.friends.action", new JObject { ["user_id"] = userId, ["action"] = action });
        public Task<JObject> InviteFriend(string userId, string roomCode, string mode) => Call("tango.friends.invite", new JObject { ["user_id"] = userId, ["room_code"] = roomCode, ["mode"] = mode });

        // ------------------------------------------------------------------ helpers
        /// <summary>Calls an RPC and throws with a readable message when the server refuses.</summary>
        public async Task<JObject> Call(string id, object payload = null)
        {
            await Initialize0();
            var r = await Net.Rpc(id, payload);
            if (r["success"] != null && (bool?)r["success"] == false)
                throw new InvalidOperationException(Refusal((string)r["error"], (string)r["message"]));
            return r;
        }

        // RPCs made during sign-in itself must not wait for sign-in to finish.
        Task Initialize0() => SignedIn ? Task.CompletedTask : Initialize();

        /// <summary>"avatar_kai" → "avatar:kai"; "back_frost" → "back:frost". Unknown prefixes give null.</summary>
        public static string ClientKey(string serverId)
        {
            if (string.IsNullOrEmpty(serverId)) return null;
            int cut = serverId.IndexOf('_');
            if (cut <= 0) return null;
            string prefix = serverId.Substring(0, cut), item = serverId.Substring(cut + 1);
            switch (prefix)
            {
                case "avatar": case "frame": case "badge": case "back": case "table": case "emote": case "effect": return prefix + ":" + item;
                default: return null;
            }
        }

        public static string ServerItemId(string category, string item) => (category == "card_back" ? "back" : category) + "_" + item;

        public static string Refusal(string code, string message)
        {
            switch (code ?? "")
            {
                case "INSUFFICIENT_FUNDS": return "You don't have enough coins or diamonds for this.";
                case "ALREADY_OWNED": return "You already own this item.";
                case "NOT_OWNED": return "Buy this item before using it.";
                case "UNKNOWN_ITEM": return "That item is not available.";
                case "INVALID_NAME": return "Use 3 to 16 letters, numbers or spaces.";
                case "ROOM_NOT_FOUND": return "No room with that code. Check the code and try again.";
                case "ROOM_STARTED": return "That room's match has already started.";
                case "ROOM_FULL": return "That room is full.";
                case "INSUFFICIENT_COINS": return "You need more coins for this entry fee.";
                case "MAINTENANCE": return string.IsNullOrEmpty(message) ? "The server is under maintenance. Please try again soon." : message;
                case "RATE_LIMITED": return "Too many tries. Wait a moment and try again.";
                case "ALREADY_IN_MATCH": return "You are already at a table.";
                case "DAILY_LIMIT": return "That's all the free coins for today.";
                case "TOO_SOON": return "Free coins are on a short break. Try again in a moment.";
                case "NOT_FOUND": return "No player found with that ID.";
                default: return string.IsNullOrEmpty(message) ? (code ?? "Request refused.").Replace('_', ' ').ToLowerInvariant() : message;
            }
        }
    }
}
