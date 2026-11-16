package tango

import "testing"

func finished(format string, winner int, points ...int) *Table {
	t := &Table{Format: format, Over: true, Winner: winner, WinTeam: TeamOf(format, winner)}
	for i, p := range points {
		t.Players = append(t.Players, &Player{Seat: i, UserID: string(rune('a' + i)), Team: TeamOf(format, i), FinalPoints: p})
	}
	return t
}

func TestPayout1v1(t *testing.T) {
	res := ComputeResult(finished(Format1v1, 1, 87, 0), 200)
	if res[0].Seat != 1 || res[0].Coins != 200 || res[0].Trophies != TrophiesWin || !res[0].Winner {
		t.Fatalf("winner row %+v", res[0])
	}
	if res[1].Coins != -200 || res[1].Trophies != TrophiesLoss || res[1].Rank != 2 {
		t.Fatalf("loser row %+v", res[1])
	}
}

func TestPayoutTeamsLastLosesFull(t *testing.T) {
	// 2v2: seats 0,2 team 0; 1,3 team 1. Seat 2 went out; teammate 0 holds 40.
	res := ComputeResult(finished(Format2v2, 2, 40, 30, 0, 120), 1000)
	byseat := map[int]ResultRow{}
	for _, r := range res {
		byseat[r.Seat] = r
	}
	if res[0].Seat != 2 || res[1].Seat != 0 {
		t.Fatalf("winners order %+v", res)
	}
	if byseat[3].Coins != -1000 {
		t.Fatalf("last place must lose the full fee: %+v", byseat[3])
	}
	if byseat[1].Coins != -250 { // 30/120 of 1000
		t.Fatalf("seat1 %+v", byseat[1])
	}
	// Teammate points cut the win a little: 25% * 40/120 of 1000 ≈ 80.
	if byseat[2].Coins != 920 || byseat[0].Coins != 1000 {
		t.Fatalf("winners %+v %+v", byseat[2], byseat[0])
	}
}

func TestPayoutRemovedLosesFull(t *testing.T) {
	tb := finished(Format2v2, 1, 10, 0, 20, 5)
	tb.Players[0].Removed = true
	res := ComputeResult(tb, 400)
	last := res[len(res)-1]
	if last.Seat != 0 || last.Coins != -400 || last.Trophies != TrophiesForfeit {
		t.Fatalf("removed %+v", last)
	}
}

func TestPayoutMinimumLoss(t *testing.T) {
	res := ComputeResult(finished(Format2v2, 1, 1, 0, 200, 0), 5000)
	for _, r := range res {
		if r.Seat == 0 && r.Coins != -500 {
			t.Fatalf("minimum loss 10%%: %+v", r)
		}
	}
}

func TestLevelFor(t *testing.T) {
	if LevelFor(0) != 1 || LevelFor(99) != 1 || LevelFor(100) != 2 || LevelFor(250) != 3 {
		t.Fatal("levels")
	}
}
