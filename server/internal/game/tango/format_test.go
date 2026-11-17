package tango

import "testing"

func TestFreeForAllFormats(t *testing.T) {
	for _, tc := range []struct {
		format string
		seats  int
		team   bool
	}{{Format1v1, 2, false}, {Format2v2, 4, true}, {Format4v4, 8, true}, {Format4P, 4, false}, {Format8P, 8, false}} {
		if !ValidFormat(tc.format) || FormatPlayers(tc.format) != tc.seats || IsTeamFormat(tc.format) != tc.team {
			t.Fatalf("%s: players %d team %v", tc.format, FormatPlayers(tc.format), IsTeamFormat(tc.format))
		}
		for seat := 0; seat < tc.seats; seat++ {
			want := seat
			if tc.team {
				want = seat % 2
			}
			if TeamOf(tc.format, seat) != want {
				t.Fatalf("%s seat %d team %d", tc.format, seat, TeamOf(tc.format, seat))
			}
		}
	}
	// Four players, each on their own: the winner gains the fee, the
	// heaviest hand loses all of it, the others lose by their points.
	tb := &Table{Format: Format4P, Winner: 2, WinTeam: TeamOf(Format4P, 2)}
	for i, pts := range []int{30, 60, 0, 15} {
		tb.Players = append(tb.Players, &Player{Seat: i, Team: TeamOf(Format4P, i), FinalPoints: pts})
	}
	coins := map[int]int64{}
	for _, r := range ComputeResult(tb, 400) {
		coins[r.Seat] = r.Coins
	}
	if coins[2] != 400 || coins[1] != -400 || coins[0] != -200 || coins[3] != -100 {
		t.Fatalf("free-for-all payout %v", coins)
	}
}
