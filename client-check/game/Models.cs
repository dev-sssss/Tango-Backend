using System;
using System.Collections.Generic;

namespace UnoGame
{
    [Serializable]
    public sealed class Card
    {
        public string Id;
        public string Color;
        public string Kind;
        public int Number;
        public Card Copy() { return new Card { Id = Id, Color = Color, Kind = Kind, Number = Number }; }
    }

    [Serializable]
    public sealed class PlayerSpec
    {
        public string Id;
        public string Name;
        public bool IsBot;
        public string Difficulty = "medium";
        public int Team;
    }

    [Serializable]
    public sealed class PlayerState
    {
        public string Id;
        public string Name;
        public bool IsBot;
        public string Difficulty;
        public int Team;
        public int Score;
        public int MissedTurns;
        /// <summary>Set by the server for a player who left the table or is missing turns; shown as DISCONNECTED.</summary>
        public bool Away;
        public bool UnoDeclared;
        public bool Eliminated;
        public List<Card> Hand = new List<Card>();
    }

    // Public effects only. Draw and hand-exchange events NEVER contain private cards.
    [Serializable]
    public sealed class MatchEvent
    {
        public int Id;
        public string Type;
        public int ActorSeat = -1;
        public int TargetSeat = -1;
        public int Amount;
        public string Color;
        public Card Card;
        public MatchEvent Copy()
        {
            return new MatchEvent { Id = Id, Type = Type, ActorSeat = ActorSeat, TargetSeat = TargetSeat,
                Amount = Amount, Color = Color, Card = Card == null ? null : Card.Copy() };
        }
    }

    // Private authority state. Never transmit this object to a multiplayer client.
    [Serializable]
    public sealed class MatchState
    {
        public string MatchId;
        public string RoomCode;
        public string Mode;
        public string Preset = "house";
        public string Phase;
        public string ActiveColor;
        public string Message;
        public int Revision;
        public int CurrentSeat;
        // Changes only when a player turn advances, not while resolving a choice on that turn.
        public int TurnId;
        public int Direction = 1;
        public int WinnerSeat = -1;
        public int RoundPoints;
        public int TargetScore = 1000;
        public int DealerSeat;
        public int RoundNumber;
        public bool Teams;
        public bool SecureRandom;
        public int RandomState;
        public string DrawnCardId;
        public int PendingDraw;
        public int PendingDrawMinimum;
        // Legacy fields retained for save/network compatibility; no challenge rule is used.
        public int PendingOffender = -1;
        public int PendingTarget = -1;
        public bool DrawFourIllegal;
        public bool ChallengeOpen;
        public string ChallengeColor;
        /// <summary>Match clock: the moment the table closes (0 = no clock). When it passes, the lightest hand wins.</summary>
        public long EndsAtUnixMs;
        public bool TimeUp;
        public int PendingWinner = -1;
        public int CatchSeat = -1;
        public List<int> CatchSeats = new List<int>();
        public int NextEventId = 1;
        public int NextLastCardCallId = 1;
        public List<LastCardCall> LastCardCalls = new List<LastCardCall>();
        public List<MatchEvent> Events = new List<MatchEvent>();
        public List<PlayerState> Players = new List<PlayerState>();
        public List<Card> DrawPile = new List<Card>();
        public List<Card> DiscardPile = new List<Card>();
        public List<Card> EliminatedPile = new List<Card>();
    }

    [Serializable]
    public sealed class LastCardCall
    {
        public int Id;
        public int Seat;
        public long ExpiresAtUnixMs;
        public int ExpiresAtTurnId;
        public LastCardCall Copy() { return new LastCardCall { Id = Id, Seat = Seat, ExpiresAtUnixMs = ExpiresAtUnixMs, ExpiresAtTurnId = ExpiresAtTurnId }; }
    }

    [Serializable]
    public sealed class PlayerView
    {
        public string Id;
        public string Name;
        public bool IsBot;
        public string Difficulty;
        public int Team;
        public int Count;
        public int Score;
        public int MissedTurns;
        /// <summary>Set by the server for a player who left the table or is missing turns; shown as DISCONNECTED.</summary>
        public bool Away;
        public bool UnoDeclared;
        public bool Eliminated;
        /// <summary>The player's equipped cosmetics, e.g. "a=nova;f=gold;b=frost;x=flame;g=star" (empty when unknown).</summary>
        public string Look = "";
        /// <summary>Trophies shown on the matchmaking screen (online tables).</summary>
        public int Trophies;
    }

    // The only match object which is safe to expose to an individual player.
    [Serializable]
    public sealed class AllyHand
    {
        public int Seat;
        public List<Card> Cards = new List<Card>();
    }

    [Serializable]
    public sealed class MatchView
    {
        /// <summary>In team matches: the hands of your partners (never the opponents').</summary>
        public List<AllyHand> AllyHands = new List<AllyHand>();
        public string MatchId;
        public string RoomCode;
        public string Mode;
        public string Preset = "house";
        public string Phase;
        public string ActiveColor;
        public string Message;
        public int Revision;
        public int CurrentSeat;
        public int TurnId;
        public int LocalSeat;
        public int Direction;
        public int WinnerSeat = -1;
        public int RoundPoints;
        public int TargetScore;
        public int PendingDraw;
        public int PendingDrawMinimum;
        public bool CanDraw;
        public bool CanPass;
        public bool CanChooseColor;
        public bool CanRespondDrawFour;
        public bool CanChallenge;
        public string ChallengeColor;
        /// <summary>Who played the wild draw card that can be challenged (-1 when nothing is open).</summary>
        public int ChallengeSeat = -1;
        /// <summary>Match clock in the authority's time (0 = no clock), and whether the match ended on it.</summary>
        public long EndsAtUnixMs;
        public bool TimeUp;
        public bool CanCallUno;
        public bool CanCatchUno;
        public bool CanChooseSwap;
        public bool CanChooseRouletteColor;
        public List<int> SwapTargets = new List<int>();
        public List<int> CatchTargets = new List<int>();
        public List<LastCardCall> LastCardCalls = new List<LastCardCall>();
        public List<PlayerView> Players = new List<PlayerView>();
        public List<Card> Hand = new List<Card>();
        public Card TopCard;
        public List<string> PlayableCardIds = new List<string>();
        public List<MatchEvent> Events = new List<MatchEvent>();
    }

    [Serializable]
    public sealed class GameAction
    {
        public string Type;
        public string CardId;
        public string Color;
        public int TargetSeat = -1;
        public int DeclarationId = -1;
    }

    public sealed class RuleException : Exception
    {
        public RuleException(string message) : base(message) { }
    }
}
