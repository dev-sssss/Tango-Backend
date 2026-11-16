package game

import "time"

// Match facts for reporting (games and players per hour/day, modes, bot
// fill, outcomes). Recording is optional: only the production server's NK
// implements Analytics, so the in-memory and Lua-vs-Go oracle runtimes are
// unaffected and gameplay never depends on it.

type MatchPlayerFact struct {
	Seat   int
	UserID string
	Bot    bool
}

type MatchStartFact struct {
	MatchID   string
	Mode      string
	Private   bool
	EntryFee  int64
	StartedAt time.Time
	Players   []MatchPlayerFact
}

type MatchResultFact struct {
	Seat      int
	Rank      int
	Forfeited bool
	Prize     int64
}

type MatchEndFact struct {
	MatchID     string
	EndedAt     time.Time
	DurationSec int64
	// EndReason is "normal", "humans_left" (bots finished alone) or "test_win".
	EndReason string
	Results   []MatchResultFact
}

// OfflineGameFact is a game played on the device without the server (against
// the computer or pass-and-play), reported by the client afterwards.
type OfflineGameFact struct {
	GameID      string // client-generated; repeats are ignored
	Mode        string // "vs_computer" or "pass_and_play"
	Humans      int
	Bots        int
	Difficulty  string // "easy" / "medium" / "hard" when bots played
	StartedAt   time.Time
	DurationSec int64
	Outcome     string // "won", "lost", "finished" or "quit"
	Client      string
}

// Analytics is implemented by NKs that persist match facts. All calls must
// return quickly; implementations queue the write.
type Analytics interface {
	RecordMatchStart(MatchStartFact)
	RecordMatchEnd(MatchEndFact)
	RecordOfflineGames(userID string, games []OfflineGameFact)
}

// RecordMatchStart forwards to nk when it implements Analytics.
func RecordMatchStart(nk NK, f MatchStartFact) {
	if a, ok := nk.(Analytics); ok {
		a.RecordMatchStart(f)
	}
}

// RecordOfflineGames forwards to nk when it implements Analytics.
func RecordOfflineGames(nk NK, userID string, games []OfflineGameFact) {
	if a, ok := nk.(Analytics); ok && len(games) > 0 {
		a.RecordOfflineGames(userID, games)
	}
}

// RecordMatchEnd forwards to nk when it implements Analytics.
func RecordMatchEnd(nk NK, f MatchEndFact) {
	if a, ok := nk.(Analytics); ok {
		a.RecordMatchEnd(f)
	}
}
