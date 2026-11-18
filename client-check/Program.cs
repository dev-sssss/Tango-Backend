using System;
using System.Collections.Generic;
using System.Linq;
using System.Threading;
using System.Threading.Tasks;
using UnoGame;
using UnoGame.Online;

// Plays real matches against a Tango server with the same client code the Unity game uses.
static class Program
{
    static TangoServerConfig Config(string[] a) => new TangoServerConfig
    { Scheme = a.Length > 0 ? a[0] : "http", Host = a.Length > 1 ? a[1] : "127.0.0.1", Port = a.Length > 2 ? int.Parse(a[2]) : 7350, ServerKey = a.Length > 3 ? a[3] : "local-key" };

    static TangoGameClient NewClient(TangoServerConfig cfg, string device)
    {
        var store = new Dictionary<string, string>();
        return new TangoGameClient(cfg, device, k => store.TryGetValue(k, out var v) ? v : "", (k, v) => store[k] = v);
    }

    static void Check(bool ok, string what) { if (!ok) throw new Exception("CHECK FAILED: " + what); Console.WriteLine("  ok  " + what); }

    static async Task<int> Main(string[] args)
    {
        var cfg = Config(args);
        string suffix = DateTime.UtcNow.Ticks.ToString();
        try
        {
            Console.WriteLine("== sign-in, profile, shop");
            var a = NewClient(cfg, "check-a-" + suffix);
            await a.Initialize();
            Check(a.SignedIn && a.Net.SocketOpen, "signed in and socket open");
            var p = a.Profile;
            Console.WriteLine($"     coins {p.Coins} diamonds {p.Diamonds} trophies {p.Trophies} owned {string.Join(",", p.Owned)} equipped {string.Join(",", p.Equipped.Select(e => e.Key + "=" + e.Value))}");
            Check(p.Coins == 2000 && p.Owned.Contains("avatar:nova") && p.Owned.Contains("emote:wink") && p.Equipped["avatar"] == "nova" && p.Equipped["back"] == "default", "starting wallet and free items");
            p = await a.SetName("Check A");
            Check(p.Name == "Check A", "name saved");
            p = await a.Purchase("frame", "sakura");
            Check(p.Owned.Contains("frame:sakura") && p.Coins == 1850, "bought Sakura frame for 150 coins");
            p = await a.Equip("frame", "sakura");
            Check(p.Equipped["frame"] == "sakura", "equipped Sakura frame");
            try { await a.Purchase("effect", "legend"); Check(false, "should refuse"); } catch (InvalidOperationException ex) { Check(ex.Message.Contains("enough coins or diamonds"), "refused effect without diamonds: " + ex.Message); }
            p = await a.DevGrant();
            Check(p.Coins == 11850 && p.Diamonds == 1020, "developer top-up");
            p = await a.Purchase("emote", "party");
            p = await a.Equip("back", "default");
            Check(p.Owned.Contains("emote:party") && p.Diamonds == 1008, "bought an emote with diamonds");

            Console.WriteLine("== public 1v1 Classic, 200 coins, bot fills the seat");
            int before = a.Profile.Coins;
            var snap = await a.QuickMatch("classic", "1v1", 200);
            Check(snap != null && !snap.Started && snap.Capacity == 2, "waiting at a public table");
            await Play(new[] { a }, TimeSpan.FromSeconds(240));
            var m = a.Match;
            var mine = m.Results.First(r => r.Seat == m.MySeat);
            p = await a.RefreshProfile();
            Console.WriteLine($"     result: rank {mine.Rank} coins {mine.Coins} trophies {mine.Trophies}; wallet {before} -> {p.Coins}");
            Check(p.Coins == before + mine.Coins, "wallet moved by the server's payout");
            Check(m.View().Events.Any(e => e.Type == "play"), "events for the table animations");
            await a.Leave();

            Console.WriteLine("== private room, 4 players each for themselves: create, join by code, start, bots fill");
            var b = NewClient(cfg, "check-b-" + suffix);
            await b.Initialize();
            await b.SetName("Check B");
            var room = await a.CreateRoom("no_mercy", "4p", 15);
            Check(room.IsHost && room.RoomCode.Length == 6 && room.Capacity == 4 && !room.Teams, "room " + room.RoomCode + " created");
            var joined = await b.JoinRoom(room.RoomCode);
            Check(joined.Players.Count >= 2 && !joined.IsHost, "second player joined");
            await a.StartRoom();
            await Play(new[] { a, b }, TimeSpan.FromSeconds(300));
            Check(a.Match.RematchAllowed, "rematch offered in a private room");
            await a.Leave(); await b.Leave();

            Console.WriteLine("== friends: find by ID, request, accept, invite");
            var fa = await a.Friends(); string codeA = (string)fa["code"];
            var fb = await b.Friends(); string codeB = (string)fb["code"];
            var found = await a.FindFriend(codeB);
            Check((string)found["player"]["display_name"] == "Check B", "found Check B by ID " + codeB);
            await a.AddFriend(codeB);
            var incoming = (await b.Friends())["incoming"].First;
            await b.FriendAction((string)incoming["user_id"], "accept");
            Check(((Newtonsoft.Json.Linq.JArray)(await a.Friends())["friends"]).Count == 1, "now friends");

            Console.WriteLine("== player ID made with the account");
            Check(a.Profile.PlayerId.Length == 8 && a.Profile.PlayerId == codeA, "player ID " + a.Profile.PlayerId + " in the profile and the friends screen");
            Check((await a.RefreshProfile()).PlayerId == codeA, "player ID stays the same");

            Console.WriteLine("== Classic 2 vs 2 room: a stranger joins by code, an invited friend sits on the host's team");
            var s = NewClient(cfg, "check-s-" + suffix);
            await s.Initialize();
            await s.SetName("Check S");
            var croom = await a.CreateRoom("classic", "2v2", 10, true, 3);
            Check(croom.IsHost && croom.Teams && croom.Capacity == 4 && croom.Preset == "team", "Classic team room " + croom.RoomCode + " created");
            await s.JoinRoom(croom.RoomCode);
            await a.InviteFriend(b.Net.Session.UserId, croom.RoomCode, "classic", "2v2");
            var inv = (await b.RoomInvites())["invites"].First;
            Check(inv != null && (string)inv["room_code"] == croom.RoomCode && (string)inv["from"]["user_id"] == a.Net.Session.UserId, "invite arrived in the friend's inbox");
            await b.AnswerInvite(a.Net.Session.UserId);
            await b.JoinRoom(croom.RoomCode, a.Net.Session.UserId);
            Check(!((Newtonsoft.Json.Linq.JArray)(await b.RoomInvites())["invites"]).Any(), "answered invite is gone");
            await a.StartRoom();
            var until = DateTime.UtcNow.AddSeconds(30);
            while ((a.Match?.Started != true || b.Match?.Started != true) && DateTime.UtcNow < until) { a.Pump(); b.Pump(); s.Pump(); await Task.Delay(50); }
            var seats = a.Match.View().Players;
            int teamA = seats.First(x => x.Name == "Check A").Team, teamB = seats.First(x => x.Name == "Check B").Team, teamS = seats.First(x => x.Name == "Check S").Team;
            Check(teamA == teamB && teamA != teamS, "teams: host and invited friend together, stranger on the other side");
            await Play(new[] { a, b, s }, TimeSpan.FromSeconds(300));
            Check(a.Match.Results.Count == 4, "Classic team game played to the end");
            await a.Leave(); await b.Leave(); await s.Leave();

            Console.WriteLine("== party: friends take a Classic 2 vs 2 table to public matchmaking together");
            var party = await a.CreateRoom("classic", "2v2", 10, true, 0);
            await a.InviteFriend(b.Net.Session.UserId, party.RoomCode, "classic", "2v2");
            await b.AnswerInvite(a.Net.Session.UserId);
            await b.JoinRoom(party.RoomCode, a.Net.Session.UserId);
            try { await b.FindOpponents(200); Check(false, "only the leader may start the search"); } catch (InvalidOperationException ex) { Check(true, "friend can't start the search: " + ex.Message); }
            await a.FindOpponents(200);
            var stranger = NewClient(cfg, "check-p-" + suffix);
            await stranger.Initialize();
            await stranger.SetName("Check P");
            await stranger.QuickMatch("classic", "2v2", 200);
            Check(stranger.Match.MatchId == a.Match.MatchId, "a player pressing PLAY on that table joins the party's match");
            until = DateTime.UtcNow.AddSeconds(40);
            while ((a.Match?.Started != true || stranger.Match?.Started != true) && DateTime.UtcNow < until) { a.Pump(); b.Pump(); stranger.Pump(); await Task.Delay(50); }
            Check(a.Match.Started, "bots filled the last seat after the wait and the game started");
            seats = a.Match.View().Players;
            teamA = seats.First(x => x.Name == "Check A").Team; teamB = seats.First(x => x.Name == "Check B").Team; int teamP = seats.First(x => x.Name == "Check P").Team;
            Check(teamA == teamB && teamP != teamA, "party together on one team, the stranger on the other");
            await Play(new[] { a, b, stranger }, TimeSpan.FromSeconds(300));
            Check(a.Match.Results.Count == 4, "party game played to the end");
            await a.Leave(); await b.Leave(); await stranger.Leave();

            Console.WriteLine("== live lobby counts");
            var stats = await a.LobbyStats("classic");
            Check(stats.Online >= 1, "players online: " + stats.Online);

            Console.WriteLine("== rankings");
            foreach (var id in new[] { "trophies", "weekly_trophies", "wins", "friends" })
            {
                var board = await a.Leaderboard(id);
                Console.WriteLine($"     {id}: {board.Rows.Count} rows, my rank {board.MyRank} score {board.MyScore}");
                Check(board.Rows.Count > 0 && board.Rows.All(r => r.Rank > 0 && r.Name.Length > 0), id + " board has ranked rows");
            }
            var pa = await a.RefreshProfile();
            Check((await a.Leaderboard("trophies")).MyScore == pa.Trophies, "trophy board shows the profile's " + pa.Trophies + " trophies");
            var fr = await a.Leaderboard("friends");
            Check(fr.Rows.Any(r => r.UserId == b.Net.Session.UserId) && fr.Rows.Any(r => r.UserId == a.Net.Session.UserId), "friends board shows you and your friend");

            Console.WriteLine("== lucky spin");
            var spin = await s.SpinStatus();
            Check(spin.FreeLeft == 1 && spin.PrizeCoins.Count == 8, "one free spin today, 8 slices");
            await s.RefreshProfile();
            int coins0 = s.Profile.Coins, dia0 = s.Profile.Diamonds;
            var won = await s.Spin(false);
            Check(won.Slice >= 0 && won.WonCoins == spin.PrizeCoins[won.Slice] && won.WonDiamonds == spin.PrizeDiamonds[won.Slice], $"landed on slice {won.Slice}: +{won.WonCoins} coins +{won.WonDiamonds} diamonds");
            Check(s.Profile.Coins == coins0 + won.WonCoins && s.Profile.Diamonds == dia0 + won.WonDiamonds, "prize paid into the wallet");
            try { await s.Spin(false); Check(false, "second free spin should be refused"); } catch (InvalidOperationException ex) { Check(true, "second free spin refused: " + ex.Message); }

            Console.WriteLine("== reconnect in the middle of a game");
            var c = NewClient(cfg, "check-c-" + suffix);
            await c.Initialize();
            await c.QuickMatch("no_mercy", "1v1", 400);
            await Play(new[] { c }, TimeSpan.FromSeconds(40), stopAfterTurns: 3);
            await c.Net.CloseSocket();
            await Task.Delay(500);
            await c.EnsureConnected();
            await Play(new[] { c }, TimeSpan.FromSeconds(240));
            Check(c.Match.Status == "OVER", "finished after reconnecting");
            Console.WriteLine("ALL CHECKS PASSED");
            return 0;
        }
        catch (Exception ex) { Console.WriteLine("FAILED: " + ex); return 1; }
    }

    /// <summary>Plays for every client until the match is over: the same actions the table sends.</summary>
    static async Task Play(TangoGameClient[] clients, TimeSpan limit, int stopAfterTurns = 0)
    {
        var until = DateTime.UtcNow + limit;
        var lastRev = clients.ToDictionary(c => c, c => -1);
        int myTurns = 0; var seenTypes = new HashSet<string>();
        while (DateTime.UtcNow < until)
        {
            bool allOver = true;
            foreach (var c in clients)
            {
                c.Pump();
                var m = c.Match;
                if (m == null || m.State == null) { allOver = false; continue; }
                if (m.Status != "OVER") allOver = false;
                if (!string.IsNullOrEmpty(c.LastError)) { Console.WriteLine("     server said: " + c.LastError); }
                var v = m.Started ? m.View() : null;
                if (v == null || m.Revision == lastRev[c]) continue;
                lastRev[c] = m.Revision;
                foreach (var e in v.Events) seenTypes.Add(e.Type);
                GameAction act = null;
                if (v.CanCatchUno) act = new GameAction { Type = "catch", TargetSeat = v.CatchTargets[0] };
                else if (v.CanCallUno) act = new GameAction { Type = "uno" };
                else if (v.CurrentSeat == v.LocalSeat && v.Phase != "matchOver")
                {
                    myTurns++;
                    var playable = v.Hand.Where(h => v.PlayableCardIds.Contains(h.Id)).ToList();
                    if (v.CanChooseColor && v.CanPass) act = new GameAction { Type = "color", Color = "red" };
                    else if (v.CanChooseColor) act = new GameAction { Type = "color", Color = "blue" };
                    else if (v.CanChallenge && myTurns % 2 == 0) act = new GameAction { Type = "challenge" };
                    else if (playable.Count > 0) { var card = playable[0]; act = new GameAction { Type = "play", CardId = card.Id, Color = card.Color == "wild" ? "green" : null }; }
                    else if (v.CanDraw) act = new GameAction { Type = "draw" };
                }
                if (act != null) await c.Act(act);
                if (stopAfterTurns > 0 && myTurns >= stopAfterTurns) return;
            }
            if (allOver) { Console.WriteLine("     event types seen: " + string.Join(" ", seenTypes.OrderBy(x => x))); return; }
            await Task.Delay(60);
        }
        throw new TimeoutException("match did not finish; status " + string.Join(",", clients.Select(c => c.Match?.Status)));
    }
}
