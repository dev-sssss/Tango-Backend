// Package tango is the authoritative Tango Cards game: decks, rules, turn
// flow, bots, payouts and the match handler. Clients only send intents
// (play this card, draw, press TANGO!) and render the state the server sends
// back; every rule is decided here.
package tango

import "fmt"

// Colours. Wild cards carry ColorWild until played; the chosen colour is
// kept in Game.Color, never written onto the card.
const (
	ColorRed    = "red"
	ColorYellow = "yellow"
	ColorGreen  = "green"
	ColorBlue   = "blue"
	ColorWild   = "wild"
)

// Colors are the four playable colours.
var Colors = []string{ColorRed, ColorYellow, ColorGreen, ColorBlue}

// Card kinds.
const (
	KindNumber  = "number"
	KindSkip    = "skip"
	KindReverse = "reverse"
	KindDraw2   = "draw2"
	KindWild    = "wild"
	KindWild4   = "wild4"
	KindWild6   = "wild6"
	KindWild10  = "wild10"
	// No Mercy's coloured cards.
	KindDraw4      = "draw4"       // coloured +4
	KindSkipAll    = "skip_all"    // skips everyone else: the same player goes again
	KindDiscardAll = "discard_all" // also discards every other card of its colour from the hand
	// No Mercy's black cards besides +6 and +10.
	KindWildRev4 = "wild_rev4" // Wild Reverse Draw 4: reverses play, then the next player draws 4
	KindRoulette = "roulette"  // Wild Color Roulette: the next player names a colour and turns cards until it shows
)

// MercyLimit: in No Mercy a player who reaches this many cards is out.
const MercyLimit = 25

// Game modes.
const (
	ModeClassic  = "classic"
	ModeNoMercy  = "no_mercy"
	Format1v1    = "1v1"
	Format2v2    = "2v2"
	Format4v4    = "4v4"
	Format4P     = "4p" // four players, everyone for themselves
	Format8P     = "8p" // eight players, everyone for themselves
	HandSize     = 7
	minPlayers   = 2
	pointsAction = 20
	pointsWild   = 50
)

// Card is one physical card. IDs are unique within a match.
type Card struct {
	ID    int    `json:"id"`
	Color string `json:"color"`
	Kind  string `json:"kind"`
	Value int    `json:"value,omitempty"` // 0-9 for number cards
}

func (c Card) String() string {
	if c.Kind == KindNumber {
		return fmt.Sprintf("%s %d", c.Color, c.Value)
	}
	return c.Color + " " + c.Kind
}

// IsWild reports any black card (wild or wild plus).
func (c Card) IsWild() bool { return c.Color == ColorWild }

// IsWildPlus reports the black plus cards (+4, +6, +10).
func (c Card) IsWildPlus() bool {
	return c.Kind == KindWild4 || c.Kind == KindWildRev4 || c.Kind == KindWild6 || c.Kind == KindWild10
}

// IsPlus reports any card that adds to a draw stack.
func (c Card) IsPlus() bool { return c.Kind == KindDraw2 || c.Kind == KindDraw4 || c.IsWildPlus() }

// DrawAmount is how many cards a plus card adds to the stack.
func (c Card) DrawAmount() int {
	switch c.Kind {
	case KindDraw2:
		return 2
	case KindWild4, KindWildRev4, KindDraw4:
		return 4
	case KindWild6:
		return 6
	case KindWild10:
		return 10
	}
	return 0
}

// Points left in hand at the end: numbers at face value, coloured action
// cards 20, every black card 50.
func (c Card) Points() int {
	switch {
	case c.Kind == KindNumber:
		return c.Value
	case c.IsWild():
		return pointsWild
	default:
		return pointsAction
	}
}

func handPoints(h []Card) int {
	n := 0
	for _, c := range h {
		n += c.Points()
	}
	return n
}

// deckSpec lists how many copies of each card a mode's deck holds.
type deckSpec struct {
	zeros, perNumber, skips, reverses, draw2s int // per colour
	draw4s, skipAlls, discardAlls             int // per colour (No Mercy)
	wild, wild4, wild6, wild10                int // black cards
	wildRev4, roulettes                       int // black cards (No Mercy)
}

var deckSpecs = map[string]deckSpec{
	// 108 cards: per colour one 0, two of 1-9, two Skip, two Reverse, two +2;
	// four Wild and four Wild +4.
	ModeClassic: {zeros: 1, perNumber: 2, skips: 2, reverses: 2, draw2s: 2, wild: 4, wild4: 4},
	// No Mercy, the official 168 cards: per colour two of 0-9, three Skip,
	// three Reverse, three +2, three Discard All, two coloured +4 and two Skip
	// Everyone; eight Wild Reverse +4, four Wild +6, four Wild +10 and eight
	// Wild Color Roulette. There is no plain Wild and no plain Wild +4.
	ModeNoMercy: {zeros: 2, perNumber: 2, skips: 3, reverses: 3, draw2s: 3, draw4s: 2, skipAlls: 2, discardAlls: 3,
		wildRev4: 8, wild6: 4, wild10: 4, roulettes: 8},
}

// ValidMode reports whether mode names a playable deck.
func ValidMode(mode string) bool { _, ok := deckSpecs[mode]; return ok }

// NewDeck builds an unshuffled deck for mode with IDs starting at 1.
func NewDeck(mode string) []Card {
	spec, ok := deckSpecs[mode]
	if !ok {
		spec = deckSpecs[ModeClassic]
	}
	var d []Card
	add := func(color, kind string, value, n int) {
		for i := 0; i < n; i++ {
			d = append(d, Card{ID: len(d) + 1, Color: color, Kind: kind, Value: value})
		}
	}
	for _, col := range Colors {
		add(col, KindNumber, 0, spec.zeros)
		for v := 1; v <= 9; v++ {
			add(col, KindNumber, v, spec.perNumber)
		}
		add(col, KindSkip, 0, spec.skips)
		add(col, KindReverse, 0, spec.reverses)
		add(col, KindDraw2, 0, spec.draw2s)
		add(col, KindDiscardAll, 0, spec.discardAlls)
		add(col, KindDraw4, 0, spec.draw4s)
		add(col, KindSkipAll, 0, spec.skipAlls)
	}
	add(ColorWild, KindWild, 0, spec.wild)
	add(ColorWild, KindWild4, 0, spec.wild4)
	add(ColorWild, KindWild6, 0, spec.wild6)
	add(ColorWild, KindWild10, 0, spec.wild10)
	add(ColorWild, KindWildRev4, 0, spec.wildRev4)
	add(ColorWild, KindRoulette, 0, spec.roulettes)
	return d
}

// FormatPlayers is the seat count of a format.
func FormatPlayers(format string) int {
	switch format {
	case Format2v2:
		return 4
	case Format4v4, Format8P:
		return 8
	case Format4P:
		return 4
	}
	return 2
}

// IsTeamFormat reports a team format (2v2, 4v4): partners see each other's
// hands and win together.
func IsTeamFormat(format string) bool { return format == Format2v2 || format == Format4v4 }

// ValidFormat reports whether format is supported.
func ValidFormat(format string) bool {
	return format == Format1v1 || format == Format2v2 || format == Format4v4 || format == Format4P || format == Format8P
}

// TeamOf returns a seat's team. Teams alternate around the table so a
// player's neighbours are always opponents; in 1v1 and the free-for-all
// formats every seat is its own team.
func TeamOf(format string, seat int) int {
	if !IsTeamFormat(format) {
		return seat
	}
	return seat % 2
}

// ValidColor reports one of the four playable colours.
func ValidColor(c string) bool {
	for _, x := range Colors {
		if x == c {
			return true
		}
	}
	return false
}
