package tango

import (
	"math/rand"
	"testing"
)

type testRNG struct{ r *rand.Rand }

func (t testRNG) RandInt(lo, hi int) int {
	if hi <= lo {
		return lo
	}
	return lo + t.r.Intn(hi-lo+1)
}

func newRNG(seed int64) testRNG { return testRNG{rand.New(rand.NewSource(seed))} }

var nextID = 1000

func card(color, kind string, value int) Card {
	nextID++
	return Card{ID: nextID, Color: color, Kind: kind, Value: value}
}

func num(color string, v int) Card { return card(color, KindNumber, v) }

// table builds a two- or four-player table with given hands, top card and
// colour, and starts seat 0's turn.
func table(format string, top Card, color string, hands ...[]Card) *Table {
	var ps []*Player
	for i, h := range hands {
		ps = append(ps, &Player{Seat: i, UserID: string(rune('a' + i)), Team: TeamOf(format, i), Hand: append([]Card{}, h...)})
	}
	var pile []Card
	for i := 0; i < 40; i++ {
		pile = append(pile, num(ColorYellow, 9)) // filler: never playable on test tops below unless noted
	}
	t := &Table{Mode: ModeNoMercy, Format: format, TurnMs: 15000, Players: ps, Pile: pile, Discard: []Card{top},
		Color: color, Dir: 1, Winner: -1, WinTeam: -1, rng: newRNG(1)}
	t.beginTurn(0, 0)
	return t
}

func mustOK(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func mustCode(t *testing.T, err error, code string) {
	t.Helper()
	if ErrorCode(err) != code {
		t.Fatalf("want %s, got %v", code, err)
	}
}

func TestDeckSizes(t *testing.T) {
	if n := len(NewDeck(ModeClassic)); n != 108 {
		t.Fatalf("classic deck %d", n)
	}
	if n := len(NewDeck(ModeNoMercy)); n != 114 {
		t.Fatalf("no mercy deck %d", n)
	}
	seen := map[int]bool{}
	for _, c := range NewDeck(ModeNoMercy) {
		if seen[c.ID] {
			t.Fatalf("duplicate id %d", c.ID)
		}
		seen[c.ID] = true
	}
}

func TestPoints(t *testing.T) {
	h := []Card{num(ColorRed, 7), card(ColorBlue, KindSkip, 0), card(ColorWild, KindWild6, 0)}
	if p := handPoints(h); p != 7+20+50 {
		t.Fatalf("points %d", p)
	}
}

func TestLegalMatching(t *testing.T) {
	tb := table(Format1v1, num(ColorRed, 5), ColorRed, []Card{num(ColorBlue, 5), num(ColorBlue, 6), num(ColorRed, 1), card(ColorWild, KindWild, 0), card(ColorBlue, KindSkip, 0)}, []Card{num(ColorGreen, 1)})
	got := map[int]bool{}
	for _, id := range tb.Playable(0) {
		got[id] = true
	}
	h := tb.Players[0].Hand
	if !got[h[0].ID] || got[h[1].ID] || !got[h[2].ID] || !got[h[3].ID] || got[h[4].ID] {
		t.Fatalf("playable wrong: %v", tb.Playable(0))
	}
	mustCode(t, tb.Play(0, h[1].ID, "", 0), "ILLEGAL_CARD")
	mustCode(t, tb.Play(1, tb.Players[1].Hand[0].ID, "", 0), "NOT_YOUR_TURN")
}

func TestActionOnActionMatches(t *testing.T) {
	tb := table(Format1v1, card(ColorRed, KindSkip, 0), ColorRed, []Card{card(ColorBlue, KindSkip, 0), num(ColorGreen, 2)}, []Card{num(ColorGreen, 1), num(ColorGreen, 3)})
	mustOK(t, tb.Play(0, tb.Players[0].Hand[0].ID, "", 0))
	// Skip in 1v1: same player again.
	if tb.Turn != 0 || tb.Color != ColorBlue {
		t.Fatalf("turn %d colour %s", tb.Turn, tb.Color)
	}
}

func TestReverseActsAsSkipWithTwo(t *testing.T) {
	tb := table(Format1v1, num(ColorRed, 5), ColorRed, []Card{card(ColorRed, KindReverse, 0), num(ColorGreen, 2)}, []Card{num(ColorGreen, 1), num(ColorGreen, 3)})
	mustOK(t, tb.Play(0, tb.Players[0].Hand[0].ID, "", 0))
	if tb.Turn != 0 {
		t.Fatalf("reverse with two players should come back, turn=%d", tb.Turn)
	}
}

func TestReverseFourPlayers(t *testing.T) {
	g := []Card{num(ColorGreen, 1), num(ColorGreen, 2)}
	tb := table(Format2v2, num(ColorRed, 5), ColorRed, []Card{card(ColorRed, KindReverse, 0), num(ColorGreen, 2)}, g, g, g)
	mustOK(t, tb.Play(0, tb.Players[0].Hand[0].ID, "", 0))
	if tb.Turn != 3 || tb.Dir != -1 {
		t.Fatalf("turn %d dir %d", tb.Turn, tb.Dir)
	}
}

func TestStackingRules(t *testing.T) {
	w4 := card(ColorWild, KindWild4, 0)
	green2 := card(ColorGreen, KindDraw2, 0)
	yellow2 := card(ColorYellow, KindDraw2, 0)
	w6 := card(ColorWild, KindWild6, 0)
	tb := table(Format1v1, num(ColorRed, 5), ColorRed,
		[]Card{w4, num(ColorRed, 1), num(ColorRed, 2)},
		[]Card{green2, yellow2, w6, num(ColorBlue, 1)})
	mustOK(t, tb.Play(0, w4.ID, ColorGreen, 0))
	if tb.Pending != 4 || tb.Turn != 1 {
		t.Fatalf("pending %d turn %d", tb.Pending, tb.Turn)
	}
	// Yellow +2 does not match green; green +2 does; a black plus always does.
	mustCode(t, tb.Play(1, yellow2.ID, "", 0), "MUST_STACK_OR_DRAW")
	mustCode(t, tb.Play(1, tb.Players[1].Hand[3].ID, "", 0), "MUST_STACK_OR_DRAW")
	mustOK(t, tb.Play(1, green2.ID, "", 0))
	// Seat 0 has nothing to stack and no challenge (stack started with +4 by
	// seat 0 itself, and +2 closed it): draws 6 automatically.
	if len(tb.Players[0].Hand) != 2+6 || tb.Pending != 0 || tb.Turn != 1 {
		t.Fatalf("auto-draw failed: hand=%d pending=%d turn=%d", len(tb.Players[0].Hand), tb.Pending, tb.Turn)
	}
}

func TestBlackPlusOnAnyPlus(t *testing.T) {
	r2 := card(ColorRed, KindDraw2, 0)
	w10 := card(ColorWild, KindWild10, 0)
	tb := table(Format1v1, num(ColorRed, 5), ColorRed, []Card{r2, num(ColorRed, 1)}, []Card{w10, num(ColorBlue, 1)})
	mustOK(t, tb.Play(0, r2.ID, "", 0))
	mustOK(t, tb.Play(1, w10.ID, ColorBlue, 0))
	if tb.Players[0].Hand[0].ID == 0 || tb.Pending != 0 {
		t.Fatal("expected auto draw")
	}
	if len(tb.Players[0].Hand) != 1+12 {
		t.Fatalf("seat0 hand %d", len(tb.Players[0].Hand))
	}
	// Not the first plus of the stack, so it was never challengeable.
}

func TestChallengeGuilty(t *testing.T) {
	w4 := card(ColorWild, KindWild4, 0)
	tb := table(Format1v1, num(ColorRed, 5), ColorRed, []Card{w4, num(ColorRed, 1), num(ColorBlue, 3)}, []Card{num(ColorBlue, 1), num(ColorGreen, 2)})
	mustOK(t, tb.Play(0, w4.ID, ColorBlue, 0))
	if !tb.CanChallenge(1) {
		t.Fatal("first wild plus must be challengeable")
	}
	mustOK(t, tb.ChallengeTop(1, 0))
	if len(tb.Players[0].Hand) != 2+4 || len(tb.Players[1].Hand) != 2 || tb.Turn != 1 || tb.Pending != 0 {
		t.Fatalf("guilty: h0=%d h1=%d turn=%d", len(tb.Players[0].Hand), len(tb.Players[1].Hand), tb.Turn)
	}
}

func TestChallengeInnocent(t *testing.T) {
	w4 := card(ColorWild, KindWild4, 0)
	tb := table(Format1v1, num(ColorRed, 5), ColorRed, []Card{w4, num(ColorBlue, 1), num(ColorBlue, 3)}, []Card{num(ColorYellow, 1), num(ColorGreen, 2)})
	mustOK(t, tb.Play(0, w4.ID, ColorBlue, 0))
	mustOK(t, tb.ChallengeTop(1, 0))
	if len(tb.Players[1].Hand) != 2+6 || tb.Turn != 0 {
		t.Fatalf("innocent: h1=%d turn=%d", len(tb.Players[1].Hand), tb.Turn)
	}
}

func TestChallengeOnlyFirstOfStack(t *testing.T) {
	r2 := card(ColorRed, KindDraw2, 0)
	w4 := card(ColorWild, KindWild4, 0)
	w4b := card(ColorWild, KindWild4, 0)
	tb := table(Format1v1, num(ColorRed, 5), ColorRed, []Card{r2, w4b, num(ColorBlue, 1)}, []Card{w4, num(ColorBlue, 1), num(ColorBlue, 2)})
	mustOK(t, tb.Play(0, r2.ID, "", 0))
	mustOK(t, tb.Play(1, w4.ID, ColorGreen, 0))
	if tb.CanChallenge(0) {
		t.Fatal("a wild plus on an existing stack must not be challengeable")
	}
	mustCode(t, tb.ChallengeTop(0, 0), "CANNOT_CHALLENGE")
	mustOK(t, tb.Play(0, w4b.ID, ColorBlue, 0))
	// Seat 1 cannot stack or challenge: draws the whole 10 at once.
	if len(tb.Players[1].Hand) != 2+10 || tb.Pending != 0 || tb.Turn != 0 {
		t.Fatalf("hand %d pending %d turn %d", len(tb.Players[1].Hand), tb.Pending, tb.Turn)
	}
}

func TestDrawNeverPenalises(t *testing.T) {
	tb := table(Format1v1, num(ColorRed, 5), ColorRed, []Card{num(ColorRed, 1), num(ColorBlue, 2)}, []Card{num(ColorBlue, 1), num(ColorBlue, 7)})
	before := len(tb.Players[0].Hand)
	mustOK(t, tb.Draw(0, 0)) // filler yellow 9 does not fit red 5
	if len(tb.Players[0].Hand) != before+1 || tb.Turn != 1 {
		t.Fatalf("draw added %d, turn %d", len(tb.Players[0].Hand)-before, tb.Turn)
	}
}

func TestDrawnPlayableAutoPlays(t *testing.T) {
	tb := table(Format1v1, num(ColorYellow, 5), ColorYellow, []Card{num(ColorRed, 1), num(ColorBlue, 2)}, []Card{num(ColorBlue, 1), num(ColorBlue, 7)})
	before := len(tb.Players[0].Hand)
	mustOK(t, tb.Draw(0, 0)) // yellow 9 fits yellow 5
	if len(tb.Players[0].Hand) != before || tb.Top().Value != 9 || tb.Turn != 1 {
		t.Fatalf("auto play failed: hand=%d top=%v", len(tb.Players[0].Hand), tb.Top())
	}
}

func TestDrawnWildPlusOffersKeepOrPlay(t *testing.T) {
	tb := table(Format1v1, num(ColorRed, 5), ColorRed, []Card{num(ColorBlue, 1), num(ColorBlue, 2)}, []Card{num(ColorBlue, 3), num(ColorBlue, 7)})
	w6 := card(ColorWild, KindWild6, 0)
	tb.Pile = append(tb.Pile, w6)
	mustOK(t, tb.Draw(0, 0))
	if tb.Phase != PhaseKeep || tb.Drawn == nil || tb.Drawn.ID != w6.ID {
		t.Fatalf("expected keep phase, got %s", tb.Phase)
	}
	mustOK(t, tb.KeepOrPlay(0, true, ColorGreen, 0))
	if tb.Pending != 6 || tb.Color != ColorGreen || tb.Turn != 1 {
		t.Fatalf("pending %d colour %s turn %d", tb.Pending, tb.Color, tb.Turn)
	}
}

func TestKeepDrawnWildPlus(t *testing.T) {
	tb := table(Format1v1, num(ColorRed, 5), ColorRed, []Card{num(ColorBlue, 1), num(ColorBlue, 2)}, []Card{num(ColorBlue, 3), num(ColorBlue, 7)})
	w4 := card(ColorWild, KindWild4, 0)
	tb.Pile = append(tb.Pile, w4)
	mustOK(t, tb.Draw(0, 0))
	mustOK(t, tb.KeepOrPlay(0, false, "", 0))
	if len(tb.Players[0].Hand) != 3 || tb.Turn != 1 || tb.Pending != 0 {
		t.Fatal("keep failed")
	}
}

func TestWildColourChosenLater(t *testing.T) {
	w := card(ColorWild, KindWild, 0)
	tb := table(Format1v1, num(ColorRed, 5), ColorRed, []Card{w, num(ColorBlue, 2), num(ColorBlue, 3)}, []Card{num(ColorBlue, 3)})
	mustOK(t, tb.Play(0, w.ID, "", 0))
	if tb.Phase != PhaseColor || tb.PhaseSeat != 0 {
		t.Fatalf("phase %s", tb.Phase)
	}
	mustCode(t, tb.ChooseColor(0, "purple", 0), "BAD_COLOR")
	mustOK(t, tb.ChooseColor(0, ColorBlue, 0))
	if tb.Color != ColorBlue || tb.Turn != 1 {
		t.Fatal("colour not applied")
	}
}

func TestTangoAndHammer(t *testing.T) {
	a, b := num(ColorRed, 1), num(ColorRed, 2)
	tb := table(Format1v1, num(ColorRed, 5), ColorRed, []Card{a, b}, []Card{num(ColorBlue, 3), num(ColorBlue, 4), num(ColorBlue, 5)})
	mustOK(t, tb.Play(0, a.ID, "", 0))
	if !tb.Players[0].Vulnerable {
		t.Fatal("should be vulnerable without TANGO!")
	}
	mustCode(t, tb.Hammer(0, 0), "NOT_AN_OPPONENT")
	mustOK(t, tb.Hammer(1, 0))
	if len(tb.Players[0].Hand) != 3 || tb.Players[0].Vulnerable {
		t.Fatalf("hammer: hand %d", len(tb.Players[0].Hand))
	}
	// A second tap misses and costs nothing.
	mustOK(t, tb.Hammer(1, 0))
	if len(tb.Players[1].Hand) != 3 || len(tb.Players[0].Hand) != 3 {
		t.Fatal("miss must not penalise anyone")
	}
}

func TestTangoProtects(t *testing.T) {
	a, b := num(ColorRed, 1), num(ColorRed, 2)
	tb := table(Format1v1, num(ColorRed, 5), ColorRed, []Card{a, b}, []Card{num(ColorBlue, 3), num(ColorBlue, 4), num(ColorBlue, 5)})
	mustCode(t, tb.PressTango(1), "TOO_MANY_CARDS")
	mustOK(t, tb.PressTango(0))
	mustOK(t, tb.Play(0, a.ID, "", 0))
	if tb.Players[0].Vulnerable {
		t.Fatal("TANGO! pressed: not vulnerable")
	}
	mustOK(t, tb.Hammer(1, 0))
	if len(tb.Players[0].Hand) != 1 {
		t.Fatal("protected player was hammered")
	}
}

func TestTangoAfterOneCard(t *testing.T) {
	a, b := num(ColorRed, 1), num(ColorRed, 2)
	tb := table(Format1v1, num(ColorRed, 5), ColorRed, []Card{a, b}, []Card{num(ColorBlue, 3), num(ColorBlue, 4)})
	mustOK(t, tb.Play(0, a.ID, "", 0))
	mustOK(t, tb.PressTango(0))
	mustOK(t, tb.Hammer(1, 0))
	if len(tb.Players[0].Hand) != 1 {
		t.Fatal("late TANGO! should still protect before the hammer")
	}
}

func TestWinEndsRound(t *testing.T) {
	a := num(ColorRed, 1)
	tb := table(Format1v1, num(ColorRed, 5), ColorRed, []Card{a}, []Card{num(ColorBlue, 3), card(ColorWild, KindWild4, 0)})
	mustOK(t, tb.Play(0, a.ID, "", 0))
	if !tb.Over || tb.Winner != 0 || tb.WinTeam != 0 || tb.Players[1].FinalPoints != 53 {
		t.Fatalf("over=%v winner=%d pts=%d", tb.Over, tb.Winner, tb.Players[1].FinalPoints)
	}
	mustCode(t, tb.Draw(1, 0), "GAME_OVER")
}

func TestRemoveForfeits(t *testing.T) {
	tb := table(Format1v1, num(ColorRed, 5), ColorRed, []Card{num(ColorRed, 1), num(ColorRed, 2)}, []Card{num(ColorBlue, 3)})
	tb.Remove(0, "left", newRNG(2), 0)
	if !tb.Over || tb.WinTeam != 1 || tb.OverReason != "forfeit" {
		t.Fatalf("over=%v team=%d", tb.Over, tb.WinTeam)
	}
}

func TestRemoveInTeamsContinues(t *testing.T) {
	g := []Card{num(ColorGreen, 1), num(ColorGreen, 2)}
	tb := table(Format2v2, num(ColorRed, 5), ColorRed, g, g, g, g)
	tb.Remove(0, "left", newRNG(2), 0)
	if tb.Over || tb.Turn != 1 {
		t.Fatalf("over=%v turn=%d", tb.Over, tb.Turn)
	}
	tb.Remove(2, "left", newRNG(2), 0)
	if !tb.Over || tb.WinTeam != 1 {
		t.Fatal("team 0 gone: team 1 wins")
	}
}

func TestTimeoutDraws(t *testing.T) {
	tb := table(Format1v1, num(ColorRed, 5), ColorRed, []Card{num(ColorBlue, 1), num(ColorBlue, 2)}, []Card{num(ColorBlue, 3)})
	if _, fired := tb.Timeout(100, newRNG(3)); fired {
		t.Fatal("too early")
	}
	seat, fired := tb.Timeout(15000, newRNG(3))
	if !fired || seat != 0 || len(tb.Players[0].Hand) != 3 || tb.Turn != 1 {
		t.Fatal("timeout should draw and pass")
	}
}

// TestRandomGamesConserveCards plays thousands of games with random legal
// actions in every mode and format and checks invariants after each step.
func TestRandomGamesConserveCards(t *testing.T) {
	for _, mode := range []string{ModeClassic, ModeNoMercy} {
		for _, format := range []string{Format1v1, Format2v2, Format4v4} {
			for seed := int64(1); seed <= 300; seed++ {
				playRandom(t, mode, format, seed)
			}
		}
	}
}

func playRandom(t *testing.T, mode, format string, seed int64) {
	rng := newRNG(seed)
	n := FormatPlayers(format)
	var ps []*Player
	for i := 0; i < n; i++ {
		ps = append(ps, &Player{Seat: i, Team: TeamOf(format, i)})
	}
	tb := NewTable(mode, format, 15000, ps, rng, 0)
	total := len(NewDeck(mode))
	now := int64(0)
	for step := 0; step < 5000 && !tb.Over; step++ {
		now += 100
		seat := tb.PhaseSeat
		switch tb.Phase {
		case PhaseColor:
			mustOK(t, tb.ChooseColor(seat, Colors[rng.RandInt(0, 3)], now))
		case PhaseKeep:
			mustOK(t, tb.KeepOrPlay(seat, rng.RandInt(0, 1) == 0, "", now))
		case PhaseTurn:
			pl := tb.Playable(seat)
			r := rng.RandInt(0, 9)
			switch {
			case r == 0 && tb.CanChallenge(seat):
				mustOK(t, tb.ChallengeTop(seat, now))
			case r <= 1:
				mustOK(t, tb.Draw(seat, now))
			case len(pl) > 0:
				col := ""
				if rng.RandInt(0, 1) == 0 {
					col = Colors[rng.RandInt(0, 3)]
				}
				mustOK(t, tb.Play(seat, pl[rng.RandInt(0, len(pl)-1)], col, now))
			default:
				mustOK(t, tb.Draw(seat, now))
			}
		}
		if len(tb.Players[seat].Hand) <= 2 && rng.RandInt(0, 1) == 0 {
			_ = tb.PressTango(seat)
		}
		for _, p := range tb.Players {
			if p.Vulnerable && rng.RandInt(0, 3) == 0 {
				_ = tb.Hammer((p.Seat+1)%n, p.Seat)
			}
		}
		if step == 300 && n > 2 && rng.RandInt(0, 3) == 0 {
			tb.Remove(rng.RandInt(0, n-1), "left", rng, now)
		}
		tb.TakeEvents()
		count := len(tb.Pile) + len(tb.Discard)
		for _, p := range tb.Players {
			count += len(p.Hand)
		}
		if count != total {
			t.Fatalf("%s %s seed %d step %d: card count %d want %d", mode, format, seed, step, count, total)
		}
		if !tb.Over && tb.Players[tb.PhaseSeat].Removed {
			t.Fatalf("%s %s seed %d: phase seat %d removed", mode, format, seed, tb.PhaseSeat)
		}
	}
}
