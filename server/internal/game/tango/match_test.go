package tango

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/dev-sssss/tango_backend/server/internal/game"
	"github.com/dev-sssss/tango_backend/server/internal/game/memnk"
)

type sent struct {
	op   int64
	data map[string]any
	to   []game.Presence
}

type fakeDispatcher struct {
	msgs   []sent
	label  string
	kicked []game.Presence
}

func (d *fakeDispatcher) Broadcast(op int64, data []byte, to []game.Presence) {
	m, _ := game.DecodeObject(string(data))
	d.msgs = append(d.msgs, sent{op, m, to})
}
func (d *fakeDispatcher) Kick(ps []game.Presence) { d.kicked = append(d.kicked, ps...) }
func (d *fakeDispatcher) UpdateLabel(l string)    { d.label = l }

func (d *fakeDispatcher) take() []sent { m := d.msgs; d.msgs = nil; return m }

type harness struct {
	t   *testing.T
	nk  *memnk.NK
	m   *Match
	d   *fakeDispatcher
	mc  *game.MatchContext
	tk  int64
	box []game.MatchMessage
}

func newHarness(t *testing.T, seed uint64, params map[string]any) *harness {
	nk := memnk.New(seed, 1_700_000_000_000, nil)
	h := &harness{t: t, nk: nk, d: &fakeDispatcher{}, mc: &game.MatchContext{Ctx: context.Background(), MatchID: "m1.node1"}}
	h.m = Factory{}.New(nk).(*Match)
	rate, label := h.m.Init(h.mc, params)
	if rate != tickRate || label == "" {
		t.Fatal("init")
	}
	return h
}

func pres(uid string) game.Presence {
	return game.Presence{UserID: uid, SessionID: "s-" + uid, Username: uid, Node: "node1"}
}

func (h *harness) join(uid string) {
	ok, reason := h.m.JoinAttempt(h.mc, h.d, h.tk, pres(uid), nil)
	if !ok {
		h.t.Fatalf("join %s refused: %s", uid, reason)
	}
	h.m.Join(h.mc, h.d, h.tk, []game.Presence{pres(uid)})
}

func (h *harness) send(uid string, op int64, payload map[string]any) {
	b, _ := json.Marshal(payload)
	h.box = append(h.box, game.MatchMessage{Sender: pres(uid), OpCode: op, Data: b, HasData: true})
}

func (h *harness) tick(ms int64) bool {
	h.nk.Advance(ms)
	h.tk++
	msgs := h.box
	h.box = nil
	return h.m.Loop(h.mc, h.d, h.tk, msgs)
}

// humanPlays makes uid act like a bot through real client messages.
func (h *harness) humanPlays(uid string) {
	s := h.m.s
	if s.Status != StatusPlaying {
		return
	}
	seat := h.m.seatOf(uid)
	t := s.Table
	if t.Over || t.PhaseSeat != seat {
		return
	}
	mv := DecideBot(t, seat, h.nk)
	switch mv.Kind {
	case "play":
		h.send(uid, OpPlay, map[string]any{"card_id": mv.CardID, "color": mv.Color, "tango": mv.Tango})
	case "color":
		h.send(uid, OpChooseColor, map[string]any{"color": mv.Color})
	case "play_drawn":
		h.send(uid, OpKeepOrPlay, map[string]any{"play": true, "color": mv.Color})
	case "keep":
		h.send(uid, OpKeepOrPlay, map[string]any{"play": false})
	case "challenge":
		h.send(uid, OpChallengeTop, map[string]any{})
	default:
		h.send(uid, OpDraw, map[string]any{})
	}
}

func TestPublicMatchFillsBotsAndSettles(t *testing.T) {
	h := newHarness(t, 7, map[string]any{"mode": ModeNoMercy, "format": Format2v2, "fee": int64(400)})
	h.nk.AddUser("u1", "alice", "", map[string]int64{"coins": 1000})
	h.join("u1")
	for i := 0; i < 200 && h.m.s.Status == StatusWaiting; i++ {
		h.tick(100)
	}
	if h.m.s.Status != StatusStarting || len(h.m.s.Seats) != 4 {
		t.Fatalf("status %s seats %d", h.m.s.Status, len(h.m.s.Seats))
	}
	for i := 0; i < 40 && h.m.s.Status != StatusPlaying; i++ {
		h.tick(100)
	}
	if h.m.s.Status != StatusPlaying {
		t.Fatal("did not start")
	}
	if c := h.nk.Wallet("u1")["coins"]; c != 600 {
		t.Fatalf("entry fee not escrowed: %d", c)
	}
	// Bots are not visible as bots and their hands are hidden.
	start := h.d.take()
	var gs map[string]any
	for _, s := range start {
		if s.op == OpGameStart {
			gs = s.data
		}
	}
	if gs == nil {
		t.Fatal("no game_start")
	}
	st := gs["state"].(map[string]any)
	for _, pv := range st["players"].([]any) {
		p := pv.(map[string]any)
		if _, isBot := p["bot"]; isBot {
			t.Fatal("bot flag leaked")
		}
		seat := int(p["seat"].(float64))
		_, hasHand := p["hand"]
		mine := seat == 0
		mate := seat == 2
		if (mine || mate) != hasHand {
			t.Fatalf("seat %d hand visibility wrong", seat)
		}
	}
	for i := 0; i < 20000 && h.m.s.Status == StatusPlaying; i++ {
		h.humanPlays("u1")
		h.tick(100)
	}
	if h.m.s.Status != StatusOver {
		t.Fatalf("game did not finish: %s", h.m.s.Status)
	}
	var mine ResultRow
	for _, r := range h.m.s.Result {
		if r.UserID == "u1" {
			mine = r
		}
	}
	want := 1000 + mine.Coins
	if c := h.nk.Wallet("u1")["coins"]; c != want {
		t.Fatalf("coins after settle %d want %d (row %+v)", c, want, mine)
	}
	prof, _, _ := game.LoadProfile(context.Background(), h.nk, "u1")
	if prof.Games != 1 {
		t.Fatalf("profile games %d", prof.Games)
	}
	// Public matches close after the results screen.
	for i := 0; i < 200; i++ {
		if !h.tick(100) {
			return
		}
	}
	t.Fatal("match should end after results")
}

func TestOneVsOneBetweenHumans(t *testing.T) {
	for seed := uint64(1); seed <= 20; seed++ {
		h := newHarness(t, seed, map[string]any{"mode": ModeClassic, "format": Format1v1, "fee": int64(200)})
		h.nk.AddUser("a", "a", "", map[string]int64{"coins": 500})
		h.nk.AddUser("b", "b", "", map[string]int64{"coins": 500})
		h.join("a")
		h.join("b")
		for i := 0; i < 50 && h.m.s.Status != StatusPlaying; i++ {
			h.tick(100)
		}
		for i := 0; i < 20000 && h.m.s.Status == StatusPlaying; i++ {
			h.humanPlays("a")
			h.humanPlays("b")
			h.tick(100)
		}
		if h.m.s.Status != StatusOver {
			t.Fatalf("seed %d not over", seed)
		}
		total := h.nk.Wallet("a")["coins"] + h.nk.Wallet("b")["coins"]
		if total != 1000 { // 1v1: winner +200, loser -200
			t.Fatalf("seed %d: coins not conserved: %d", seed, total)
		}
	}
}

func TestInsufficientCoinsRefused(t *testing.T) {
	h := newHarness(t, 1, map[string]any{"format": Format1v1, "fee": int64(1000)})
	h.nk.AddUser("poor", "poor", "", map[string]int64{"coins": 100})
	ok, reason := h.m.JoinAttempt(h.mc, h.d, 0, pres("poor"), nil)
	if ok || reason != "INSUFFICIENT_COINS" {
		t.Fatalf("ok=%v reason=%s", ok, reason)
	}
}

func TestDisconnectedPlayerRemovedAfterThreeMisses(t *testing.T) {
	h := newHarness(t, 3, map[string]any{"format": Format1v1, "fee": int64(200)})
	h.nk.AddUser("a", "a", "", map[string]int64{"coins": 500})
	h.nk.AddUser("b", "b", "", map[string]int64{"coins": 500})
	h.join("a")
	h.join("b")
	for i := 0; i < 50 && h.m.s.Status != StatusPlaying; i++ {
		h.tick(100)
	}
	h.m.Leave(h.mc, h.d, h.tk, []game.Presence{pres("b")})
	for i := 0; i < 3000 && h.m.s.Status == StatusPlaying; i++ {
		h.humanPlays("a")
		h.tick(100)
	}
	if h.m.s.Status != StatusOver {
		t.Fatal("not over")
	}
	for _, r := range h.m.s.Result {
		if r.UserID == "a" && !r.Winner {
			// a can also win normally before b is removed; both are fine,
			// but b must never be ranked above a.
			t.Fatalf("a should win: %+v", h.m.s.Result)
		}
	}
}

func TestRejoinGetsSnapshot(t *testing.T) {
	h := newHarness(t, 4, map[string]any{"format": Format1v1})
	h.nk.AddUser("a", "a", "", nil)
	h.nk.AddUser("b", "b", "", nil)
	h.join("a")
	h.join("b")
	for i := 0; i < 50 && h.m.s.Status != StatusPlaying; i++ {
		h.tick(100)
	}
	h.m.Leave(h.mc, h.d, h.tk, []game.Presence{pres("b")})
	h.d.take()
	h.join("b")
	got := false
	for _, s := range h.d.take() {
		if s.op == OpSnapshot && s.to[0].UserID == "b" {
			got = true
			st := s.data["state"].(map[string]any)
			if int(st["you"].(float64)) != 1 {
				t.Fatal("wrong seat in snapshot")
			}
		}
	}
	if !got {
		t.Fatal("no snapshot on rejoin")
	}
}

func TestHandoverRoundTrip(t *testing.T) {
	h := newHarness(t, 5, map[string]any{"format": Format2v2, "mode": ModeNoMercy})
	h.nk.AddUser("a", "a", "", nil)
	h.join("a")
	for i := 0; i < 200 && h.m.s.Status != StatusPlaying; i++ {
		h.tick(100)
	}
	for i := 0; i < 30; i++ {
		h.humanPlays("a")
		h.tick(100)
	}
	b, err := h.m.MarshalState()
	if err != nil {
		t.Fatal(err)
	}
	restored, err := Factory{}.Restore(h.nk, StateSchemaVersion, b)
	if err != nil {
		t.Fatal(err)
	}
	h.m = restored.(*Match)
	if h.m.s.Seats[0].Player != h.m.s.Table.Players[0] {
		t.Fatal("seats and table not relinked")
	}
	for i := 0; i < 20000 && h.m.s.Status == StatusPlaying; i++ {
		h.humanPlays("a")
		h.tick(100)
	}
	if h.m.s.Status != StatusOver {
		t.Fatal("restored match did not finish")
	}
}

func TestPrivateRoomRematch(t *testing.T) {
	h := newHarness(t, 6, map[string]any{"format": Format1v1, "private": true, "host_id": "a", "room_code": "123456",
		"settings": map[string]any{"turn_seconds": 10, "emotes": false}})
	h.nk.AddUser("a", "a", "", nil)
	h.nk.AddUser("b", "b", "", nil)
	h.join("a")
	h.join("b")
	for i := 0; i < 50 && h.m.s.Status != StatusPlaying; i++ {
		h.tick(100)
	}
	if h.m.s.Table.TurnMs != 10000 {
		t.Fatal("turn seconds not applied")
	}
	h.send("a", OpSendEmote, map[string]any{"id": "laugh"})
	h.tick(100)
	for i := 0; i < 20000 && h.m.s.Status == StatusPlaying; i++ {
		h.humanPlays("a")
		h.humanPlays("b")
		h.tick(100)
	}
	h.send("a", OpRematchVote, nil)
	h.send("b", OpRematchVote, nil)
	h.tick(100)
	if h.m.s.Status != StatusStarting {
		t.Fatalf("rematch not started: %s", h.m.s.Status)
	}
	for i := 0; i < 50 && h.m.s.Status != StatusPlaying; i++ {
		h.tick(100)
	}
	if h.m.s.Round != 2 {
		t.Fatalf("round %d", h.m.s.Round)
	}
}
