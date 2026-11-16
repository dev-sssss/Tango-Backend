package tango

// The rules engine. It is deterministic given its RNG and clock, has no I/O,
// and only ever appends events; the match handler turns events into
// per-player messages. Rules implemented (from the game design):
//
//   - Classic and No Mercy decks; 7 cards each; first discard is a number.
//   - A card is playable on the same colour, the same number, the same action,
//     or any black card.
//   - Stacking: while a draw stack is pending, a black +4/+6/+10 can go on any
//     plus; a coloured +2 only when its colour is the active colour. A player
//     with nothing to stack (and no challenge) draws the whole stack at once.
//   - Challenge: only the first black plus of a stack can be challenged, by the
//     next player. If the player who played it held the colour that was active,
//     they draw the stack and the challenger plays next; otherwise the
//     challenger draws the stack plus two and loses the turn.
//   - A player may always draw one card on their turn. Drawing never adds
//     penalty cards. A drawn card that fits plays automatically, except a drawn
//     black plus, which offers keep or play.
//   - TANGO!: a player going down to one card is safe if they pressed TANGO!
//     (at two cards or fewer). Otherwise any opponent can tap the hammer on
//     them while they hold that one card; the hammered player draws two. A
//     wrong hammer tap costs nothing.
//   - The first player to empty their hand wins for their team; one round.

import (
	"errors"
)

// Phases of the current turn.
const (
	PhaseTurn  = "turn"  // play a card, draw, or (with a stack) stack/draw/challenge
	PhaseColor = "color" // pick a colour for a black card just played
	PhaseKeep  = "keep"  // keep or play a drawn black plus
	PhaseOver  = "over"
)

// Server→client event opcodes.
const (
	OpSnapshot     int64 = 100
	OpLobby        int64 = 101
	OpGameStart    int64 = 102
	OpTurn         int64 = 103
	OpCardPlayed   int64 = 104
	OpCardsDrawn   int64 = 105
	OpColorChosen  int64 = 106
	OpTango        int64 = 107
	OpHammer       int64 = 108
	OpChallenge    int64 = 109
	OpDrawnChoice  int64 = 110
	OpPlayerStatus int64 = 111
	OpEmote        int64 = 112
	OpGameOver     int64 = 113
	OpRematch      int64 = 114
	OpError        int64 = 115
)

// RNG supplies randomness (the server's NK implements it).
type RNG interface {
	RandInt(lo, hi int) int
}

// RuleError is a rejected action. Code is stable for clients.
type RuleError struct{ Code string }

func (e *RuleError) Error() string { return e.Code }

func ruleErr(code string) error { return &RuleError{Code: code} }

// ErrorCode extracts the code of a RuleError ("INTERNAL" otherwise).
func ErrorCode(err error) string {
	var re *RuleError
	if errors.As(err, &re) {
		return re.Code
	}
	return "INTERNAL"
}

// Player is one seat. Profile fields are copied from the player's profile
// when they take the seat.
type Player struct {
	Seat     int    `json:"seat"`
	UserID   string `json:"user_id"`
	Name     string `json:"name"`
	Avatar   string `json:"avatar,omitempty"`
	Frame    string `json:"frame,omitempty"`
	Badge    string `json:"badge,omitempty"`
	CardBack string `json:"card_back,omitempty"`
	Trophies int    `json:"trophies"`
	Bot      bool   `json:"bot"`
	Team     int    `json:"team"`
	Hand     []Card `json:"hand"`

	Removed       bool   `json:"removed"`
	RemovedReason string `json:"removed_reason,omitempty"`
	TangoArmed    bool   `json:"tango_armed"`
	Vulnerable    bool   `json:"vulnerable"`
	FinalPoints   int    `json:"final_points"`
}

// Challenge describes the black plus on top of the stack.
type Challenge struct {
	Target      int    `json:"target"`       // seat that played it
	Open        bool   `json:"open"`         // first plus of its stack
	HadColor    bool   `json:"had_color"`    // target held the active colour when playing it
	ColorBefore string `json:"color_before"` // colour that was active
}

// PendingWild is a black card waiting for its colour.
type PendingWild struct {
	Seat         int    `json:"seat"`
	Card         Card   `json:"card"`
	FirstOfStack bool   `json:"first_of_stack"`
	HadColor     bool   `json:"had_color"`
	ColorBefore  string `json:"color_before"`
}

// Event is something that happened, for the match handler to send. Cards are
// visible only to Seat and Seat's teammates.
type Event struct {
	Op    int64          `json:"-"`
	Type  string         `json:"type"`
	Seat  int            `json:"seat"`
	Data  map[string]any `json:"data,omitempty"`
	Cards []Card         `json:"-"`
}

// Table is the full game state of one round.
type Table struct {
	Mode    string    `json:"mode"`
	Format  string    `json:"format"`
	TurnMs  int64     `json:"turn_ms"`
	Players []*Player `json:"players"`

	Pile    []Card `json:"pile"`    // top of the pile is the last element
	Discard []Card `json:"discard"` // top card is the last element

	Color     string       `json:"color"`
	Turn      int          `json:"turn"`
	Dir       int          `json:"dir"`
	Pending   int          `json:"pending"`
	Phase     string       `json:"phase"`
	PhaseSeat int          `json:"phase_seat"`
	Deadline  int64        `json:"deadline"`
	Drawn     *Card        `json:"drawn,omitempty"`
	Chal      *Challenge   `json:"chal,omitempty"`
	Wild      *PendingWild `json:"wild,omitempty"`

	Over       bool   `json:"over"`
	Winner     int    `json:"winner"`
	WinTeam    int    `json:"win_team"`
	OverReason string `json:"over_reason,omitempty"`

	events []Event
	rng    RNG
}

// TakeEvents returns and clears the events produced since the last call.
func (t *Table) TakeEvents() []Event {
	ev := t.events
	t.events = nil
	return ev
}

func (t *Table) emit(op int64, typ string, seat int, data map[string]any, cards []Card) {
	t.events = append(t.events, Event{Op: op, Type: typ, Seat: seat, Data: data, Cards: cards})
}

// NewTable deals a new round. players must already have Seat, Team and
// profile fields set; hands are replaced.
func NewTable(mode, format string, turnMs int64, players []*Player, rng RNG, now int64) *Table {
	t := &Table{Mode: mode, Format: format, TurnMs: turnMs, Players: players, Dir: 1, Winner: -1, WinTeam: -1, rng: rng}
	t.Pile = NewDeck(mode)
	shuffle(t.Pile, rng)
	for _, p := range players {
		p.Hand = nil
		p.TangoArmed, p.Vulnerable, p.FinalPoints = false, false, 0
	}
	for i := 0; i < HandSize; i++ {
		for _, p := range players {
			if !p.Removed {
				p.Hand = append(p.Hand, t.pop())
			}
		}
	}
	// First discard must be a number card; anything else goes to the bottom.
	for guard := 0; guard < len(t.Pile)+1; guard++ {
		c := t.pop()
		if c.Kind == KindNumber {
			t.Discard = append(t.Discard, c)
			break
		}
		t.Pile = append([]Card{c}, t.Pile...)
	}
	t.Color = t.Discard[len(t.Discard)-1].Color
	first := rng.RandInt(0, len(players)-1)
	for players[first].Removed {
		first = (first + 1) % len(players)
	}
	t.emit(OpGameStart, "game_start", first, map[string]any{"first_seat": first}, nil)
	t.beginTurn(first, now)
	return t
}

func shuffle(cards []Card, rng RNG) {
	for i := len(cards) - 1; i > 0; i-- {
		j := rng.RandInt(0, i)
		cards[i], cards[j] = cards[j], cards[i]
	}
}

func (t *Table) pop() Card {
	c := t.Pile[len(t.Pile)-1]
	t.Pile = t.Pile[:len(t.Pile)-1]
	return c
}

// Top is the card on the discard pile.
func (t *Table) Top() Card { return t.Discard[len(t.Discard)-1] }

func (t *Table) player(seat int) *Player {
	if seat < 0 || seat >= len(t.Players) {
		return nil
	}
	return t.Players[seat]
}

// ActiveCount is the number of players still in the round.
func (t *Table) ActiveCount() int {
	n := 0
	for _, p := range t.Players {
		if !p.Removed {
			n++
		}
	}
	return n
}

// Next returns the seat `steps` active players after from, in play order.
func (t *Table) Next(from, steps int) int {
	n := len(t.Players)
	s := from
	for k, guard := 0, 0; k < steps && guard < n*steps+n; guard++ {
		s = ((s+t.Dir)%n + n) % n
		if !t.Players[s].Removed {
			k++
		}
	}
	return s
}

// Teammates reports whether two seats are on the same team.
func (t *Table) Teammates(a, b int) bool {
	pa, pb := t.player(a), t.player(b)
	return pa != nil && pb != nil && pa.Team == pb.Team
}

// Legal reports whether c can be played now by the player on turn (ignoring
// whose card it is).
func (t *Table) Legal(c Card) bool {
	if t.Pending > 0 {
		return c.IsWildPlus() || (c.Kind == KindDraw2 && c.Color == t.Color)
	}
	if c.IsWild() || c.Color == t.Color {
		return true
	}
	top := t.Top()
	if top.Kind == KindNumber {
		return c.Kind == KindNumber && c.Value == top.Value
	}
	return !top.IsWild() && c.Kind == top.Kind
}

// Playable lists the IDs in seat's hand that can be played right now.
func (t *Table) Playable(seat int) []int {
	p := t.player(seat)
	if p == nil || t.Over || t.Phase != PhaseTurn || t.Turn != seat {
		return []int{}
	}
	out := []int{}
	for _, c := range p.Hand {
		if t.Legal(c) {
			out = append(out, c.ID)
		}
	}
	return out
}

func (t *Table) canStack(p *Player) bool {
	for _, c := range p.Hand {
		if t.Legal(c) {
			return true
		}
	}
	return false
}

// CanChallenge reports whether seat may challenge the black plus on top.
func (t *Table) CanChallenge(seat int) bool {
	return !t.Over && t.Phase == PhaseTurn && t.Turn == seat && t.Pending > 0 &&
		t.Chal != nil && t.Chal.Open && t.Chal.Target != seat
}

func (t *Table) beginTurn(seat int, now int64) {
	for guard := 0; guard < 64 && !t.Over; guard++ {
		t.Turn, t.Phase, t.PhaseSeat, t.Drawn = seat, PhaseTurn, seat, nil
		t.Deadline = now + t.TurnMs
		p := t.Players[seat]
		if t.Pending > 0 && !t.canStack(p) && !t.CanChallenge(seat) {
			// Nothing to answer the stack with: take it and pass.
			t.drawStack(seat, now)
			seat = t.Next(seat, 1)
			continue
		}
		t.emit(OpTurn, "turn", seat, t.turnData(), nil)
		return
	}
}

func (t *Table) turnData() map[string]any {
	return map[string]any{"phase": t.Phase, "phase_seat": t.PhaseSeat, "deadline_ms": t.Deadline,
		"pending": t.Pending, "color": t.Color, "dir": t.Dir, "can_challenge": t.CanChallenge(t.Turn)}
}

func (t *Table) requireTurn(seat int) (*Player, error) {
	if t.Over {
		return nil, ruleErr("GAME_OVER")
	}
	p := t.player(seat)
	if p == nil || p.Removed {
		return nil, ruleErr("NOT_IN_GAME")
	}
	if t.Phase != PhaseTurn || t.Turn != seat {
		return nil, ruleErr("NOT_YOUR_TURN")
	}
	return p, nil
}

// Play plays card id from seat's hand. color is required for black cards
// unless the player will pick it afterwards with ChooseColor.
func (t *Table) Play(seat, cardID int, color string, now int64) error {
	p, err := t.requireTurn(seat)
	if err != nil {
		return err
	}
	idx := indexOf(p.Hand, cardID)
	if idx < 0 {
		return ruleErr("CARD_NOT_IN_HAND")
	}
	c := p.Hand[idx]
	if !t.Legal(c) {
		if t.Pending > 0 {
			return ruleErr("MUST_STACK_OR_DRAW")
		}
		return ruleErr("ILLEGAL_CARD")
	}
	if c.IsWild() && color != "" && !ValidColor(color) {
		return ruleErr("BAD_COLOR")
	}
	t.commitPlay(seat, idx, color, now, false)
	return nil
}

func indexOf(h []Card, id int) int {
	for i, c := range h {
		if c.ID == id {
			return i
		}
	}
	return -1
}

func (t *Table) commitPlay(seat, idx int, color string, now int64, auto bool) {
	p := t.Players[seat]
	c := p.Hand[idx]
	hadColor := false
	for i, x := range p.Hand {
		if i != idx && x.Color == t.Color {
			hadColor = true
		}
	}
	firstOfStack := t.Pending == 0
	p.Hand = append(p.Hand[:idx:idx], p.Hand[idx+1:]...)
	t.Discard = append(t.Discard, c)
	switch len(p.Hand) {
	case 1:
		p.Vulnerable = !p.TangoArmed
	default:
		p.Vulnerable = false
	}
	t.emit(OpCardPlayed, "card_played", seat, map[string]any{"card": c, "auto": auto, "hand_count": len(p.Hand), "vulnerable": p.Vulnerable}, nil)
	if len(p.Hand) == 0 {
		t.finish(seat, p.Team, "empty_hand")
		return
	}
	if c.IsWild() {
		t.Wild = &PendingWild{Seat: seat, Card: c, FirstOfStack: firstOfStack, HadColor: hadColor, ColorBefore: t.Color}
		if ValidColor(color) {
			t.applyColor(seat, color, now)
			return
		}
		t.Phase, t.PhaseSeat, t.Deadline = PhaseColor, seat, now+t.TurnMs
		t.emit(OpTurn, "turn", seat, t.turnData(), nil)
		return
	}
	t.Color = c.Color
	switch c.Kind {
	case KindDraw2:
		t.Pending += 2
		t.Chal = nil
		t.beginTurn(t.Next(seat, 1), now)
	case KindSkip:
		skipped := t.Next(seat, 1)
		t.emit(OpTurn, "skipped", skipped, map[string]any{"by": seat}, nil)
		t.beginTurn(t.Next(seat, 2), now)
	case KindReverse:
		t.Dir = -t.Dir
		if t.ActiveCount() == 2 {
			t.beginTurn(t.Next(seat, 2), now) // with two players Reverse acts as Skip
		} else {
			t.beginTurn(t.Next(seat, 1), now)
		}
	default:
		t.beginTurn(t.Next(seat, 1), now)
	}
}

func (t *Table) applyColor(seat int, color string, now int64) {
	w := t.Wild
	t.Wild = nil
	t.Color = color
	t.emit(OpColorChosen, "color_chosen", seat, map[string]any{"color": color}, nil)
	if w != nil && w.Card.IsWildPlus() {
		t.Pending += w.Card.DrawAmount()
		t.Chal = &Challenge{Target: seat, Open: w.FirstOfStack, HadColor: w.HadColor, ColorBefore: w.ColorBefore}
	}
	t.beginTurn(t.Next(seat, 1), now)
}

// ChooseColor sets the colour of the black card seat just played.
func (t *Table) ChooseColor(seat int, color string, now int64) error {
	if t.Over {
		return ruleErr("GAME_OVER")
	}
	if t.Phase != PhaseColor || t.PhaseSeat != seat {
		return ruleErr("NOT_YOUR_TURN")
	}
	if !ValidColor(color) {
		return ruleErr("BAD_COLOR")
	}
	t.applyColor(seat, color, now)
	return nil
}

// Draw takes the pending stack, or one card. A drawn card that fits is
// played automatically, except a black plus, which waits for KeepOrPlay.
func (t *Table) Draw(seat int, now int64) error {
	if _, err := t.requireTurn(seat); err != nil {
		return err
	}
	if t.Pending > 0 {
		t.drawStack(seat, now)
		t.beginTurn(t.Next(seat, 1), now)
		return nil
	}
	got := t.drawCards(seat, 1, "draw")
	if len(got) == 0 || !t.Legal(got[0]) {
		t.beginTurn(t.Next(seat, 1), now)
		return nil
	}
	c := got[0]
	p := t.Players[seat]
	if c.IsWildPlus() {
		t.Phase, t.PhaseSeat, t.Deadline, t.Drawn = PhaseKeep, seat, now+t.TurnMs, &c
		t.emit(OpDrawnChoice, "drawn_choice", seat, map[string]any{"deadline_ms": t.Deadline}, []Card{c})
		t.emit(OpTurn, "turn", seat, t.turnData(), nil)
		return nil
	}
	t.commitPlay(seat, indexOf(p.Hand, c.ID), "", now, true)
	return nil
}

// KeepOrPlay answers the choice offered for a drawn black plus.
func (t *Table) KeepOrPlay(seat int, play bool, color string, now int64) error {
	if t.Over {
		return ruleErr("GAME_OVER")
	}
	if t.Phase != PhaseKeep || t.PhaseSeat != seat || t.Drawn == nil {
		return ruleErr("NOT_YOUR_TURN")
	}
	p := t.Players[seat]
	idx := indexOf(p.Hand, t.Drawn.ID)
	if !play || idx < 0 {
		t.Drawn = nil
		t.beginTurn(t.Next(seat, 1), now)
		return nil
	}
	if color != "" && !ValidColor(color) {
		return ruleErr("BAD_COLOR")
	}
	t.Drawn = nil
	t.Phase = PhaseTurn
	t.commitPlay(seat, idx, color, now, false)
	return nil
}

// ChallengeTop challenges the black plus on top of the stack.
func (t *Table) ChallengeTop(seat int, now int64) error {
	if _, err := t.requireTurn(seat); err != nil {
		return err
	}
	if !t.CanChallenge(seat) {
		return ruleErr("CANNOT_CHALLENGE")
	}
	ch := t.Chal
	amount := t.Pending
	t.Chal, t.Pending = nil, 0
	if ch.HadColor {
		t.emit(OpChallenge, "challenge", seat, map[string]any{"target": ch.Target, "success": true, "drawer": ch.Target, "count": amount, "color_before": ch.ColorBefore}, nil)
		t.drawCards(ch.Target, amount, "challenge")
		t.beginTurn(seat, now)
		return nil
	}
	t.emit(OpChallenge, "challenge", seat, map[string]any{"target": ch.Target, "success": false, "drawer": seat, "count": amount + 2, "color_before": ch.ColorBefore}, nil)
	t.drawCards(seat, amount+2, "challenge")
	t.beginTurn(t.Next(seat, 1), now)
	return nil
}

// PressTango declares TANGO! for seat (allowed at two cards or fewer).
func (t *Table) PressTango(seat int) error {
	if t.Over {
		return ruleErr("GAME_OVER")
	}
	p := t.player(seat)
	if p == nil || p.Removed {
		return ruleErr("NOT_IN_GAME")
	}
	if len(p.Hand) > 2 {
		return ruleErr("TOO_MANY_CARDS")
	}
	if p.TangoArmed && !p.Vulnerable {
		return nil
	}
	p.TangoArmed, p.Vulnerable = true, false
	t.emit(OpTango, "tango", seat, map[string]any{"hand_count": len(p.Hand)}, nil)
	return nil
}

// Hammer lets an opponent catch a player who is down to one card without
// TANGO!. A miss has no penalty.
func (t *Table) Hammer(by, target int) error {
	if t.Over {
		return ruleErr("GAME_OVER")
	}
	pb, pt := t.player(by), t.player(target)
	if pb == nil || pb.Removed || pt == nil || pt.Removed {
		return ruleErr("NOT_IN_GAME")
	}
	if by == target || pb.Team == pt.Team {
		return ruleErr("NOT_AN_OPPONENT")
	}
	if !pt.Vulnerable || len(pt.Hand) != 1 {
		t.emit(OpHammer, "hammer", by, map[string]any{"target": target, "hit": false, "count": 0}, nil)
		return nil
	}
	pt.Vulnerable = false
	t.emit(OpHammer, "hammer", by, map[string]any{"target": target, "hit": true, "count": 2}, nil)
	t.drawCards(target, 2, "hammer")
	return nil
}

// Timeout applies the default action when the phase deadline has passed and
// reports which seat timed out.
func (t *Table) Timeout(now int64, rng RNG) (int, bool) {
	if t.Over || t.Deadline == 0 || now < t.Deadline {
		return -1, false
	}
	seat := t.PhaseSeat
	switch t.Phase {
	case PhaseColor:
		t.applyColor(seat, BestColor(t.Players[seat].Hand, rng), now)
	case PhaseKeep:
		_ = t.KeepOrPlay(seat, false, "", now)
	case PhaseTurn:
		_ = t.Draw(seat, now)
	}
	return seat, true
}

// BestColor picks the colour the hand holds most of.
func BestColor(h []Card, rng RNG) string {
	count := map[string]int{}
	for _, c := range h {
		if !c.IsWild() {
			count[c.Color]++
		}
	}
	best, n := "", 0
	for _, col := range Colors {
		if count[col] > n {
			best, n = col, count[col]
		}
	}
	if best == "" {
		return Colors[rng.RandInt(0, len(Colors)-1)]
	}
	return best
}

func (t *Table) drawStack(seat int, now int64) {
	n := t.Pending
	t.Pending, t.Chal = 0, nil
	t.drawCards(seat, n, "stack")
}

func (t *Table) drawCards(seat, n int, reason string) []Card {
	p := t.Players[seat]
	var got []Card
	for i := 0; i < n; i++ {
		if len(t.Pile) == 0 && !t.reshuffle() {
			break
		}
		c := t.pop()
		got = append(got, c)
		p.Hand = append(p.Hand, c)
	}
	if len(p.Hand) != 1 {
		p.Vulnerable = false
	}
	if len(p.Hand) >= 3 {
		p.TangoArmed = false
	}
	t.emit(OpCardsDrawn, "cards_drawn", seat, map[string]any{"count": len(got), "reason": reason, "hand_count": len(p.Hand)}, got)
	return got
}

// reshuffle turns the discard pile (minus its top card) into a new draw pile.
func (t *Table) reshuffle() bool {
	if len(t.Discard) <= 1 {
		return false
	}
	top := t.Discard[len(t.Discard)-1]
	t.Pile = append(t.Pile, t.Discard[:len(t.Discard)-1]...)
	t.Discard = []Card{top}
	if t.rng != nil {
		shuffle(t.Pile, t.rng)
	}
	t.emit(OpCardsDrawn, "reshuffle", -1, map[string]any{"draw_count": len(t.Pile)}, nil)
	return true
}

// Remove takes a player out of the round (left for good or idle too long).
// Their cards go to the bottom of the draw pile.
func (t *Table) Remove(seat int, reason string, rng RNG, now int64) {
	p := t.player(seat)
	if p == nil || p.Removed || t.Over {
		return
	}
	p.FinalPoints = handPoints(p.Hand)
	p.Removed, p.RemovedReason = true, reason
	p.Vulnerable, p.TangoArmed = false, false
	t.Pile = append(append([]Card{}, p.Hand...), t.Pile...)
	p.Hand = nil
	t.emit(OpPlayerStatus, "player_removed", seat, map[string]any{"reason": reason}, nil)
	teams := map[int]bool{}
	for _, q := range t.Players {
		if !q.Removed {
			teams[q.Team] = true
		}
	}
	if len(teams) <= 1 {
		win := -1
		for tm := range teams {
			win = tm
		}
		t.finish(-1, win, "forfeit")
		return
	}
	if t.PhaseSeat == seat || t.Turn == seat {
		if t.Phase == PhaseColor && t.Wild != nil {
			t.applyColor(seat, Colors[rng.RandInt(0, 3)], now)
			return
		}
		t.Drawn = nil
		t.beginTurn(t.Next(seat, 1), now)
	}
}

func (t *Table) finish(winner, team int, reason string) {
	t.Over, t.Winner, t.WinTeam, t.OverReason = true, winner, team, reason
	t.Phase, t.Deadline, t.Wild, t.Drawn = PhaseOver, 0, nil, nil
	for _, p := range t.Players {
		if !p.Removed {
			p.FinalPoints = handPoints(p.Hand)
		}
	}
}

// SetRNG gives the table its random source (not serialised).
func (t *Table) SetRNG(r RNG) { t.rng = r }
