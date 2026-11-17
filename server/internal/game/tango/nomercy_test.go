package tango

import "testing"

// The No Mercy cards and rules as the game describes them in HOW TO PLAY.

func TestNoMercyDeck(t *testing.T) {
	count := map[string]int{}
	for _, c := range NewDeck(ModeNoMercy) {
		count[c.Kind]++
	}
	want := map[string]int{KindNumber: 80, KindSkip: 12, KindReverse: 12, KindDraw2: 12, KindDiscardAll: 12, KindDraw4: 8,
		KindSkipAll: 8, KindWild4: 8, KindWild6: 4, KindWild10: 4}
	for k, n := range want {
		if count[k] != n {
			t.Fatalf("%s: %d want %d", k, count[k], n)
		}
	}
	if count[KindWild] != 0 || len(NewDeck(ModeClassic)) != 108 {
		t.Fatalf("plain wilds %d, classic %d", count[KindWild], len(NewDeck(ModeClassic)))
	}
}

func TestColouredPlusFourStacks(t *testing.T) {
	r4, b4 := card(ColorRed, KindDraw4, 0), card(ColorBlue, KindDraw4, 0)
	tb := table(Format4P, num(ColorRed, 3), ColorRed,
		[]Card{r4, num(ColorRed, 1)}, []Card{card(ColorRed, KindDraw2, 0), b4, num(ColorGreen, 1)}, []Card{num(ColorBlue, 2)}, []Card{num(ColorBlue, 3)})
	mustOK(t, tb.Play(0, r4.ID, "", 0))
	if tb.Pending != 4 || tb.Turn != 1 {
		t.Fatalf("pending %d turn %d", tb.Pending, tb.Turn)
	}
	// A red +2 answers (colour in play); a blue +4 does not.
	mustCode(t, tb.Play(1, b4.ID, "", 0), "MUST_STACK_OR_DRAW")
	red2 := tb.Players[1].Hand[0]
	mustOK(t, tb.Play(1, red2.ID, "", 0))
	// Seat 2 cannot answer the +6: takes all six automatically, seat 3 plays next.
	if len(tb.Players[2].Hand) != 7 || tb.Turn != 3 {
		t.Fatalf("seat 2 hand %d, turn %d", len(tb.Players[2].Hand), tb.Turn)
	}
}

func TestSkipEveryone(t *testing.T) {
	sa := card(ColorGreen, KindSkipAll, 0)
	tb := table(Format4P, num(ColorGreen, 3), ColorGreen, []Card{sa, num(ColorRed, 1), num(ColorRed, 2)}, []Card{num(ColorBlue, 1)}, []Card{num(ColorBlue, 2)}, []Card{num(ColorBlue, 3)})
	mustOK(t, tb.Play(0, sa.ID, "", 0))
	if tb.Turn != 0 || tb.Phase != PhaseTurn {
		t.Fatalf("turn %d phase %s", tb.Turn, tb.Phase)
	}
}

func TestDiscardAll(t *testing.T) {
	da := card(ColorBlue, KindDiscardAll, 0)
	tb := table(Format1v1, num(ColorBlue, 3), ColorBlue, []Card{da, num(ColorBlue, 1), num(ColorBlue, 8), num(ColorRed, 2)}, []Card{num(ColorGreen, 1)})
	mustOK(t, tb.Play(0, da.ID, "", 0))
	if len(tb.Players[0].Hand) != 1 || tb.Players[0].Hand[0].Color != ColorRed {
		t.Fatalf("hand %v", tb.Players[0].Hand)
	}
	if tb.Top().ID != da.ID || len(tb.Discard) != 4 {
		t.Fatalf("top %v discard %d", tb.Top(), len(tb.Discard))
	}
	if !tb.Players[0].Vulnerable {
		t.Fatal("down to one card without TANGO! should be hammerable")
	}
	// Discarding the last cards of a colour can win the round.
	da2 := card(ColorRed, KindDiscardAll, 0)
	tb2 := table(Format1v1, num(ColorRed, 3), ColorRed, []Card{da2, num(ColorRed, 1)}, []Card{num(ColorGreen, 1)})
	mustOK(t, tb2.Play(0, da2.ID, "", 0))
	if !tb2.Over || tb2.Winner != 0 {
		t.Fatalf("over %v winner %d", tb2.Over, tb2.Winner)
	}
}

func TestZeroRotatesHands(t *testing.T) {
	zero := num(ColorRed, 0)
	a, b, c := []Card{zero, num(ColorRed, 1)}, []Card{num(ColorBlue, 1), num(ColorBlue, 2)}, []Card{num(ColorGreen, 1), num(ColorGreen, 2), num(ColorGreen, 3)}
	tb := table(Format4P, num(ColorRed, 5), ColorRed, a, b, c, []Card{num(ColorYellow, 1)})
	mustOK(t, tb.Play(0, zero.ID, "", 0))
	// Hands pass in the direction of play: 0 → 1 → 2 → 3 → 0.
	if len(tb.Players[1].Hand) != 1 || tb.Players[1].Hand[0].Color != ColorRed ||
		len(tb.Players[2].Hand) != 2 || len(tb.Players[3].Hand) != 3 || tb.Players[0].Hand[0].Color != ColorYellow {
		t.Fatalf("after rotation: %v | %v | %v | %v", tb.Players[0].Hand, tb.Players[1].Hand, tb.Players[2].Hand, tb.Players[3].Hand)
	}
	// Classic keeps 0 a plain number.
	tc := table(Format1v1, num(ColorRed, 5), ColorRed, []Card{num(ColorRed, 0), num(ColorRed, 1)}, []Card{num(ColorBlue, 1)})
	tc.Mode = ModeClassic
	mustOK(t, tc.Play(0, tc.Players[0].Hand[0].ID, "", 0))
	if len(tc.Players[0].Hand) != 1 || tc.Players[0].Hand[0].Color != ColorRed {
		t.Fatal("classic 0 rotated hands")
	}
}

func TestSevenSwapsHands(t *testing.T) {
	seven := num(ColorRed, 7)
	tb := table(Format4P, num(ColorRed, 5), ColorRed, []Card{seven, num(ColorRed, 1), num(ColorRed, 2)}, []Card{num(ColorBlue, 1)},
		[]Card{num(ColorGreen, 1), num(ColorGreen, 2)}, []Card{num(ColorYellow, 1), num(ColorYellow, 2), num(ColorYellow, 3)})
	mustOK(t, tb.Play(0, seven.ID, "", 0))
	if tb.Phase != PhaseSwap || tb.PhaseSeat != 0 {
		t.Fatalf("phase %s seat %d", tb.Phase, tb.PhaseSeat)
	}
	mustCode(t, tb.Draw(0, 0), "NOT_YOUR_TURN")
	mustCode(t, tb.Swap(0, 0, 0), "BAD_TARGET")
	mustOK(t, tb.Swap(0, 1, 0))
	if len(tb.Players[0].Hand) != 1 || len(tb.Players[1].Hand) != 2 || tb.Turn != 1 {
		t.Fatalf("hands %d %d turn %d", len(tb.Players[0].Hand), len(tb.Players[1].Hand), tb.Turn)
	}
	// With one opponent the swap happens straight away; a timeout takes the smallest hand.
	t2 := table(Format1v1, num(ColorRed, 5), ColorRed, []Card{num(ColorRed, 7), num(ColorRed, 1)}, []Card{num(ColorBlue, 1), num(ColorBlue, 2), num(ColorBlue, 3)})
	mustOK(t, t2.Play(0, t2.Players[0].Hand[0].ID, "", 0))
	if len(t2.Players[0].Hand) != 3 || len(t2.Players[1].Hand) != 1 {
		t.Fatalf("1v1 swap: %d %d", len(t2.Players[0].Hand), len(t2.Players[1].Hand))
	}
	t3 := table(Format4P, num(ColorRed, 5), ColorRed, []Card{num(ColorRed, 7), num(ColorRed, 1), num(ColorRed, 2)}, []Card{num(ColorBlue, 1), num(ColorBlue, 2)},
		[]Card{num(ColorGreen, 1)}, []Card{num(ColorYellow, 1), num(ColorYellow, 2), num(ColorYellow, 3)})
	mustOK(t, t3.Play(0, t3.Players[0].Hand[0].ID, "", 0))
	if seat, ok := t3.Timeout(t3.Deadline, newRNG(1)); !ok || seat != 0 || len(t3.Players[0].Hand) != 1 || len(t3.Players[2].Hand) != 2 {
		t.Fatalf("timeout swap: %v %d hands %d %d", ok, seat, len(t3.Players[0].Hand), len(t3.Players[2].Hand))
	}
}

func TestMercyLimit(t *testing.T) {
	var big []Card
	for i := 0; i < 20; i++ {
		big = append(big, num(ColorGreen, 1))
	}
	w10 := card(ColorWild, KindWild10, 0)
	tb := table(Format4P, num(ColorRed, 5), ColorRed, []Card{w10, num(ColorRed, 1)}, big, []Card{num(ColorBlue, 1)}, []Card{num(ColorYellow, 1)})
	mustOK(t, tb.Play(0, w10.ID, ColorRed, 0))
	// Seat 1 cannot answer the +10: takes it (30 cards) and is out.
	mustOK(t, tb.Draw(1, 0))
	tb.ApplyMercy(0)
	p := tb.Players[1]
	if !p.Removed || p.RemovedReason != "mercy" || len(p.Hand) != 0 || tb.Over {
		t.Fatalf("removed %v reason %q hand %d over %v", p.Removed, p.RemovedReason, len(p.Hand), tb.Over)
	}
	if tb.Turn == 1 {
		t.Fatal("turn stayed on an eliminated player")
	}
	// The last player standing wins.
	t2 := table(Format1v1, num(ColorRed, 5), ColorRed, []Card{w10, num(ColorRed, 1)}, big)
	mustOK(t, t2.Play(0, w10.ID, ColorRed, 0))
	mustOK(t, t2.Draw(1, 0))
	t2.ApplyMercy(0)
	if !t2.Over || t2.Winner != 0 || t2.OverReason != "last_standing" {
		t.Fatalf("over %v winner %d reason %s", t2.Over, t2.Winner, t2.OverReason)
	}
	res := ComputeResult(t2, 400)
	for _, r := range res {
		if r.Seat == 1 && (r.Coins != -400 || !r.Out || r.Removed || r.Rank != 2) {
			t.Fatalf("out player result %+v", r)
		}
	}
	// Classic has no limit.
	t3 := table(Format1v1, num(ColorRed, 5), ColorRed, []Card{num(ColorRed, 1)}, append(append([]Card{}, big...), big...))
	t3.Mode = ModeClassic
	t3.ApplyMercy(0)
	if t3.Players[1].Removed {
		t.Fatal("classic eliminated a player")
	}
}

func TestMatchClockFewestPointsWins(t *testing.T) {
	tb := table(Format4P, num(ColorRed, 5), ColorRed,
		[]Card{num(ColorRed, 9), num(ColorRed, 8)}, []Card{num(ColorBlue, 1), num(ColorBlue, 2)}, []Card{card(ColorWild, KindWild4, 0)}, []Card{num(ColorGreen, 9)})
	tb.EndsAt = 1000
	if tb.CheckClock(999) {
		t.Fatal("clock ended early")
	}
	if !tb.CheckClock(1000) || !tb.Over || tb.Winner != 1 || tb.OverReason != "time_up" {
		t.Fatalf("over %v winner %d reason %s", tb.Over, tb.Winner, tb.OverReason)
	}
	if MatchClockMs(ModeClassic, 0) != 180000 || MatchClockMs(ModeNoMercy, 0) != 300000 || MatchClockMs(ModeClassic, 10) != 600000 {
		t.Fatal("match lengths")
	}
}
