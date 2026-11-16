package tango

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/dev-sssss/tango_backend/server/internal/game"
)

// Module is the match handler name used with match_create.
const Module = "tango_match"

// StateSchemaVersion is bumped whenever State changes shape; Restore must
// keep accepting every older version (migrate forward).
const StateSchemaVersion = 1

// Match statuses (also in the label).
const (
	StatusWaiting  = "WAITING"
	StatusStarting = "STARTING"
	StatusPlaying  = "PLAYING"
	StatusOver     = "OVER"
)

// Client→server opcodes.
const (
	OpPlay            int64 = 1  // {"card_id":12, "color":"red", "tango":true}
	OpDraw            int64 = 2  // {}
	OpKeepOrPlay      int64 = 3  // {"play":true, "color":"blue"}
	OpPressTango      int64 = 4  // {}
	OpHammerTap       int64 = 5  // {"seat":2}
	OpChallengeTop    int64 = 6  // {}
	OpSendEmote       int64 = 7  // {"id":"laugh"}
	OpRematchVote     int64 = 8  // {}
	OpRequestSnapshot int64 = 9  // {}
	OpForfeit         int64 = 10 // {}
	OpChooseColor     int64 = 11 // {"color":"green"}
	OpStart           int64 = 12 // private room host: start now (empty seats get bots)
)

const (
	tickRate              = 10
	lobbyWaitMs           = 15000 // public: fill with bots after this
	startCountdownMs      = 2500
	reserveMs             = 10000
	emptyMatchMs          = 30000
	disconnectedTurnMs    = 5000
	maxDisconnectedMisses = 3
	maxIdleMisses         = 5
	publicEndMs           = 15000
	privateRematchMs      = 60000
	emoteCooldownMs       = 1500
	hammerCooldownMs      = 600
	defaultTurnSeconds    = 15
)

// Settings are chosen when creating a private room.
type Settings struct {
	TurnSeconds int  `json:"turn_seconds"`
	Emotes      bool `json:"emotes"`
	Spectators  bool `json:"spectators"`
	Mic         bool `json:"mic"`
}

// Seat is a player's place at the table plus connection bookkeeping.
type Seat struct {
	Player        *Player        `json:"player"`
	Presence      *game.Presence `json:"presence,omitempty"`
	Connected     bool           `json:"connected"`
	DiscMisses    int            `json:"disc_misses"`
	IdleMisses    int            `json:"idle_misses"`
	Escrowed      bool           `json:"escrowed"`
	ReservedUntil int64          `json:"reserved_until"`
	LastEmoteMs   int64          `json:"last_emote_ms"`
	LastHammerMs  int64          `json:"last_hammer_ms"`
	BotHammerAt   int64          `json:"bot_hammer_at"`
	Rematch       bool           `json:"rematch"`
}

// State is the whole match, serialised for live handover and crash restore.
type State struct {
	Schema     int                      `json:"schema"`
	MatchID    string                   `json:"match_id"`
	Mode       string                   `json:"mode"`
	Format     string                   `json:"format"`
	Fee        int64                    `json:"fee"`
	Private    bool                     `json:"private"`
	RoomCode   string                   `json:"room_code,omitempty"`
	HostID     string                   `json:"host_id,omitempty"`
	Settings   Settings                 `json:"settings"`
	Status     string                   `json:"status"`
	Round      int                      `json:"round"`
	CreatedMs  int64                    `json:"created_ms"`
	StartAtMs  int64                    `json:"start_at_ms"`
	StartedMs  int64                    `json:"started_ms"`
	EndAtMs    int64                    `json:"end_at_ms"`
	Seats      []*Seat                  `json:"seats"`
	Spectators map[string]game.Presence `json:"spectators,omitempty"`
	Table      *Table                   `json:"table,omitempty"`
	Result     []ResultRow              `json:"result,omitempty"`
	OverReason string                   `json:"over_reason,omitempty"`
	Settled    bool                     `json:"settled"`
	BotKey     string                   `json:"bot_key,omitempty"`
	BotActAt   int64                    `json:"bot_act_at"`
	Label      string                   `json:"label"`
	// Rev increases with every change sent to clients; a client that ever
	// sees it go down has seen a rollback.
	Rev int64 `json:"rev"`
}

// Match implements game.Match.
type Match struct {
	nk game.NK
	s  *State
}

// Factory registers the match with the host.
type Factory struct{}

func (Factory) Name() string          { return Module }
func (Factory) MaxSchemaVersion() int { return StateSchemaVersion }
func (Factory) New(nk game.NK) game.Match {
	return &Match{nk: nk}
}

func (Factory) Restore(nk game.NK, schema int, data []byte) (game.Match, error) {
	if schema < 1 || schema > StateSchemaVersion {
		return nil, fmt.Errorf("tango: unsupported state schema %d", schema)
	}
	s := &State{}
	if err := json.Unmarshal(data, s); err != nil {
		return nil, err
	}
	s.relink()
	if s.Table != nil {
		s.Table.SetRNG(nk)
	}
	return &Match{nk: nk, s: s}, nil
}

// relink makes seats and table share the same Player objects after decoding.
func (s *State) relink() {
	if s.Table == nil {
		return
	}
	for i, st := range s.Seats {
		if i < len(s.Table.Players) {
			st.Player = s.Table.Players[i]
		}
	}
}

func (m *Match) SchemaVersion() int { return StateSchemaVersion }
func (m *Match) MarshalState() ([]byte, error) {
	m.s.Schema = StateSchemaVersion
	return json.Marshal(m.s)
}

// State exposes the state for tests.
func (m *Match) State() *State { return m.s }

func numParam(v any, def int64) int64 {
	if f, ok := game.ToNumber(v); ok {
		return int64(f)
	}
	return def
}

func boolParam(v any, def bool) bool {
	if b, ok := v.(bool); ok {
		return b
	}
	return def
}

func strParam(v any, def string) string {
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	return def
}

func (m *Match) Init(mc *game.MatchContext, params map[string]any) (int, string) {
	mode := strParam(params["mode"], ModeClassic)
	if !ValidMode(mode) {
		mode = ModeClassic
	}
	format := strParam(params["format"], Format1v1)
	if !ValidFormat(format) {
		format = Format1v1
	}
	st := Settings{TurnSeconds: defaultTurnSeconds, Emotes: true}
	if sm, ok := params["settings"].(map[string]any); ok {
		st.TurnSeconds = int(numParam(sm["turn_seconds"], defaultTurnSeconds))
		st.Emotes = boolParam(sm["emotes"], true)
		st.Spectators = boolParam(sm["spectators"], false)
		st.Mic = boolParam(sm["mic"], false)
	}
	if st.TurnSeconds < 5 || st.TurnSeconds > 60 {
		st.TurnSeconds = defaultTurnSeconds
	}
	fee := numParam(params["fee"], 0)
	if fee < 0 {
		fee = 0
	}
	m.s = &State{Schema: StateSchemaVersion, MatchID: mc.MatchID, Mode: mode, Format: format, Fee: fee,
		Private: boolParam(params["private"], false), RoomCode: strParam(params["room_code"], ""), HostID: strParam(params["host_id"], ""),
		Settings: st, Status: StatusWaiting, CreatedMs: m.nk.NowMs(), Spectators: map[string]game.Presence{}}
	return tickRate, m.label()
}

func (m *Match) maxSeats() int { return FormatPlayers(m.s.Format) }

func (m *Match) label() string {
	s := m.s
	l := game.Encode(map[string]any{"game": "tango", "mode": s.Mode, "format": s.Format, "fee": s.Fee, "private": s.Private,
		"status": s.Status, "players": len(s.Seats), "max": m.maxSeats(), "room_code": s.RoomCode})
	s.Label = l
	return l
}

func (m *Match) updateLabel(d game.Dispatcher) {
	old := m.s.Label
	if l := m.label(); l != old {
		d.UpdateLabel(l)
	}
}

func (m *Match) seatOf(userID string) int {
	for i, st := range m.s.Seats {
		if st.Player.UserID == userID {
			return i
		}
	}
	return -1
}

func (m *Match) ctx(mc *game.MatchContext) context.Context {
	if mc != nil && mc.Ctx != nil {
		return mc.Ctx
	}
	return context.Background()
}

// ------------------------------------------------------------- joining

func (m *Match) JoinAttempt(mc *game.MatchContext, d game.Dispatcher, tick int64, p game.Presence, metadata map[string]string) (bool, string) {
	s := m.s
	if i := m.seatOf(p.UserID); i >= 0 {
		if s.Seats[i].Player.Removed {
			return false, "REMOVED"
		}
		return true, ""
	}
	if s.Status != StatusWaiting {
		if s.Private && s.Settings.Spectators {
			return true, ""
		}
		return false, "MATCH_STARTED"
	}
	if metadata["spectate"] == "true" {
		return s.Private && s.Settings.Spectators, "SPECTATORS_OFF"
	}
	if len(s.Seats) >= m.maxSeats() {
		return false, "MATCH_FULL"
	}
	ctx := m.ctx(mc)
	if s.Fee > 0 {
		acct, err := m.nk.AccountGet(ctx, p.UserID)
		if err != nil {
			return false, "ACCOUNT_ERROR"
		}
		if acct.Wallet["coins"] < s.Fee {
			return false, "INSUFFICIENT_COINS"
		}
	}
	prof, _, err := game.LoadProfile(ctx, m.nk, p.UserID)
	if err != nil {
		return false, "PROFILE_ERROR"
	}
	name := prof.Name
	if name == "" {
		name = p.Username
	}
	pl := &Player{UserID: p.UserID, Name: name, Avatar: prof.Avatar, Frame: prof.Frame, Badge: prof.Badge, CardBack: prof.CardBack, Trophies: prof.Trophies}
	s.Seats = append(s.Seats, &Seat{Player: pl, ReservedUntil: m.nk.NowMs() + reserveMs})
	m.updateLabel(d)
	return true, ""
}

func (m *Match) Join(mc *game.MatchContext, d game.Dispatcher, tick int64, presences []game.Presence) {
	s := m.s
	for _, p := range presences {
		p := p
		i := m.seatOf(p.UserID)
		if i < 0 {
			s.Spectators[p.SessionID] = p
			m.sendSnapshot(d, p, -1)
			continue
		}
		st := s.Seats[i]
		st.Presence, st.Connected, st.ReservedUntil, st.DiscMisses = &p, true, 0, 0
		if s.Status == StatusWaiting || s.Status == StatusStarting {
			continue
		}
		m.sendSnapshot(d, p, i)
		m.broadcast(d, OpPlayerStatus, map[string]any{"type": "player_connected", "seat": i}, nil)
	}
	if s.Status == StatusWaiting {
		m.maybeStart(d, m.nk.NowMs())
		m.broadcastLobby(d)
	}
}

func (m *Match) Leave(mc *game.MatchContext, d game.Dispatcher, tick int64, presences []game.Presence) {
	s := m.s
	for _, p := range presences {
		if _, ok := s.Spectators[p.SessionID]; ok {
			delete(s.Spectators, p.SessionID)
			continue
		}
		i := m.seatOf(p.UserID)
		if i < 0 {
			continue
		}
		st := s.Seats[i]
		if st.Presence != nil && st.Presence.SessionID != p.SessionID {
			continue // an older socket of a player who already rejoined
		}
		if s.Status == StatusWaiting {
			s.Seats = append(s.Seats[:i], s.Seats[i+1:]...)
			continue
		}
		st.Presence, st.Connected = nil, false
		m.broadcast(d, OpPlayerStatus, map[string]any{"type": "player_disconnected", "seat": i}, nil)
	}
	if s.Status == StatusWaiting {
		m.updateLabel(d)
		m.broadcastLobby(d)
	}
}

func (m *Match) maybeStart(d game.Dispatcher, now int64) {
	s := m.s
	if s.Status != StatusWaiting || len(s.Seats) < m.maxSeats() {
		return
	}
	for _, st := range s.Seats {
		if !st.Player.Bot && !st.Connected {
			return
		}
	}
	s.Status, s.StartAtMs = StatusStarting, now+startCountdownMs
	m.updateLabel(d)
}

func (m *Match) fillBots() {
	s := m.s
	taken := map[string]bool{}
	for _, st := range s.Seats {
		taken[st.Player.Name] = true
	}
	for len(s.Seats) < m.maxSeats() {
		b := NewBotPlayer(m.nk, m.nk.UUID(), s.Fee, taken)
		taken[b.Name] = true
		s.Seats = append(s.Seats, &Seat{Player: b, Connected: true})
	}
}

// ------------------------------------------------------------- loop

func (m *Match) Loop(mc *game.MatchContext, d game.Dispatcher, tick int64, messages []game.MatchMessage) bool {
	s := m.s
	now := m.nk.NowMs()
	ctx := m.ctx(mc)
	for _, msg := range messages {
		m.handle(ctx, d, msg, now)
	}
	switch s.Status {
	case StatusWaiting:
		kept := s.Seats[:0]
		for _, st := range s.Seats {
			if st.ReservedUntil > 0 && !st.Connected && now > st.ReservedUntil {
				continue // reserved a seat but never joined
			}
			kept = append(kept, st)
		}
		if len(kept) != len(s.Seats) {
			s.Seats = kept
			m.broadcastLobby(d)
		}
		if len(s.Seats) == 0 {
			if now-s.CreatedMs > emptyMatchMs {
				return false
			}
		} else if !s.Private && now-s.CreatedMs >= lobbyWaitMs && m.connectedHumans() > 0 {
			m.fillBots()
			m.maybeStart(d, now)
			m.broadcastLobby(d)
		}
		m.updateLabel(d)
	case StatusStarting:
		if now >= s.StartAtMs {
			m.startRound(ctx, d, now)
		}
	case StatusPlaying:
		m.playTick(ctx, d, now)
	case StatusOver:
		if now >= s.EndAtMs {
			return false
		}
	}
	return true
}

func (m *Match) connectedHumans() int {
	n := 0
	for _, st := range m.s.Seats {
		if !st.Player.Bot && st.Connected {
			n++
		}
	}
	return n
}

func (m *Match) handle(ctx context.Context, d game.Dispatcher, msg game.MatchMessage, now int64) {
	s := m.s
	data := map[string]any{}
	if msg.HasData && len(msg.Data) > 0 {
		if v, ok := game.DecodeObject(string(msg.Data)); ok {
			data = v
		}
	}
	seat := m.seatOf(msg.Sender.UserID)
	if msg.OpCode == OpRequestSnapshot || data["action"] == "REQUEST_SNAPSHOT" {
		m.sendSnapshot(d, msg.Sender, seat)
		return
	}
	if seat < 0 {
		return // spectators only watch
	}
	st := s.Seats[seat]
	switch msg.OpCode {
	case OpSendEmote:
		id, _ := data["id"].(string)
		if !s.Settings.Emotes || id == "" || len(id) > 40 {
			m.sendError(d, msg.Sender, "EMOTES_OFF")
			return
		}
		if now-st.LastEmoteMs < emoteCooldownMs {
			return
		}
		st.LastEmoteMs = now
		m.broadcast(d, OpEmote, map[string]any{"type": "emote", "seat": seat, "id": id}, nil)
		return
	case OpStart:
		if s.Private && s.Status == StatusWaiting && msg.Sender.UserID == s.HostID && len(s.Seats) >= minPlayers {
			m.fillBots()
			m.maybeStart(d, now)
			m.broadcastLobby(d)
		}
		return
	case OpRematchVote:
		m.voteRematch(d, seat, now)
		return
	}
	if s.Status != StatusPlaying || s.Table == nil {
		m.sendError(d, msg.Sender, "NOT_PLAYING")
		return
	}
	t := s.Table
	var err error
	switch msg.OpCode {
	case OpPlay:
		if data["tango"] == true {
			_ = t.PressTango(seat)
		}
		err = t.Play(seat, int(numParam(data["card_id"], -1)), strParam(data["color"], ""), now)
	case OpDraw:
		err = t.Draw(seat, now)
	case OpKeepOrPlay:
		if data["tango"] == true {
			_ = t.PressTango(seat)
		}
		err = t.KeepOrPlay(seat, data["play"] == true, strParam(data["color"], ""), now)
	case OpChooseColor:
		err = t.ChooseColor(seat, strParam(data["color"], ""), now)
	case OpChallengeTop:
		err = t.ChallengeTop(seat, now)
	case OpPressTango:
		err = t.PressTango(seat)
	case OpHammerTap:
		if now-st.LastHammerMs < hammerCooldownMs {
			return
		}
		st.LastHammerMs = now
		err = t.Hammer(seat, int(numParam(data["seat"], -1)))
	case OpForfeit:
		t.Remove(seat, "forfeit", m.nk, now)
	default:
		err = ruleErr("UNKNOWN_OP")
	}
	if err != nil {
		m.sendError(d, msg.Sender, ErrorCode(err))
		return
	}
	st.IdleMisses = 0
	m.flush(d)
	if t.Over {
		m.finishRound(ctx, d, now)
	}
}

func (m *Match) playTick(ctx context.Context, d game.Dispatcher, now int64) {
	s := m.s
	t := s.Table
	if t.Over {
		m.finishRound(ctx, d, now)
		return
	}
	// Everyone human has left for good: end now; the bots' result stands.
	humans := 0
	for _, st := range s.Seats {
		if !st.Player.Bot && !st.Player.Removed {
			humans++
		}
	}
	if humans == 0 {
		best, bestPts := -1, 1<<30
		for _, p := range t.Players {
			if !p.Removed && handPoints(p.Hand) < bestPts {
				best, bestPts = p.Team, handPoints(p.Hand)
			}
		}
		t.finish(-1, best, "humans_left")
		m.finishRound(ctx, d, now)
		return
	}
	m.botTick(now)
	m.botHammers(now)
	// Disconnected players get a short clock so the table keeps moving.
	ps := s.Seats[t.PhaseSeat]
	if !ps.Player.Bot && !ps.Connected && t.Deadline > now+disconnectedTurnMs {
		t.Deadline = now + disconnectedTurnMs
	}
	if seat, fired := t.Timeout(now, m.nk); fired {
		st := s.Seats[seat]
		if !st.Player.Bot {
			if st.Connected {
				st.IdleMisses++
			} else {
				st.DiscMisses++
			}
			if st.DiscMisses >= maxDisconnectedMisses {
				t.Remove(seat, "disconnected", m.nk, now)
			} else if st.IdleMisses >= maxIdleMisses {
				t.Remove(seat, "idle", m.nk, now)
			}
		}
	}
	m.flush(d)
	if t.Over {
		m.finishRound(ctx, d, now)
	}
}

func (m *Match) botTick(now int64) {
	s := m.s
	t := s.Table
	if t.Over {
		return
	}
	seat := t.PhaseSeat
	if !s.Seats[seat].Player.Bot {
		return
	}
	key := fmt.Sprintf("%d:%s:%d", seat, t.Phase, t.Deadline)
	if s.BotKey != key {
		s.BotKey, s.BotActAt = key, now+int64(m.nk.RandInt(900, 2200))
		return
	}
	if now < s.BotActAt {
		return
	}
	mv := DecideBot(t, seat, m.nk)
	if mv.Tango {
		_ = t.PressTango(seat)
	}
	var err error
	switch mv.Kind {
	case "play":
		err = t.Play(seat, mv.CardID, mv.Color, now)
	case "color":
		err = t.ChooseColor(seat, mv.Color, now)
	case "play_drawn":
		err = t.KeepOrPlay(seat, true, mv.Color, now)
	case "keep":
		err = t.KeepOrPlay(seat, false, "", now)
	case "challenge":
		err = t.ChallengeTop(seat, now)
	default:
		err = t.Draw(seat, now)
	}
	if err != nil && t.Phase == PhaseTurn && t.Turn == seat {
		_ = t.Draw(seat, now)
	}
}

// botHammers: a bot opponent catches a forgotten TANGO! about half the time,
// after a human-like delay.
func (m *Match) botHammers(now int64) {
	s := m.s
	t := s.Table
	for i, st := range s.Seats {
		p := st.Player
		if !p.Vulnerable {
			st.BotHammerAt = 0
			continue
		}
		var bots []int
		for j, o := range s.Seats {
			if o.Player.Bot && !o.Player.Removed && o.Player.Team != p.Team {
				bots = append(bots, j)
			}
		}
		if len(bots) == 0 {
			continue
		}
		if st.BotHammerAt == 0 {
			if m.nk.RandInt(1, 100) <= 55 {
				st.BotHammerAt = now + int64(m.nk.RandInt(1500, 4000))
			} else {
				st.BotHammerAt = -1 // this time the bots miss it
			}
			continue
		}
		if st.BotHammerAt > 0 && now >= st.BotHammerAt {
			st.BotHammerAt = -1
			_ = t.Hammer(bots[m.nk.RandInt(0, len(bots)-1)], i)
		}
	}
}

// ------------------------------------------------------------- rounds

func (m *Match) startRound(ctx context.Context, d game.Dispatcher, now int64) {
	s := m.s
	s.Round++
	s.Result, s.Settled, s.OverReason = nil, false, ""
	for i, st := range s.Seats {
		st.Rematch, st.IdleMisses, st.DiscMisses, st.Escrowed, st.BotHammerAt = false, 0, 0, false, 0
		st.Player.Seat, st.Player.Team = i, TeamOf(s.Format, i)
		st.Player.Removed, st.Player.RemovedReason = false, ""
	}
	m.escrow(ctx, d)
	players := make([]*Player, len(s.Seats))
	for i, st := range s.Seats {
		players[i] = st.Player
	}
	s.Table = NewTable(s.Mode, s.Format, int64(s.Settings.TurnSeconds)*1000, players, m.nk, now)
	s.Table.TakeEvents()
	s.Status, s.StartedMs = StatusPlaying, now
	for _, st := range s.Seats {
		if !st.Player.Bot {
			_, _ = m.nk.StorageWrite(ctx, []*game.Write{{Collection: "active_match", Key: "current", UserID: st.Player.UserID,
				Value: map[string]any{"match_id": s.MatchID, "round": s.Round, "mode": s.Mode, "format": s.Format}, PermissionRead: 1, PermissionWrite: 0}})
		}
	}
	m.updateLabel(d)
	m.recordStart(now)
	for _, r := range m.recipients() {
		m.send(d, r.p, OpGameStart, map[string]any{"type": "game_start", "seat": s.Table.Turn, "state": m.view(r.seat)})
	}
}

// escrow takes the entry fee from every human at the start of a round. The
// storage write and the wallet change are one transaction, and the storage
// key makes it impossible to charge the same round twice. A player who can
// no longer pay is replaced by a bot.
func (m *Match) escrow(ctx context.Context, d game.Dispatcher) {
	s := m.s
	if s.Fee <= 0 {
		return
	}
	taken := map[string]bool{}
	for _, st := range s.Seats {
		taken[st.Player.Name] = true
	}
	for _, st := range s.Seats {
		p := st.Player
		if p.Bot {
			continue
		}
		key := s.MatchID + ":" + strconv.Itoa(s.Round)
		err := m.nk.MultiUpdate(ctx,
			[]*game.Write{{Collection: "match_escrow", Key: key, UserID: p.UserID, Value: map[string]any{"fee": s.Fee}, Version: "*"}},
			[]*game.WalletUpdate{{UserID: p.UserID, Changeset: map[string]int64{"coins": -s.Fee}, Metadata: map[string]any{"reason": "match_entry", "match_id": s.MatchID, "round": s.Round}}},
			true)
		if err == nil {
			st.Escrowed = true
			continue
		}
		m.nk.Logger().Warn("entry fee failed; seat goes to a bot", "user_id", p.UserID, "error", err.Error())
		if st.Presence != nil {
			m.send(d, *st.Presence, OpError, map[string]any{"code": "INSUFFICIENT_COINS"})
			d.Kick([]game.Presence{*st.Presence})
		}
		b := NewBotPlayer(m.nk, m.nk.UUID(), s.Fee, taken)
		b.Seat, b.Team = p.Seat, p.Team
		st.Player, st.Presence, st.Connected = b, nil, true
	}
}

func (m *Match) finishRound(ctx context.Context, d game.Dispatcher, now int64) {
	s := m.s
	if s.Status == StatusOver {
		return
	}
	t := s.Table
	s.Result = ComputeResult(t, s.Fee)
	s.OverReason = t.OverReason
	if !s.Settled {
		m.settle(ctx)
		s.Settled = true
	}
	s.Status = StatusOver
	if s.Private {
		s.EndAtMs = now + privateRematchMs
	} else {
		s.EndAtMs = now + publicEndMs
	}
	for _, st := range s.Seats {
		if !st.Player.Bot {
			_ = m.nk.StorageDelete(ctx, []game.DeleteID{{Collection: "active_match", Key: "current", UserID: st.Player.UserID}})
		}
	}
	m.recordEnd(now)
	m.updateLabel(d)
	for _, r := range m.recipients() {
		m.send(d, r.p, OpGameOver, map[string]any{"type": "game_over", "reason": s.OverReason, "winner_seat": t.Winner,
			"winner_team": t.WinTeam, "results": m.publicResults(), "rematch_allowed": s.Private, "state": m.view(r.seat)})
	}
}

func (m *Match) publicResults() []map[string]any {
	out := make([]map[string]any, 0, len(m.s.Result))
	for _, r := range m.s.Result {
		out = append(out, map[string]any{"seat": r.Seat, "rank": r.Rank, "user_id": r.UserID, "name": r.Name, "avatar": r.Avatar,
			"frame": r.Frame, "team": r.Team, "points": r.Points, "winner": r.Winner, "removed": r.Removed, "coins": r.Coins,
			"trophies": r.Trophies, "level": LevelFor(m.trophiesAfter(r))})
	}
	return out
}

func (m *Match) trophiesAfter(r ResultRow) int {
	for _, st := range m.s.Seats {
		if st.Player.Seat == r.Seat {
			return st.Player.Trophies
		}
	}
	return 0
}

// settle pays out coins and trophies once per round (idempotent by key).
func (m *Match) settle(ctx context.Context) {
	s := m.s
	key := s.MatchID + ":" + strconv.Itoa(s.Round)
	for _, r := range s.Result {
		st := s.Seats[r.Seat]
		if st.Player.Bot {
			st.Player.Trophies = max(0, st.Player.Trophies+r.Trophies)
			continue
		}
		var wallets []*game.WalletUpdate
		if st.Escrowed {
			if refund := s.Fee + r.Coins; refund > 0 {
				wallets = append(wallets, &game.WalletUpdate{UserID: r.UserID, Changeset: map[string]int64{"coins": refund},
					Metadata: map[string]any{"reason": "match_settle", "match_id": s.MatchID, "round": s.Round, "rank": r.Rank, "delta": r.Coins}})
			}
		}
		err := m.nk.MultiUpdate(ctx,
			[]*game.Write{{Collection: "match_settle", Key: key, UserID: r.UserID, Value: map[string]any{"coins": r.Coins, "trophies": r.Trophies, "rank": r.Rank}, Version: "*"}},
			wallets, true)
		if err != nil {
			m.nk.Logger().Warn("settle skipped", "user_id", r.UserID, "error", err.Error())
			continue
		}
		p, err := game.UpdateProfile(ctx, m.nk, r.UserID, func(p *game.Profile) error {
			p.Trophies = max(0, p.Trophies+r.Trophies)
			p.Games++
			if r.Winner {
				p.Wins++
			} else {
				p.Losses++
			}
			return nil
		})
		if err != nil {
			m.nk.Logger().Warn("profile update failed", "user_id", r.UserID, "error", err.Error())
			continue
		}
		st.Player.Trophies = p.Trophies
		_, _ = m.nk.LeaderboardRecordWrite(ctx, LeaderboardTrophies, r.UserID, st.Player.Name, int64(p.Trophies), 0, nil)
		if r.Trophies > 0 {
			_, _ = m.nk.LeaderboardRecordWrite(ctx, LeaderboardWeekly, r.UserID, st.Player.Name, int64(r.Trophies), 0, nil)
		}
	}
}

// Leaderboards created at startup.
const (
	LeaderboardTrophies = "trophies"        // total trophies (set)
	LeaderboardWeekly   = "weekly_trophies" // trophies won this week (incr, resets Monday)
)

func (m *Match) voteRematch(d game.Dispatcher, seat int, now int64) {
	s := m.s
	if !s.Private || s.Status != StatusOver {
		return
	}
	s.Seats[seat].Rematch = true
	votes, needed := []int{}, 0
	for i, st := range s.Seats {
		if st.Player.Bot {
			continue
		}
		if st.Connected {
			needed++
			if st.Rematch {
				votes = append(votes, i)
			}
		}
	}
	m.broadcast(d, OpRematch, map[string]any{"type": "rematch", "votes": votes, "needed": needed}, nil)
	if needed == 0 || len(votes) < needed {
		return
	}
	// Everyone still here agreed: seats of players who left go to bots.
	kept := s.Seats[:0]
	for _, st := range s.Seats {
		if st.Player.Bot || st.Connected {
			kept = append(kept, st)
		}
	}
	s.Seats = kept
	m.fillBots()
	s.Table, s.Status, s.StartAtMs = nil, StatusStarting, now+startCountdownMs
	m.updateLabel(d)
	m.broadcastLobby(d)
}

func (m *Match) Signal(mc *game.MatchContext, d game.Dispatcher, tick int64, data string) (bool, string) {
	s := m.s
	req, _ := game.DecodeObject(data)
	now := m.nk.NowMs()
	switch req["action"] {
	case "CANCEL_EMPTY_MATCH":
		if len(s.Seats) == 0 {
			return false, `{"cancelled":true}`
		}
		return true, `{"cancelled":false}`
	case "FORFEIT":
		uid, _ := req["user_id"].(string)
		i := m.seatOf(uid)
		if i < 0 || s.Status != StatusPlaying {
			return true, `{"ok":false}`
		}
		s.Table.Remove(i, "forfeit", m.nk, now)
		m.flush(d)
		if s.Table.Over {
			m.finishRound(m.ctx(mc), d, now)
		}
		return true, `{"ok":true}`
	case "FORCE_FINISH":
		// Admin: end the match and give every escrowed fee back.
		m.refundEscrow(m.ctx(mc))
		return false, `{"ok":true}`
	case "INFO":
		return true, game.Encode(map[string]any{"status": s.Status, "players": len(s.Seats), "round": s.Round})
	}
	return true, `{"error":"unknown action"}`
}

func (m *Match) Terminate(mc *game.MatchContext, d game.Dispatcher, tick int64, graceSeconds int) {
	if m.s.Status == StatusPlaying && !m.s.Settled {
		m.refundEscrow(m.ctx(mc))
	}
}

func (m *Match) refundEscrow(ctx context.Context) {
	s := m.s
	if s.Settled || s.Fee <= 0 {
		return
	}
	key := s.MatchID + ":" + strconv.Itoa(s.Round)
	for _, st := range s.Seats {
		if !st.Escrowed || st.Player.Bot {
			continue
		}
		_ = m.nk.MultiUpdate(ctx,
			[]*game.Write{{Collection: "match_settle", Key: key, UserID: st.Player.UserID, Value: map[string]any{"refund": s.Fee}, Version: "*"}},
			[]*game.WalletUpdate{{UserID: st.Player.UserID, Changeset: map[string]int64{"coins": s.Fee}, Metadata: map[string]any{"reason": "match_refund", "match_id": s.MatchID}}},
			true)
		_ = m.nk.StorageDelete(ctx, []game.DeleteID{{Collection: "active_match", Key: "current", UserID: st.Player.UserID}})
	}
	s.Settled = true
}

// ------------------------------------------------------------- analytics

func (m *Match) recordStart(now int64) {
	s := m.s
	f := game.MatchStartFact{MatchID: s.MatchID, Mode: s.Mode + "_" + s.Format, Private: s.Private, EntryFee: s.Fee, StartedAt: time.UnixMilli(now)}
	for i, st := range s.Seats {
		f.Players = append(f.Players, game.MatchPlayerFact{Seat: i, UserID: st.Player.UserID, Bot: st.Player.Bot})
	}
	game.RecordMatchStart(m.nk, f)
}

func (m *Match) recordEnd(now int64) {
	s := m.s
	reason := "normal"
	if s.OverReason == "humans_left" {
		reason = "humans_left"
	}
	f := game.MatchEndFact{MatchID: s.MatchID, EndedAt: time.UnixMilli(now), DurationSec: (now - s.StartedMs) / 1000, EndReason: reason}
	for _, r := range s.Result {
		f.Results = append(f.Results, game.MatchResultFact{Seat: r.Seat, Rank: r.Rank, Forfeited: r.Removed, Prize: r.Coins})
	}
	game.RecordMatchEnd(m.nk, f)
}
