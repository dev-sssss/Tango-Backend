using System;
using System.Collections.Generic;

namespace UnoGame.Online
{
    // Public, short-lived room reactions. These never contain cards or game actions.
    [Serializable]
    public sealed class RoomEmote
    {
        public long Id;
        public string PlayerId = "";
        public string Emoji = "";
        public int Seat;
        public int TargetSeat = -1;
        public long SentUnixMs;
        public long ExpiresUnixMs;
    }

    public static class EmoteRules
    {
        public const int LifetimeMs = 5000;
        public const int CooldownMs = 2000;
        public const int MaximumEvents = 16;
        /// <summary>Every emote in the game, in shop order. The first four are free.</summary>
        public static readonly string[] Ids = {
            "wink", "laugh", "gasp", "grin", "think", "shy", "cry", "angry",
            "love", "cool", "party", "sleepy", "kiss", "tongue", "mindblown", "plead", "rage", "smirk",
            "starstruck", "scream", "sad", "yawn", "halo", "devil", "nerd", "shush", "eyeroll", "zany" };
        public static bool IsAllowed(string emoji) => emoji != null && Array.IndexOf(Ids, emoji) >= 0;
    }

    [Serializable]
    public sealed class OnlineRoomSnapshot
    {
        public string RoomCode = "";
        public int Capacity;
        public bool IsHost;
        public bool Started;
        /// <summary>Every seat is taken and the round begins in a moment.</summary>
        public bool Starting;
        public bool Teams;
        public string Preset = "house";
        public string Error = "";
        public List<PlayerView> Players = new List<PlayerView>();
        public MatchView View;
        public List<RoomEmote> Emotes = new List<RoomEmote>();
        public long TurnDeadlineUnixMs;
        public long ServerTimeUnixMs;
        /// <summary>Full match length chosen for this room, in seconds (0 = the usual length of its mode).</summary>
        public int MatchSeconds;
        /// <summary>Seconds per turn at this table.</summary>
        public int TurnSeconds;
        /// <summary>A friends room (invite-only) rather than a public table.</summary>
        public bool Private;
        /// <summary>A public table a group of friends took to matchmaking together.</summary>
        public bool Party;
        /// <summary>Entry fee of a public table.</summary>
        public int Fee;
        /// <summary>When the server fills the empty seats of a public table with players (0 = not waiting).</summary>
        public long FillAtUnixMs;
    }

    /// <summary>Who is playing right now: players online and the players waiting at each public table of a mode.</summary>
    public sealed class LobbyStats
    {
        public int Online, InGame;
        /// <summary>"format_fee" (e.g. "2v2_400") → players waiting at that table.</summary>
        public Dictionary<string, int> Waiting = new Dictionary<string, int>();
        public int WaitingAt(string format, int fee) => Waiting.TryGetValue(format + "_" + fee, out int n) ? n : 0;
    }

    /// <summary>The player's wallet, collection and standing, as the server keeps them.</summary>
    [Serializable]
    public sealed class CosmeticProfile
    {
        public string Error = "";
        public int Coins;
        public int Diamonds;
        public string Name = "";
        /// <summary>The permanent public player ID friends search for (made by the server with the account).</summary>
        public string PlayerId = "";
        public int Trophies, Level = 1, Wins, Games;
        public bool TutorialDone;
        public int MatchRewardCoins;
        public List<string> Owned = new List<string>();
        public Dictionary<string, string> Equipped = new Dictionary<string, string>();
        public List<string> RewardedMatchIds = new List<string>();
    }
    // ---- Friends (shared by the game and the cloud module) ----
    [Serializable]
    public sealed class SocialPerson
    {
        public string Code = "";
        public string Name = "";
        public string Avatar = "";
        public int Trophies;
        public long LastSeenUnixMs;
        /// <summary>"online", "in_game" or "offline".</summary>
        public string Status = "offline";
    }
    [Serializable]
    public sealed class SocialInvite
    {
        public string From = "";
        public string Name = "";
        public string Room = "";
        public long SentUnixMs;
        public string Mode = "";
        public string Format = "";
        public string Avatar = "";
    }

    /// <summary>One line of a ranking board.</summary>
    [Serializable]
    public sealed class RankRow
    {
        public int Rank;
        public string UserId = "";
        public string Name = "";
        public string Avatar = "";
        public string Frame = "";
        public long Score;
        public int Level = 1;
    }

    [Serializable]
    public sealed class RankBoard
    {
        public string Id = "";
        public List<RankRow> Rows = new List<RankRow>();
        public int MyRank;
        public long MyScore;
    }

    /// <summary>The lucky spin: its slices, spins left today, and (after a spin) the slice that won.</summary>
    [Serializable]
    public sealed class SpinState
    {
        public List<int> PrizeCoins = new List<int>();
        public List<int> PrizeDiamonds = new List<int>();
        public int FreeLeft, AdLeft;
        public long AdWaitMs, NextResetUnixMs;
        public int Slice = -1, WonCoins, WonDiamonds;
    }
    [Serializable]
    public sealed class SocialState
    {
        public string Error = "";
        public SocialPerson Me;
        public List<SocialPerson> Friends = new List<SocialPerson>();
        public List<SocialPerson> Requests = new List<SocialPerson>();
        public List<SocialInvite> Invites = new List<SocialInvite>();
        public long ServerTimeUnixMs;
    }
}
