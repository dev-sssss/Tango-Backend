package tango

import (
	"math"
	"sort"
)

// Payout rules (entry fee F):
//
//   - Winning team: every member gains F. In team games each winner's gain
//     drops by up to 25% of F in proportion to the points their teammates
//     still hold, so a win where a teammate kept many cards pays a little less.
//   - Losing players are ranked by points left in hand (fewest first). The
//     last place loses the full F; the others lose F × points / most points,
//     at least 10% of F. Rounded to the nearest 10 coins.
//   - A player removed for leaving or being idle always loses the full F.
//   - Trophies: +30 for a win, −10 for a loss, −15 for leaving.
const (
	TrophiesWin      = 30
	TrophiesLoss     = -10
	TrophiesForfeit  = -15
	TrophiesPerLevel = 100
	teamWinMaxCut    = 0.25
	minLossShare     = 0.10
)

// ResultRow is one player's line on the scoreboard.
type ResultRow struct {
	Seat     int    `json:"seat"`
	Rank     int    `json:"rank"`
	UserID   string `json:"user_id"`
	Name     string `json:"name"`
	Avatar   string `json:"avatar,omitempty"`
	Frame    string `json:"frame,omitempty"`
	Team     int    `json:"team"`
	Points   int    `json:"points"`
	Winner   bool   `json:"winner"`
	Removed  bool   `json:"removed"`
	Coins    int64  `json:"coins"`
	Trophies int    `json:"trophies"`
	bot      bool
}

// LevelFor converts trophies to a level (100 trophies per level, from 1).
func LevelFor(trophies int) int {
	if trophies < 0 {
		trophies = 0
	}
	return trophies/TrophiesPerLevel + 1
}

func round10(x float64) int64 { return int64(math.Round(x/10) * 10) }

// ComputeResult ranks the players of a finished table and works out coins
// and trophies for each.
func ComputeResult(t *Table, fee int64) []ResultRow {
	var winners, losers, removed []ResultRow
	maxLoser := 0
	for _, p := range t.Players {
		r := ResultRow{Seat: p.Seat, UserID: p.UserID, Name: p.Name, Avatar: p.Avatar, Frame: p.Frame, Team: p.Team,
			Points: p.FinalPoints, Removed: p.Removed, bot: p.Bot}
		switch {
		case p.Removed:
			removed = append(removed, r)
		case p.Team == t.WinTeam:
			r.Winner = true
			winners = append(winners, r)
		default:
			losers = append(losers, r)
			if r.Points > maxLoser {
				maxLoser = r.Points
			}
		}
	}
	sort.SliceStable(winners, func(i, j int) bool {
		if winners[i].Seat == t.Winner {
			return true
		}
		if winners[j].Seat == t.Winner {
			return false
		}
		return winners[i].Points < winners[j].Points
	})
	sort.SliceStable(losers, func(i, j int) bool { return losers[i].Points < losers[j].Points })

	teamPts := 0
	for _, w := range winners {
		teamPts += w.Points
	}
	for i := range winners {
		w := &winners[i]
		mates := teamPts - w.Points
		cut := 0.0
		if mates > 0 {
			den := math.Max(float64(maxLoser), float64(mates))
			cut = teamWinMaxCut * float64(mates) / den
		}
		w.Coins = fee - round10(float64(fee)*cut)
		w.Trophies = TrophiesWin
	}
	for i := range losers {
		l := &losers[i]
		share := 1.0
		if i != len(losers)-1 && maxLoser > 0 {
			share = math.Max(minLossShare, float64(l.Points)/float64(maxLoser))
		}
		l.Coins = -round10(float64(fee) * share)
		if -l.Coins > fee {
			l.Coins = -fee
		}
		l.Trophies = TrophiesLoss
	}
	for i := range removed {
		removed[i].Coins = -fee
		removed[i].Trophies = TrophiesForfeit
	}
	out := append(append(winners, losers...), removed...)
	for i := range out {
		out[i].Rank = i + 1
	}
	return out
}
