package tango

import "testing"

// The No Mercy cards and rules as the game describes them in HOW TO PLAY.

func TestNoMercyDeck(t *testing.T) {
	count := map[string]int{}
	for _, c := range NewDeck(ModeNoMercy) {
		count[c.Kind]++
	}
	// The official 168-card deck.
	want := map[string]int{KindNumber: 80, KindSkip: 12, KindReverse: 12, KindDraw2: 12, KindDiscardAll: 12, KindDraw4: 8,
		KindSkipAll: 8, KindWildRev4: 8, KindWild6: 4, KindWild10: 4, KindRoulette: 8}
	for k, n := range want {
		if count[k] != n {
			t.Fatalf("%s: %d want %d", k, count[k], n)
		}
	}
	if count[KindWild] != 0 || count[KindWild4] != 0 || len(NewDeck(ModeClassic)) != 108 {
		t.Fatalf("plain wilds %d, plain wild +4 %d, classic %d", count[KindWild], count[KindWild4], len(NewDeck(ModeClassic)))
	}
}

// Official stacking: any draw card of equal or higher value, any colour.
func TestColouredPlusFourStacks(t *testing.T) {
	r4, b4 := card(ColorRed, KindDraw4, 0), card(ColorBlue, KindDraw4, 0)
	tb := table(Format4P, num(ColorRed, 3), ColorRed,
		[]Card{r4, num(ColorRed, 1)}, []Card{card(ColorRed, KindDraw2, 0), b4, num(ColorGreen, 1)}, []Card{num(ColorBlue, 2)}, []Card{num(ColorBlue, 3)})
	mustOK(t, tb.Play(0, r4.ID, "", 0))
	if tb.Pending != 4 || tb.Turn != 1 {
		t.Fatalf("pending %d turn %d", tb.Pending, tb.Turn)
	}
	// A red +2 is lower than +4: refused even in the colour in play; a blue +4 (equal) answers.
	red2 := tb.Players[1].Hand[0]
	mustCode(t, tb.Play(1, red2.ID, "", 0), "MUST_STACK_OR_DRAW")
	mustOK(t, tb.Play(1, b4.ID, "", 0))
	if tb.Color != ColorBlue {
		t.Fatalf("colour %s after a blue +4", tb.Color)
	}
	// Seat 2 cannot answer the +8: takes all eight automatically, seat 3 plays next.
	if len(tb.Players[2].Hand) != 9 || tb.Turn != 3 {
		t.Fatalf("seat 2 hand %d, turn %d", len(tb.Players[2].Hand), tb.Turn)
	}
}

func TestStackEqualOrHigher(t *testing.T) {
	w6, w10, rev4 := card(ColorWild, KindWild6, 0), card(ColorWild, KindWild10, 0), card(ColorWild, KindWildRev4, 0)
	g2 := card(ColorGreen, KindDraw2, 0)
	tb := table(Format1v1, num(ColorRed, 3), ColorRed, []Card{g2, w10, num(ColorRed, 1)}, []Card{w6, rev4, num(ColorBlue, 1)})
	// Any colour of +2 starts nothing special: green +2 on red needs a colour/kind match, so play w10 instead.
	mustOK(t, tb.Play(0, w10.ID, ColorGreen, 0))
	// +6 and +4 are lower than +10: refused, so seat 1 has nothing and draws 10 at once.
	if len(tb.Players[1].Hand) != 3+10 || tb.Pending != 0 || tb.Turn != 0 {
		t.Fatalf("seat 1 hand %d pending %d turn %d", len(tb.Players[1].Hand), tb.Pending, tb.Turn)
	}
	if tb.Chal != nil || tb.CanChallenge(1) {
		t.Fatal("No Mercy has no challenge")
	}
}

func TestWildReverseDrawFour(t *testing.T) {
	rev4 := card(ColorWild, KindWildRev4, 0)
	tb := table(Format4P, num(ColorRed, 3), ColorRed, []Card{rev4, num(ColorRed, 1)}, []Card{num(ColorBlue, 1)}, []Card{num(ColorBlue, 2)}, []Card{num(ColorBlue, 3)})
	mustOK(t, tb.Play(0, rev4.ID, ColorYellow, 0))
	// Direction reverses first, so the player before (seat 3) draws 4 and seat 2 plays.
	if tb.Dir != -1 || len(tb.Players[3].Hand) != 1+4 || tb.Turn != 2 || tb.Color != ColorYellow {
		t.Fatalf("dir %d seat3 %d turn %d colour %s", tb.Dir, len(tb.Players[3].Hand), tb.Turn, tb.Color)
	}
}

func TestColorRoulette(t *testing.T) {
	roul := card(ColorWild, KindRoulette, 0)
	tb := table(Format4P, num(ColorRed, 3), ColorRed, []Card{roul, num(ColorRed, 1)}, []Card{num(ColorBlue, 1)}, []Card{num(ColorBlue, 2)}, []Card{num(ColorBlue, 3)})
	// Pile (top is the end): ..., yellow 9 filler, then green 4, wild, red 2 on top.
	tb.Pile = append(tb.Pile, num(ColorGreen, 4), card(ColorWild, KindWild6, 0), num(ColorRed, 2))
	mustOK(t, tb.Play(0, roul.ID, "", 0))
	if tb.Phase != PhaseRoulette || tb.PhaseSeat != 1 {
		t.Fatalf("phase %s seat %d", tb.Phase, tb.PhaseSeat)
	}
	mustCode(t, tb.ChooseColor(0, ColorGreen, 0), "NOT_YOUR_TURN") // the victim names the colour
	mustOK(t, tb.ChooseColor(1, ColorGreen, 0))
	// Turned red 2, wild +6 (black doesn't count), green 4: keeps all three, loses the turn.
	if len(tb.Players[1].Hand) != 1+3 || tb.Color != ColorGreen || tb.Turn != 2 {
		t.Fatalf("victim hand %d colour %s turn %d", len(tb.Players[1].Hand), tb.Color, tb.Turn)
	}
}

func TestDrawUntilPlayable(t *testing.T) {
	tb := table(Format1v1, num(ColorRed, 5), ColorRed, []Card{num(ColorBlue, 1), num(ColorBlue, 2)}, []Card{num(ColorBlue, 3)})
	// Three yellow 9s (not playable) come off first, then a red 8.
	tb.Pile = append(tb.Pile, num(ColorRed, 8), num(ColorYellow, 9), num(ColorYellow, 9), num(ColorYellow, 9))
	mustOK(t, tb.Draw(0, 0))
	// Drew four, the red 8 fitted and was played: three stay in hand.
	if len(tb.Players[0].Hand) != 2+3 || tb.Top().Value != 8 || tb.Turn != 1 {
		t.Fatalf("hand %d top %v turn %d", len(tb.Players[0].Hand), tb.Top(), tb.Turn)
	}
	// Stops at the Mercy limit: drawing on with nothing playable puts the player out.
	t2 := table(Format4P, num(ColorRed, 5), ColorRed, []Card{num(ColorBlue, 1)}, []Card{num(ColorBlue, 2)}, []Card{num(ColorBlue, 3)}, []Card{num(ColorBlue, 4)})
	mustOK(t, t2.Draw(0, 0))
	t2.ApplyMercy(0)
	if !t2.Players[0].Removed || t2.Players[0].RemovedReason != "mercy" {
		t.Fatalf("drawing to 25 must knock out: removed %v hand %d", t2.Players[0].Removed, len(t2.Players[0].Hand))
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
