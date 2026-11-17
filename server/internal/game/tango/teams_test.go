package tango

import (
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
