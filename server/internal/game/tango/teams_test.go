package tango

import (
	"strings"
	"testing"

	"github.com/dev-sssss/tango_backend/server/internal/game"
)

func (h *harness) joinWith(uid string, meta map[string]string) {
	ok, reason := h.m.JoinAttempt(h.mc, h.d, h.tk, pres(uid), meta)
	if !ok {
		h.t.Fatalf("join %s refused: %s", uid, reason)
	}
	h.m.Join(h.mc, h.d, h.tk, []game.Presence{pres(uid)})
}

// A friend invited by the host sits on the host's team even when an
// uninvited player joined first.
func TestInvitedFriendJoinsInvitersTeam(t *testing.T) {
	h := newHarness(t, 3, map[string]any{"mode": ModeClassic, "format": Format2v2, "private": true, "host_id": "host"})
	h.join("host")
	h.join("stranger")                                           // by room code: the smaller team
	h.joinWith("friend", map[string]string{"team_with": "host"}) // invited by the host
	plan := h.m.teamPlan()
	if plan[0] != 0 || plan[1] != 1 || plan[2] != 0 {
		t.Fatalf("plan %v", plan)
	}
	if v := h.m.view(0)["players"].([]map[string]any); v[2]["team"] != 0 {
		t.Fatalf("lobby shows the friend on team %v", v[2]["team"])
	}
	h.send("host", OpStart, map[string]any{})
	h.tick(100)
	s := h.m.s
	if len(s.Seats) != 4 {
		t.Fatalf("seats %d", len(s.Seats))
	}
	host, friend, stranger := h.m.seatOf("host"), h.m.seatOf("friend"), h.m.seatOf("stranger")
	if TeamOf(s.Format, host) != TeamOf(s.Format, friend) || TeamOf(s.Format, host) == TeamOf(s.Format, stranger) {
		t.Fatalf("host %d friend %d stranger %d", host, friend, stranger)
	}
}

// When the inviter's team is full the invited player goes to the other team.
func TestInviteToFullTeamGoesToOtherTeam(t *testing.T) {
	h := newHarness(t, 3, map[string]any{"mode": ModeNoMercy, "format": Format2v2, "private": true, "host_id": "host"})
	h.join("host")
	h.joinWith("f1", map[string]string{"team_with": "host"})
	h.joinWith("f2", map[string]string{"team_with": "host"})
	plan := h.m.teamPlan()
	if plan[0] != 0 || plan[1] != 0 || plan[2] != 1 {
		t.Fatalf("plan %v", plan)
	}
}

// A player out on the 25-card rule can reconnect and watch the end; one
// removed for missing turns cannot come back.
func TestMercyOutPlayerCanRejoin(t *testing.T) {
	h := newHarness(t, 5, map[string]any{"mode": ModeNoMercy, "format": Format4P, "private": true, "host_id": "a"})
	for _, u := range []string{"a", "b", "c", "d"} {
		h.join(u)
	}
	h.send("a", OpStart, map[string]any{})
	for i := 0; i < 200 && h.m.s.Status != StatusPlaying; i++ {
		h.tick(100)
	}
	if h.m.s.Status != StatusPlaying {
		t.Fatalf("status %v", h.m.s.Status)
	}
	b, c := h.m.seatOf("b"), h.m.seatOf("c")
	h.m.s.Seats[b].Player.Removed, h.m.s.Seats[b].Player.RemovedReason = true, "mercy"
	h.m.s.Seats[c].Player.Removed, h.m.s.Seats[c].Player.RemovedReason = true, "idle"
	if ok, reason := h.m.JoinAttempt(h.mc, h.d, h.tk, pres("b"), nil); !ok {
		t.Fatalf("mercy-out player refused: %s", reason)
	}
	if ok, reason := h.m.JoinAttempt(h.mc, h.d, h.tk, pres("c"), nil); ok || reason != "REMOVED" {
		t.Fatalf("idle-removed player: %v %s", ok, reason)
	}
}

// A friends party (host + invited friend in a 2v2 room) goes public: only
// the host can do it, a stranger who presses Play joins the other team, and
// after the public wait bots fill the last seat and the game starts with the
// party still together.
func TestPartyGoesPublic(t *testing.T) {
	h := newHarness(t, 9, map[string]any{"mode": ModeClassic, "format": Format2v2, "private": true, "host_id": "host", "room_code": "123456"})
	h.nk.AddUser("host", "host", "", map[string]int64{"coins": 1000})
	h.nk.AddUser("friend", "friend", "", map[string]int64{"coins": 150})
	h.nk.AddUser("stranger", "stranger", "", map[string]int64{"coins": 1000})
	h.join("host")
	h.joinWith("friend", map[string]string{"team_with": "host"})
	if _, resp := h.m.Signal(h.mc, h.d, h.tk, `{"action":"GO_PUBLIC","user_id":"friend","fee":200}`); !strings.Contains(resp, `"ok":false`) {
		t.Fatalf("only the host may take the party public: %s", resp)
	}
	if _, resp := h.m.Signal(h.mc, h.d, h.tk, `{"action":"GO_PUBLIC","user_id":"host","fee":200}`); !strings.Contains(resp, "INSUFFICIENT_COINS") || !strings.Contains(resp, "friend") {
		t.Fatalf("a party member who cannot pay must be named: %s", resp)
	}
	if _, resp := h.m.Signal(h.mc, h.d, h.tk, `{"action":"GO_PUBLIC","user_id":"host","fee":100}`); resp != `{"ok":true}` {
		t.Fatalf("go public: %s", resp)
	}
	s := h.m.s
	if s.Private || !s.Party || s.Fee != 100 || !strings.Contains(s.Label, `"private":false`) || s.Settings.TurnSeconds != defaultTurnSeconds {
		t.Fatalf("after going public: private=%v party=%v fee=%d label=%s", s.Private, s.Party, s.Fee, s.Label)
	}
	h.join("stranger")
	plan := h.m.teamPlan()
	if plan[0] != plan[1] || plan[2] == plan[0] {
		t.Fatalf("teams while waiting %v: party must stay together, stranger on the other side", plan)
	}
	// Not yet: the public wait counts from going public.
	h.tick(lobbyWaitMs - 2000)
	if s.Status != StatusWaiting {
		t.Fatalf("filled too early: %s", s.Status)
	}
	for i := 0; i < 200 && s.Status != StatusPlaying; i++ {
		h.tick(100)
	}
	if s.Status != StatusPlaying || len(s.Seats) != 4 {
		t.Fatalf("status %s seats %d", s.Status, len(s.Seats))
	}
	team := func(uid string) int { return h.m.seatOf(uid) % 2 }
	if team("host") != team("friend") || team("stranger") == team("host") {
		t.Fatalf("teams at the table: host %d friend %d stranger %d", team("host"), team("friend"), team("stranger"))
	}
}
