package tango

import "testing"

// Room house rules: Jump-In and 7-0 in Classic.

func TestJumpIn(t *testing.T) {
	r5a, r5b := num(ColorRed, 5), num(ColorRed, 5)
	tb := table(Format4P, num(ColorRed, 3), ColorRed,
		[]Card{r5a, num(ColorBlue, 1)}, []Card{num(ColorBlue, 2), num(ColorBlue, 7)}, []Card{r5b, num(ColorGreen, 4), num(ColorGreen, 6)}, []Card{num(ColorBlue, 3)})
	tb.Mode = ModeClassic
	// Off by default: playing out of turn is refused.
	if len(tb.JumpInCards(2)) != 0 {
		t.Fatal("jump-in offered with the rule off")
	}
	tb.JumpIn = true
	mustOK(t, tb.Play(0, r5a.ID, "", 0)) // red 5 on top, seat 1 to play
	if tb.Turn != 1 {
		t.Fatalf("turn %d", tb.Turn)
	}
	got := tb.JumpInCards(2)
	if len(got) != 1 || got[0] != r5b.ID {
		t.Fatalf("jump-in cards %v", got)
	}
	// A card that is only the same colour or the same number is not identical.
	if len(tb.JumpInCards(1)) != 0 {
		t.Fatal("seat 1 is on turn")
	}
	mustCode(t, tb.JumpInPlay(2, tb.Players[2].Hand[1].ID, 0), "NOT_YOUR_TURN")
	mustOK(t, tb.JumpInPlay(2, r5b.ID, 0))
	// Play continues from the player who jumped in.
	if tb.Turn != 3 || len(tb.Players[2].Hand) != 2 || tb.Top().ID != r5b.ID {
		t.Fatalf("turn %d hand %d top %v", tb.Turn, len(tb.Players[2].Hand), tb.Top())
	}
}

func TestJumpInNotIntoStack(t *testing.T) {
	r2a, r2b := card(ColorRed, KindDraw2, 0), card(ColorRed, KindDraw2, 0)
	tb := table(Format4P, num(ColorRed, 3), ColorRed,
		[]Card{r2a, num(ColorBlue, 1)}, []Card{card(ColorWild, KindWild4, 0), num(ColorBlue, 2)}, []Card{r2b, num(ColorGreen, 4)}, []Card{num(ColorBlue, 3)})
	tb.Mode, tb.JumpIn = ModeClassic, true
	mustOK(t, tb.Play(0, r2a.ID, "", 0))
	if tb.Pending == 0 || len(tb.JumpInCards(2)) != 0 {
		t.Fatalf("pending %d jump-in %v", tb.Pending, tb.JumpInCards(2))
	}
}

func TestSevenOInClassicRoom(t *testing.T) {
	r7 := num(ColorRed, 7)
	tb := table(Format1v1, num(ColorRed, 3), ColorRed, []Card{r7, num(ColorBlue, 1)}, []Card{num(ColorBlue, 2), num(ColorBlue, 4), num(ColorGreen, 5)})
	tb.Mode = ModeClassic
	mustOK(t, tb.Play(0, r7.ID, "", 0))
	if len(tb.Players[0].Hand) != 1 {
		t.Fatal("classic 7 swapped without the 7-0 rule")
	}
	r7b := num(ColorBlue, 7)
	tb = table(Format1v1, num(ColorBlue, 3), ColorBlue, []Card{r7b, num(ColorBlue, 1)}, []Card{num(ColorBlue, 2), num(ColorBlue, 4), num(ColorGreen, 5)})
	tb.Mode, tb.SevenO = ModeClassic, true
	mustOK(t, tb.Play(0, r7b.ID, "", 0))
	// With one opponent the swap is automatic.
	if len(tb.Players[0].Hand) != 3 || len(tb.Players[1].Hand) != 1 {
		t.Fatalf("hands %d / %d after 7 with 7-0", len(tb.Players[0].Hand), len(tb.Players[1].Hand))
	}
}
