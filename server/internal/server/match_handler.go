package server

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/heroiclabs/nakama-common/rtapi"
	"google.golang.org/protobuf/proto"

	"github.com/dev-sssss/tango_backend/server/internal/game"
)

// PresenceList is the ordered set of presences in a match, keyed by session.
type PresenceList struct {
	list []game.Presence
}

func (p *PresenceList) index(sessionID string) int {
	for i, x := range p.list {
		if x.SessionID == sessionID {
			return i
		}
	}
	return -1
}

func (p *PresenceList) Add(x game.Presence) bool {
	if p.index(x.SessionID) >= 0 {
		return false
	}
	p.list = append(p.list, x)
	return true
}

func (p *PresenceList) Remove(sessionID string) (game.Presence, bool) {
	i := p.index(sessionID)
	if i < 0 {
		return game.Presence{}, false
	}
	x := p.list[i]
	p.list = append(p.list[:i], p.list[i+1:]...)
	return x, true
}

func (p *PresenceList) Get(sessionID string) (game.Presence, bool) {
	i := p.index(sessionID)
	if i < 0 {
		return game.Presence{}, false
	}
	return p.list[i], true
}

func (p *PresenceList) Contains(sessionID string) bool { return p.index(sessionID) >= 0 }
func (p *PresenceList) Len() int                       { return len(p.list) }
func (p *PresenceList) All() []game.Presence           { return append([]game.Presence(nil), p.list...) }

// MatchSnapshot is everything needed to resume a match on another node.
type MatchSnapshot struct {
	MatchID    string          `json:"match_id"`
	Module     string          `json:"module"`
	Schema     int             `json:"schema"`
	State      []byte          `json:"state"`
	Tick       int64           `json:"tick"`
	TickRate   int             `json:"tick_rate"`
	Label      string          `json:"label"`
	CreateTime int64           `json:"create_time"`
	Presences  []game.Presence `json:"presences"`
	// Sessions migrating between gateways and when their deferred leave is due.
	PendingLeaves map[string]int64 `json:"pending_leaves,omitempty"`
	// Inputs received but not yet processed (handover only).
	Inputs []snapshotInput `json:"inputs,omitempty"`
}

type snapshotInput struct {
	Sender        game.Presence `json:"sender"`
	OpCode        int64         `json:"op_code"`
	Data          []byte        `json:"data"`
	HasData       bool          `json:"has_data"`
	Reliable      bool          `json:"reliable"`
	ReceiveTimeMs int64         `json:"receive_time_ms"`
}

func toInput(m game.MatchMessage) snapshotInput {
	return snapshotInput{Sender: m.Sender, OpCode: m.OpCode, Data: m.Data, HasData: m.HasData, Reliable: m.Reliable, ReceiveTimeMs: m.ReceiveTimeMs}
}

func (in snapshotInput) message() game.MatchMessage {
	return game.MatchMessage{Sender: in.Sender, OpCode: in.OpCode, Data: in.Data, HasData: in.HasData, Reliable: in.Reliable, ReceiveTimeMs: in.ReceiveTimeMs}
}

type joinResult struct {
	Allow     bool
	Reason    string
	Label     string
	Presences []game.Presence
}

var errMoved = errors.New("match moved")

// MatchHandler runs one authoritative match on its own goroutine. Every call
// into the game.Match happens on that goroutine.
type MatchHandler struct {
	ID       string // "<uuid>.<creation node>"
	UUID     string
	Module   string
	host     *MatchHost
	match    game.Match
	mc       *game.MatchContext
	logger   *slog.Logger
	tickRate int
	tick     int64
	created  int64
	epoch    atomic.Int64

	label atomic.Value // string
	size  atomic.Int64

	presences     PresenceList
	pendingLeaves map[string]*time.Timer
	pendingDue    map[string]int64

	inMu   sync.Mutex
	inputs []game.MatchMessage

	calls   chan func()
	stopCh  chan struct{}
	stopped atomic.Bool
	paused  atomic.Bool

	// Live handover state.
	mvMu    sync.Mutex
	moving  bool
	mvDone  chan struct{}
	mvBuf   []game.MatchMessage
	movedTo string

	dirty    atomic.Bool
	lastHash [32]byte
}

func newMatchHandler(host *MatchHost, id, uuidStr, module string, m game.Match, tickRate int, label string, tick, created, epoch int64, presences []game.Presence) *MatchHandler {
	if tickRate < 1 {
		tickRate = 1
	}
	h := &MatchHandler{
		ID: id, UUID: uuidStr, Module: module, host: host, match: m,
		mc:            &game.MatchContext{Ctx: context.Background(), MatchID: id, Node: host.node, Env: host.env},
		logger:        host.logger.With("match_id", id),
		tickRate:      tickRate,
		tick:          tick,
		created:       created,
		pendingLeaves: map[string]*time.Timer{},
		pendingDue:    map[string]int64{},
		calls:         make(chan func(), host.cfg.Match.CallQueueSize+host.cfg.Match.JoinAttemptQueueSize),
		stopCh:        make(chan struct{}),
	}
	h.epoch.Store(epoch)
	h.label.Store(label)
	for _, p := range presences {
		h.presences.Add(p)
	}
	h.size.Store(int64(h.presences.Len()))
	return h
}

func (h *MatchHandler) Label() string { return h.label.Load().(string) }

func (h *MatchHandler) start() { go h.run() }

func (h *MatchHandler) run() {
	ticker := time.NewTicker(time.Second / time.Duration(h.tickRate))
	defer ticker.Stop()
	for {
		select {
		case <-h.stopCh:
			return
		case <-ticker.C:
			if !h.paused.Load() {
				h.loop()
			}
		case fn := <-h.calls:
			fn()
		}
	}
}

// guard runs a game call, converting panics into a match stop (Lua runtime
// errors stopped the match the same way).
func (h *MatchHandler) guard(what string, fn func()) (ok bool) {
	defer func() {
		if r := recover(); r != nil {
			h.logger.Error("match handler error, stopping match", "call", what, "error", fmt.Sprint(r), "stack", string(debug.Stack()))
			ok = false
			h.stop(false)
		}
	}()
	fn()
	h.dirty.Store(true)
	h.afterChange()
	return true
}

// afterChange hands a snapshot to the checkpoint writer when the state
// changed. It runs on the match goroutine; the database write is async, so
// a crash loses at most the writes still in flight.
func (h *MatchHandler) afterChange() {
	if h.host.cfg.Cluster.CheckpointIntervalMs < 0 || h.stopped.Load() || h.paused.Load() {
		return
	}
	s, changed := h.snapshotIfChanged()
	if changed {
		h.dirty.Store(false)
		h.host.queueCheckpoint(h, s)
	}
}

// forceNextCheckpoint makes the next change check write even if the state
// is identical to the last snapshot (used after a failed write).
func (h *MatchHandler) forceNextCheckpoint() {
	h.queue(func() { h.lastHash = [32]byte{} })
}

// snapshotIfChanged must run on the match goroutine.
func (h *MatchHandler) snapshotIfChanged() (*MatchSnapshot, bool) {
	s, err := h.snapshotLocked()
	if err != nil {
		h.logger.Warn("match state could not be serialised for checkpointing", "error", err)
		return nil, false
	}
	b, _ := json.Marshal(struct {
		S []byte
		P []game.Presence
		L string
		D map[string]int64
	}{s.State, s.Presences, s.Label, s.PendingLeaves})
	sum := sha256.Sum256(b)
	if sum == h.lastHash {
		return nil, false
	}
	h.lastHash = sum
	return s, true
}

func (h *MatchHandler) loop() {
	if h.stopped.Load() {
		return
	}
	h.inMu.Lock()
	msgs := h.inputs
	h.inputs = nil
	h.inMu.Unlock()
	keep := true
	if !h.guard("match_loop", func() { keep = h.match.Loop(h.mc, h, h.tick, msgs) }) {
		return
	}
	h.tick++
	if !keep {
		h.logger.Info("match loop ended the match")
		h.stop(false)
	}
}

// enter blocks while a handover is in progress. It returns errMoved if the
// match now lives elsewhere.
func (h *MatchHandler) enter() error {
	for {
		h.mvMu.Lock()
		if h.movedTo != "" {
			h.mvMu.Unlock()
			return errMoved
		}
		if !h.moving {
			h.mvMu.Unlock()
			return nil
		}
		ch := h.mvDone
		h.mvMu.Unlock()
		<-ch
	}
}

func (h *MatchHandler) queue(fn func()) bool {
	if h.stopped.Load() {
		return false
	}
	select {
	case h.calls <- fn:
		return true
	default:
		h.logger.Warn("match call queue full")
		return false
	}
}

func (h *MatchHandler) QueueData(m game.MatchMessage) error {
	h.mvMu.Lock()
	if h.movedTo != "" {
		h.mvMu.Unlock()
		return errMoved
	}
	if h.moving {
		h.mvBuf = append(h.mvBuf, m)
		h.mvMu.Unlock()
		return nil
	}
	h.mvMu.Unlock()
	if h.stopped.Load() {
		return errMatchNotFound
	}
	h.inMu.Lock()
	if len(h.inputs) < h.host.cfg.Match.InputQueueSize {
		h.inputs = append(h.inputs, m)
	}
	h.inMu.Unlock()
	return nil
}

func (h *MatchHandler) JoinAttempt(ctx context.Context, p game.Presence, metadata map[string]string) (*joinResult, error) {
	if err := h.enter(); err != nil {
		return nil, err
	}
	ch := make(chan *joinResult, 1)
	if !h.queue(func() {
		select {
		case <-ctx.Done():
			ch <- &joinResult{Allow: false}
			return
		default:
		}
		// A returning user replaces a migrating session silently.
		for sid, t := range h.pendingLeaves {
			if old, ok := h.presences.Get(sid); ok && old.UserID == p.UserID {
				t.Stop()
				delete(h.pendingLeaves, sid)
				delete(h.pendingDue, sid)
				h.presences.Remove(sid)
			}
		}
		allow, reason := false, ""
		if !h.guard("match_join_attempt", func() { allow, reason = h.match.JoinAttempt(h.mc, h, h.tick, p, metadata) }) {
			ch <- &joinResult{Allow: false}
			return
		}
		res := &joinResult{Allow: allow, Reason: reason, Label: h.Label()}
		if allow {
			res.Presences = h.presences.All()
			if h.presences.Add(p) {
				h.size.Store(int64(h.presences.Len()))
				h.host.sizeChanged(h)
				// match_join runs as its own call, after the joiner has been
				// told the join succeeded (Nakama does the same).
				h.queue(func() {
					if !h.presences.Contains(p.SessionID) {
						return
					}
					h.guard("match_join", func() { h.match.Join(h.mc, h, h.tick, []game.Presence{p}) })
					h.presenceEvent([]game.Presence{p}, nil)
				})
			}
		}
		ch <- res
	}) {
		return nil, errMatchNotFound
	}
	select {
	case r := <-ch:
		return r, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-h.stopCh:
		return nil, errMatchNotFound
	}
}

// QueueLeave processes leaves. migrating=true defers the leave for the
// presence grace period so that a gateway restart is invisible to the game.
func (h *MatchHandler) QueueLeave(ps []game.Presence, migrating bool) error {
	if err := h.enter(); err != nil {
		return err
	}
	ok := h.queue(func() {
		if migrating && h.host.presenceGrace > 0 {
			for _, p := range ps {
				if !h.presences.Contains(p.SessionID) {
					continue
				}
				h.deferLeave(p.SessionID, time.Now().Add(h.host.presenceGrace))
			}
			return
		}
		h.processLeaves(ps)
	})
	if !ok {
		return errMatchNotFound
	}
	return nil
}

func (h *MatchHandler) deferLeave(sid string, due time.Time) {
	if _, ok := h.pendingLeaves[sid]; ok {
		return
	}
	h.pendingDue[sid] = due.UnixMilli()
	h.pendingLeaves[sid] = time.AfterFunc(time.Until(due), func() {
		h.queue(func() {
			if _, ok := h.pendingLeaves[sid]; !ok {
				return
			}
			p, ok := h.presences.Get(sid)
			delete(h.pendingLeaves, sid)
			delete(h.pendingDue, sid)
			if ok {
				h.processLeaves([]game.Presence{p})
			}
		})
	})
}

func (h *MatchHandler) processLeaves(ps []game.Presence) {
	var processed []game.Presence
	for _, p := range ps {
		if t, ok := h.pendingLeaves[p.SessionID]; ok {
			t.Stop()
			delete(h.pendingLeaves, p.SessionID)
			delete(h.pendingDue, p.SessionID)
		}
		if x, ok := h.presences.Remove(p.SessionID); ok {
			processed = append(processed, x)
		}
	}
	if len(processed) == 0 {
		return
	}
	h.size.Store(int64(h.presences.Len()))
	h.host.sizeChanged(h)
	h.guard("match_leave", func() { h.match.Leave(h.mc, h, h.tick, processed) })
	h.presenceEvent(nil, processed)
}

// RemoveNodePresences leaves every presence held by a dead gateway node.
func (h *MatchHandler) RemoveNodePresences(node string) {
	h.queue(func() {
		var gone []game.Presence
		for _, p := range h.presences.list {
			if p.Node == node {
				gone = append(gone, p)
			}
		}
		h.processLeaves(gone)
	})
}

func (h *MatchHandler) Signal(ctx context.Context, data string) (string, error) {
	if err := h.enter(); err != nil {
		return "", err
	}
	type res struct {
		resp string
		ok   bool
	}
	ch := make(chan res, 1)
	if !h.queue(func() {
		keep, resp := true, ""
		if !h.guard("match_signal", func() { keep, resp = h.match.Signal(h.mc, h, h.tick, data) }) {
			ch <- res{ok: false}
			return
		}
		if !keep {
			h.stop(false)
			ch <- res{ok: false}
			return
		}
		ch <- res{resp: resp, ok: true}
	}) {
		return "", errMatchNotFound
	}
	select {
	case r := <-ch:
		if !r.ok {
			return "", errMatchNotFound
		}
		return r.resp, nil
	case <-ctx.Done():
		return "", ctx.Err()
	case <-h.stopCh:
		return "", errMatchNotFound
	}
}

// stop ends the match. terminate calls match_terminate first (used only
// when a node must shut down without being able to hand matches over).
func (h *MatchHandler) stop(terminate bool) {
	if !h.stopped.CompareAndSwap(false, true) {
		return
	}
	if terminate {
		func() {
			defer func() { _ = recover() }()
			h.match.Terminate(h.mc, h, h.tick, 0)
		}()
	}
	for _, t := range h.pendingLeaves {
		t.Stop()
	}
	close(h.stopCh)
	h.host.onStopped(h)
}

// ---------------------------------------------------------------- dispatcher

func (h *MatchHandler) Broadcast(op int64, data []byte, to []game.Presence) {
	var targets []game.Presence
	if to == nil {
		targets = h.presences.All()
	} else {
		for _, p := range to {
			if x, ok := h.presences.Get(p.SessionID); ok {
				targets = append(targets, x)
			}
		}
	}
	if len(targets) == 0 {
		return
	}
	env := &rtapi.Envelope{Message: &rtapi.Envelope_MatchData{MatchData: &rtapi.MatchData{MatchId: h.ID, OpCode: op, Data: data, Reliable: true}}}
	h.host.deliver(env, targets)
}

func (h *MatchHandler) presenceEvent(joins, leaves []game.Presence) {
	targets := h.presences.All()
	if len(targets) == 0 {
		return
	}
	conv := func(ps []game.Presence) []*rtapi.UserPresence {
		out := make([]*rtapi.UserPresence, len(ps))
		for i, p := range ps {
			out[i] = &rtapi.UserPresence{UserId: p.UserID, SessionId: p.SessionID, Username: p.Username}
		}
		return out
	}
	env := &rtapi.Envelope{Message: &rtapi.Envelope_MatchPresenceEvent{MatchPresenceEvent: &rtapi.MatchPresenceEvent{MatchId: h.ID, Joins: conv(joins), Leaves: conv(leaves)}}}
	h.host.deliver(env, targets)
}

func (h *MatchHandler) Kick(ps []game.Presence) {
	cp := append([]game.Presence(nil), ps...)
	h.queue(func() { h.processLeaves(cp) })
	h.host.notifyKick(h.ID, cp)
}

func (h *MatchHandler) UpdateLabel(label string) {
	h.label.Store(label)
	h.host.labelChanged(h)
}

// ---------------------------------------------------------------- persistence

func (h *MatchHandler) snapshotLocked() (*MatchSnapshot, error) {
	state, err := h.match.MarshalState()
	if err != nil {
		return nil, err
	}
	s := &MatchSnapshot{MatchID: h.ID, Module: h.Module, Schema: h.match.SchemaVersion(), State: state, Tick: h.tick,
		TickRate: h.tickRate, Label: h.Label(), CreateTime: h.created, Presences: h.presences.All()}
	if len(h.pendingDue) > 0 {
		s.PendingLeaves = map[string]int64{}
		for k, v := range h.pendingDue {
			s.PendingLeaves[k] = v
		}
	}
	return s, nil
}

// Checkpoint returns a snapshot when the state changed since the last one
// written (force ignores that check).
func (h *MatchHandler) Checkpoint(ctx context.Context, force bool) (*MatchSnapshot, error) {
	if h.stopped.Load() || h.paused.Load() {
		return nil, nil
	}
	type res struct {
		s   *MatchSnapshot
		err error
	}
	ch := make(chan res, 1)
	if !h.queue(func() {
		if force {
			h.lastHash = [32]byte{}
		}
		h.dirty.Store(false)
		s, changed := h.snapshotIfChanged()
		if !changed {
			ch <- res{}
			return
		}
		ch <- res{s: s}
	}) {
		return nil, nil
	}
	select {
	case r := <-ch:
		return r.s, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-h.stopCh:
		return nil, nil
	}
}

// beginMove blocks new joins/leaves/signals, buffers data, pauses ticking
// and returns a snapshot that includes unprocessed inputs.
func (h *MatchHandler) beginMove(ctx context.Context) (*MatchSnapshot, error) {
	h.mvMu.Lock()
	if h.moving || h.movedTo != "" {
		h.mvMu.Unlock()
		return nil, errors.New("handover already in progress")
	}
	h.moving = true
	h.mvDone = make(chan struct{})
	h.mvMu.Unlock()
	type res struct {
		s   *MatchSnapshot
		err error
	}
	ch := make(chan res, 1)
	if !h.queue(func() {
		h.paused.Store(true)
		s, err := h.snapshotLocked()
		if err == nil {
			h.inMu.Lock()
			for _, m := range h.inputs {
				s.Inputs = append(s.Inputs, toInput(m))
			}
			h.inputs = nil
			h.inMu.Unlock()
		}
		ch <- res{s, err}
	}) {
		h.abortMove(nil)
		return nil, errMatchNotFound
	}
	select {
	case r := <-ch:
		if r.err != nil {
			h.abortMove(nil)
		}
		return r.s, r.err
	case <-ctx.Done():
		h.abortMove(nil)
		return nil, ctx.Err()
	}
}

// abortMove resumes the match locally; inputs returns snapshot inputs.
func (h *MatchHandler) abortMove(inputs []snapshotInput) {
	h.mvMu.Lock()
	buf := h.mvBuf
	h.mvBuf = nil
	h.moving = false
	done := h.mvDone
	h.mvMu.Unlock()
	h.inMu.Lock()
	var restored []game.MatchMessage
	for _, in := range inputs {
		restored = append(restored, in.message())
	}
	h.inputs = append(append(restored, h.inputs...), buf...)
	h.inMu.Unlock()
	h.paused.Store(false)
	if done != nil {
		close(done)
	}
}

// completeMove marks the match as moved and stops the local copy. It
// returns data received during the move, to be forwarded in order.
func (h *MatchHandler) completeMove(target string) []game.MatchMessage {
	h.mvMu.Lock()
	buf := h.mvBuf
	h.mvBuf = nil
	h.moving = false
	h.movedTo = target
	done := h.mvDone
	h.mvMu.Unlock()
	if h.stopped.CompareAndSwap(false, true) {
		for _, t := range h.pendingLeaves {
			t.Stop()
		}
		close(h.stopCh)
	}
	if done != nil {
		close(done)
	}
	return buf
}

// applyRestore re-arms deferred leaves and queued inputs from a snapshot.
func (h *MatchHandler) applyRestore(s *MatchSnapshot) {
	for sid, due := range s.PendingLeaves {
		h.deferLeave(sid, time.UnixMilli(due))
	}
	h.inMu.Lock()
	for _, in := range s.Inputs {
		h.inputs = append(h.inputs, in.message())
	}
	h.inMu.Unlock()
}

func encodeEnvelope(env *rtapi.Envelope) []byte {
	b, _ := proto.Marshal(env)
	return b
}
