package tango

import "github.com/dev-sssss/tango_backend/server/internal/game"

// What each client is allowed to see. A player sees their own hand and, in
// team games, their teammates' hands; everyone else only sees card counts.
// Spectators (seat -1) see counts only. Bots are not marked as bots.

type recipient struct {
	p    game.Presence
	seat int
}

func (m *Match) recipients() []recipient {
	var out []recipient
	for i, st := range m.s.Seats {
		if !st.Player.Bot && st.Connected && st.Presence != nil {
			out = append(out, recipient{*st.Presence, i})
		}
	}
	for _, p := range m.s.Spectators {
		out = append(out, recipient{p, -1})
	}
	return out
}

func (m *Match) canSee(viewer, seat int) bool {
	if viewer < 0 || seat < 0 {
		return false
	}
	if viewer == seat {
		return true
	}
	return IsTeamFormat(m.s.Format) && TeamOf(m.s.Format, viewer) == TeamOf(m.s.Format, seat)
}

func (m *Match) send(d game.Dispatcher, p game.Presence, op int64, payload map[string]any) {
	payload["server_time_ms"] = m.nk.NowMs()
	d.Broadcast(op, game.EncodeBytes(payload), []game.Presence{p})
}

// broadcast sends the same payload to everyone (no private cards). When
// withState is set each recipient also gets their own state view.
func (m *Match) broadcast(d game.Dispatcher, op int64, payload map[string]any, withState *bool) {
	for _, r := range m.recipients() {
		pl := make(map[string]any, len(payload)+2)
		for k, v := range payload {
			pl[k] = v
		}
		if withState != nil && *withState {
			pl["state"] = m.view(r.seat)
		}
		m.send(d, r.p, op, pl)
	}
}

func (m *Match) sendError(d game.Dispatcher, p game.Presence, code string) {
	m.send(d, p, OpError, map[string]any{"type": "error", "code": code})
}

func (m *Match) sendSnapshot(d game.Dispatcher, p game.Presence, seat int) {
	m.send(d, p, OpSnapshot, map[string]any{"type": "snapshot", "state": m.view(seat)})
}

func (m *Match) broadcastLobby(d game.Dispatcher) {
	t := true
	m.broadcast(d, OpLobby, map[string]any{"type": "lobby"}, &t)
}

// flush turns the table's pending events into messages. Cards are attached
// only for recipients allowed to see them; the last event of the batch
// carries each recipient's full state.
func (m *Match) flush(d game.Dispatcher) {
	t := m.s.Table
	if t == nil {
		return
	}
	evs := t.TakeEvents()
	if len(evs) == 0 {
		return
	}
	m.s.Rev++
	rs := m.recipients()
	for i, ev := range evs {
		for _, r := range rs {
			pl := map[string]any{"type": ev.Type, "seat": ev.Seat}
			for k, v := range ev.Data {
				pl[k] = v
			}
			if len(ev.Cards) > 0 && m.canSee(r.seat, ev.Seat) {
				pl["cards"] = ev.Cards
			}
			if i == len(evs)-1 {
				pl["state"] = m.view(r.seat)
			}
			m.send(d, r.p, ev.Op, pl)
		}
	}
}

// view is the state as seen from seat (-1 for spectators).
func (m *Match) view(seat int) map[string]any {
	s := m.s
	players := make([]map[string]any, 0, len(s.Seats))
	var plan []int
	if s.Status == StatusWaiting {
		plan = m.teamPlan() // teams as the room fills; seats are arranged at the start
	}
	for i, st := range s.Seats {
		p := st.Player
		team := TeamOf(s.Format, i)
		if plan != nil {
			team = plan[i]
		}
		pv := map[string]any{"seat": i, "user_id": p.UserID, "name": p.Name, "avatar": p.Avatar, "frame": p.Frame, "badge": p.Badge,
			"card_back": p.CardBack, "trophies": p.Trophies, "level": LevelFor(p.Trophies), "team": team,
			"connected": p.Bot || st.Connected, "removed": p.Removed, "hand_count": len(p.Hand),
			"tango": p.TangoArmed, "vulnerable": p.Vulnerable, "is_host": p.UserID == s.HostID}
		if m.canSee(seat, i) {
			pv["hand"] = p.Hand
		}
		players = append(players, pv)
	}
	v := map[string]any{"match_id": s.MatchID, "mode": s.Mode, "format": s.Format, "fee": s.Fee, "private": s.Private,
		"room_code": s.RoomCode, "status": s.Status, "round": s.Round, "settings": s.Settings, "you": seat,
		"max_players": m.maxSeats(), "players": players, "rev": s.Rev}
	switch s.Status {
	case StatusWaiting:
		if !s.Private {
			v["fill_at_ms"] = s.CreatedMs + lobbyWaitMs
		}
	case StatusStarting:
		v["start_at_ms"] = s.StartAtMs
	case StatusOver:
		v["results"] = m.publicResults()
		v["end_at_ms"] = s.EndAtMs
	}
	if t := s.Table; t != nil && s.Status != StatusStarting {
		g := map[string]any{"top": t.Top(), "color": t.Color, "turn": t.Turn, "dir": t.Dir, "pending": t.Pending,
			"phase": t.Phase, "phase_seat": t.PhaseSeat, "deadline_ms": t.Deadline, "draw_count": len(t.Pile),
			"discard_count": len(t.Discard), "over": t.Over, "ends_at_ms": t.EndsAt, "over_reason": t.OverReason}
		recent := t.Discard
		if len(recent) > 5 {
			recent = recent[len(recent)-5:]
		}
		g["recent"] = recent
		if seat >= 0 {
			g["playable"] = t.Playable(seat)
			g["can_challenge"] = t.CanChallenge(seat)
			if t.Phase == PhaseKeep && t.PhaseSeat == seat && t.Drawn != nil {
				g["drawn"] = *t.Drawn
			}
			if t.Phase == PhaseSwap && t.PhaseSeat == seat {
				g["swap_targets"] = t.SwapTargets(seat)
			}
		}
		v["game"] = g
	}
	return v
}
