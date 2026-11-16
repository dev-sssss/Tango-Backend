package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/heroiclabs/nakama-common/rtapi"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"

	"github.com/dev-sssss/tango_backend/server/internal/game"
)

// MatchHost owns the matches running on this node and routes every match
// operation in the cluster to the node that owns the match.
type MatchHost struct {
	cfg           *Config
	node          string
	env           map[string]string
	logger        *slog.Logger
	db            *pgxpool.Pool
	factories     map[string]game.MatchFactory
	nk            game.NK
	sessions      *SessionRegistry
	bus           *Bus
	cluster       *Cluster
	metrics       *Metrics
	presenceGrace time.Duration

	mu       sync.RWMutex
	local    map[string]*MatchHandler // by uuid
	routes   map[string]string        // uuid -> owner node (cache)
	forwards map[string]string        // uuid -> node a match moved to

	ckMu      sync.Mutex
	ckPending map[string]*pendingCheckpoint
	ckKick    chan struct{}

	labelMu    sync.Mutex
	labelDirty map[string]*MatchHandler
	labelKick  chan struct{}
}

func NewMatchHost(cfg *Config, logger *slog.Logger, db *pgxpool.Pool, metrics *Metrics) *MatchHost {
	return &MatchHost{
		cfg: cfg, node: cfg.Name, env: cfg.RuntimeEnv(), logger: logger, db: db, metrics: metrics,
		factories:     map[string]game.MatchFactory{},
		presenceGrace: time.Duration(cfg.Cluster.PresenceGraceMs) * time.Millisecond,
		local:         map[string]*MatchHandler{},
		routes:        map[string]string{},
		forwards:      map[string]string{},
		labelDirty:    map[string]*MatchHandler{},
		labelKick:     make(chan struct{}, 1),
		ckPending:     map[string]*pendingCheckpoint{},
		ckKick:        make(chan struct{}, 1),
	}
}

func (m *MatchHost) RegisterFactory(f game.MatchFactory) { m.factories[f.Name()] = f }

func splitMatchID(id string) (string, bool) {
	u, _, _ := strings.Cut(id, ".")
	if _, err := uuid.FromString(u); err != nil {
		return "", false
	}
	return strings.ToLower(u), true
}

func (m *MatchHost) localHandler(u string) *MatchHandler {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.local[u]
}

func (m *MatchHost) LocalCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.local)
}

func (m *MatchHost) LocalHandlers() []*MatchHandler {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*MatchHandler, 0, len(m.local))
	for _, h := range m.local {
		out = append(out, h)
	}
	return out
}

// owner resolves the node that owns a match.
func (m *MatchHost) owner(ctx context.Context, u string) (string, error) {
	m.mu.RLock()
	if _, ok := m.local[u]; ok {
		m.mu.RUnlock()
		return m.node, nil
	}
	if n, ok := m.forwards[u]; ok {
		m.mu.RUnlock()
		return n, nil
	}
	if n, ok := m.routes[u]; ok {
		m.mu.RUnlock()
		return n, nil
	}
	m.mu.RUnlock()
	var node string
	err := m.db.QueryRow(ctx, "SELECT node FROM cluster_match WHERE id = $1", u).Scan(&node)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", errMatchNotFound
	}
	if err != nil {
		return "", err
	}
	m.mu.Lock()
	m.routes[u] = node
	m.mu.Unlock()
	return node, nil
}

func (m *MatchHost) forgetRoute(u string) {
	m.mu.Lock()
	delete(m.routes, u)
	m.mu.Unlock()
}

// ---------------------------------------------------------------- create

func (m *MatchHost) Create(ctx context.Context, module string, params map[string]any) (string, error) {
	if _, ok := m.factories[module]; !ok {
		return "", fmt.Errorf("match handler %q not found", module)
	}
	target, err := m.cluster.PlaceMatch(module)
	if err != nil {
		return "", err
	}
	if target == m.node {
		return m.createLocal(ctx, module, params)
	}
	p, _ := json.Marshal(params)
	resp, err := m.bus.Request(ctx, target, &busMsg{Kind: kCreate, Module: module, Data: p})
	if err != nil {
		// Fall back to hosting it here if possible.
		if m.cluster.IsGame() && !m.cluster.Draining() {
			return m.createLocal(ctx, module, params)
		}
		return "", err
	}
	if resp.Err != "" {
		return "", errors.New(resp.Err)
	}
	return resp.MatchID, nil
}

func (m *MatchHost) createLocal(ctx context.Context, module string, params map[string]any) (id string, err error) {
	f := m.factories[module]
	if f == nil {
		return "", fmt.Errorf("match handler %q not found", module)
	}
	u := uuid.Must(uuid.NewV4()).String()
	id = u + "." + m.node
	match := f.New(m.nk)
	mc := &game.MatchContext{Ctx: ctx, MatchID: id, Node: m.node, Env: m.env}
	var tickRate int
	var label string
	func() {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("match_init failed: %v", r)
			}
		}()
		tickRate, label = match.Init(mc, params)
	}()
	if err != nil {
		return "", err
	}
	if tickRate < 1 || tickRate > 60 {
		return "", errors.New("match_init returned invalid tick rate, must be between 1 and 60")
	}
	now := time.Now()
	if _, err := m.db.Exec(ctx, `INSERT INTO cluster_match (id, node, epoch, module, label, tick_rate, size, create_time, update_time)
		VALUES ($1, $2, 1, $3, $4, $5, 0, $6, $6)`, u, m.node, module, label, tickRate, now); err != nil {
		return "", err
	}
	h := newMatchHandler(m, id, u, module, match, tickRate, label, 0, now.Unix(), 1, nil)
	m.mu.Lock()
	m.local[u] = h
	m.mu.Unlock()
	h.start()
	m.metrics.GaugeAdd("matches_local", nil, 1)
	m.metrics.CounterAdd("matches_created_total", map[string]string{"module": module}, 1)
	return id, nil
}

// ---------------------------------------------------------------- get / signal

func (m *MatchHost) Get(ctx context.Context, id string) (*game.MatchInfo, error) {
	u, ok := splitMatchID(id)
	if !ok {
		return nil, errors.New("invalid match id")
	}
	if h := m.localHandler(u); h != nil && !h.stopped.Load() {
		return &game.MatchInfo{MatchID: id, Label: h.Label(), Size: int(h.size.Load()), TickRate: h.tickRate, Handler: h.Module}, nil
	}
	var label, module string
	var size, rate int
	err := m.db.QueryRow(ctx, "SELECT label, size, tick_rate, module FROM cluster_match WHERE id = $1", u).Scan(&label, &size, &rate, &module)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &game.MatchInfo{MatchID: id, Label: label, Size: size, TickRate: rate, Handler: module}, nil
}

// routed runs fn against the owner, retrying when a match moves.
func (m *MatchHost) routed(ctx context.Context, id string, local func(h *MatchHandler) error, remote func(node string) error) error {
	u, ok := splitMatchID(id)
	if !ok {
		return errMatchNotFound
	}
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		node, err := m.owner(ctx, u)
		if err != nil {
			return err
		}
		if node == m.node {
			h := m.localHandler(u)
			if h == nil {
				// The registry says we own it but it is not loaded: it is
				// being restored after a restart; give it a moment.
				m.forgetRoute(u)
				lastErr = errMatchNotFound
				time.Sleep(50 * time.Millisecond)
				continue
			}
			err = local(h)
		} else {
			err = remote(node)
		}
		if errors.Is(err, errMoved) || errors.Is(err, errNotOwner) || errors.Is(err, errPeerUnavailable) {
			m.forgetRoute(u)
			lastErr = err
			time.Sleep(time.Duration(20*(attempt+1)) * time.Millisecond)
			continue
		}
		return err
	}
	if lastErr == nil {
		lastErr = errMatchNotFound
	}
	return lastErr
}

func (m *MatchHost) Signal(ctx context.Context, id, data string) (string, error) {
	var resp string
	err := m.routed(ctx, id, func(h *MatchHandler) error {
		r, err := h.Signal(ctx, data)
		resp = r
		return err
	}, func(node string) error {
		r, err := m.bus.Request(ctx, node, &busMsg{Kind: kSignal, MatchID: id, Data: []byte(data)})
		if err != nil {
			return err
		}
		if err := r.error(); err != nil {
			return err
		}
		resp = string(r.Data)
		return nil
	})
	if err != nil {
		return "", errMatchNotFound
	}
	return resp, nil
}

// ---------------------------------------------------------------- presence routing

func (m *MatchHost) JoinAttempt(ctx context.Context, id string, p game.Presence, metadata map[string]string) (*joinResult, error) {
	var res *joinResult
	err := m.routed(ctx, id, func(h *MatchHandler) error {
		r, err := h.JoinAttempt(ctx, p, metadata)
		res = r
		return err
	}, func(node string) error {
		r, err := m.bus.Request(ctx, node, &busMsg{Kind: kJoinAttempt, MatchID: id, Presence: p, Metadata: metadata})
		if err != nil {
			return err
		}
		if err := r.error(); err != nil {
			return err
		}
		res = &joinResult{Allow: r.Allow, Reason: r.Reason, Label: r.Label, Presences: r.Presences}
		return nil
	})
	return res, err
}

func (m *MatchHost) SendData(ctx context.Context, id string, msg game.MatchMessage) {
	_ = m.routed(ctx, id, func(h *MatchHandler) error {
		return h.QueueData(msg)
	}, func(node string) error {
		return m.bus.Send(node, &busMsg{Kind: kData, MatchID: id, Presence: msg.Sender, OpCode: msg.OpCode, Data: msg.Data, HasData: msg.HasData, Reliable: msg.Reliable, ReceiveTimeMs: msg.ReceiveTimeMs})
	})
}

func (m *MatchHost) Leave(ctx context.Context, id string, ps []game.Presence, migrating bool) {
	_ = m.routed(ctx, id, func(h *MatchHandler) error {
		return h.QueueLeave(ps, migrating)
	}, func(node string) error {
		return m.bus.Send(node, &busMsg{Kind: kLeave, MatchID: id, Presences: ps, Migrating: migrating})
	})
}

// deliver sends an envelope to presences, grouped by their gateway node.
func (m *MatchHost) deliver(env *rtapi.Envelope, targets []game.Presence) {
	byNode := map[string][]string{}
	for _, p := range targets {
		byNode[p.Node] = append(byNode[p.Node], p.SessionID)
	}
	var raw []byte
	for node, sids := range byNode {
		if node == m.node {
			m.sessions.SendEnvelope(sids, env)
			continue
		}
		if raw == nil {
			raw, _ = proto.Marshal(env)
		}
		if err := m.bus.Send(node, &busMsg{Kind: kDeliver, Sessions: sids, Data: raw}); err != nil {
			m.metrics.CounterAdd("bus_deliver_failures_total", nil, 1)
		}
	}
}

func (m *MatchHost) notifyKick(matchID string, ps []game.Presence) {
	byNode := map[string][]string{}
	for _, p := range ps {
		byNode[p.Node] = append(byNode[p.Node], p.SessionID)
	}
	for node, sids := range byNode {
		if node == m.node {
			m.sessions.DropMembership(sids, matchID)
		} else {
			_ = m.bus.Send(node, &busMsg{Kind: kKick, MatchID: matchID, Sessions: sids})
		}
	}
}

// ---------------------------------------------------------------- label/size persistence

func (m *MatchHost) labelChanged(h *MatchHandler) { m.markLabel(h) }
func (m *MatchHost) sizeChanged(h *MatchHandler)  { m.markLabel(h) }

func (m *MatchHost) markLabel(h *MatchHandler) {
	m.labelMu.Lock()
	m.labelDirty[h.UUID] = h
	m.labelMu.Unlock()
	select {
	case m.labelKick <- struct{}{}:
	default:
	}
}

// runLabelWriter writes label/size changes through to the registry so every
// node sees them (quick join and private rooms read labels cluster-wide).
func (m *MatchHost) runLabelWriter(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.labelKick:
		}
		m.labelMu.Lock()
		batch := m.labelDirty
		m.labelDirty = map[string]*MatchHandler{}
		m.labelMu.Unlock()
		for u, h := range batch {
			if h.stopped.Load() {
				continue
			}
			wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			_, err := m.db.Exec(wctx, "UPDATE cluster_match SET label = $2, size = $3, update_time = now() WHERE id = $1 AND node = $4", u, h.Label(), h.size.Load(), m.node)
			cancel()
			if err != nil {
				m.logger.Warn("label write failed", "match_id", h.ID, "error", err)
				m.markLabel(h)
				time.Sleep(200 * time.Millisecond)
			}
		}
	}
}

// ---------------------------------------------------------------- lifecycle

func (m *MatchHost) onStopped(h *MatchHandler) {
	m.mu.Lock()
	if m.local[h.UUID] == h {
		delete(m.local, h.UUID)
	}
	m.mu.Unlock()
	m.metrics.GaugeAdd("matches_local", nil, -1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	epoch := h.epoch.Load()
	_, _ = m.db.Exec(ctx, "DELETE FROM cluster_match WHERE id = $1 AND node = $2 AND epoch = $3", h.UUID, m.node, epoch)
	_, _ = m.db.Exec(ctx, "DELETE FROM cluster_match_state WHERE id = $1 AND epoch <= $2", h.UUID, epoch)
	// Tell gateways to drop the membership.
	nodes := map[string][]string{}
	for _, p := range h.presences.All() {
		nodes[p.Node] = append(nodes[p.Node], p.SessionID)
	}
	for node, sids := range nodes {
		if node == m.node {
			m.sessions.DropMembership(sids, h.ID)
		} else {
			_ = m.bus.Send(node, &busMsg{Kind: kKick, MatchID: h.ID, Sessions: sids})
		}
	}
}

// restore starts a match from a snapshot (handover or crash takeover).
func (m *MatchHost) restore(s *MatchSnapshot, epoch int64, deadNodes map[string]bool) (*MatchHandler, error) {
	f := m.factories[s.Module]
	if f == nil {
		return nil, fmt.Errorf("match handler %q not found", s.Module)
	}
	if s.Schema > f.MaxSchemaVersion() {
		return nil, fmt.Errorf("match state schema %d is newer than this build supports (%d)", s.Schema, f.MaxSchemaVersion())
	}
	match, err := f.Restore(m.nk, s.Schema, s.State)
	if err != nil {
		return nil, err
	}
	u, _ := splitMatchID(s.MatchID)
	var keep, gone []game.Presence
	for _, p := range s.Presences {
		if deadNodes[p.Node] {
			gone = append(gone, p)
		} else {
			keep = append(keep, p)
		}
	}
	h := newMatchHandler(m, s.MatchID, u, s.Module, match, s.TickRate, s.Label, s.Tick, s.CreateTime, epoch, keep)
	h.applyRestore(s)
	if deadNodes != nil && m.cfg.Cluster.ResyncData != "" {
		// Restored from a checkpoint after a crash: surviving players may have
		// missed updates sent after it, so ask the game to resend a snapshot.
		now := time.Now().UnixMilli()
		h.inMu.Lock()
		for _, p := range keep {
			h.inputs = append(h.inputs, game.MatchMessage{Sender: p, OpCode: m.cfg.Cluster.ResyncOpCode,
				Data: []byte(m.cfg.Cluster.ResyncData), HasData: true, Reliable: true, ReceiveTimeMs: now})
		}
		h.inMu.Unlock()
	}
	if len(gone) > 0 {
		// Their sockets died with the node: the game sees ordinary leaves
		// and the players rejoin through another gateway.
		h.queue(func() {
			for _, p := range gone {
				h.presences.Add(p)
			}
			h.processLeaves(gone)
		})
	}
	m.mu.Lock()
	m.local[u] = h
	delete(m.forwards, u)
	delete(m.routes, u)
	m.mu.Unlock()
	h.start()
	m.metrics.GaugeAdd("matches_local", nil, 1)
	return h, nil
}

// ---------------------------------------------------------------- checkpoints

type pendingCheckpoint struct {
	h    *MatchHandler
	snap *MatchSnapshot
}

// queueCheckpoint records the newest snapshot of a match; older unwritten
// snapshots of the same match are superseded.
func (m *MatchHost) queueCheckpoint(h *MatchHandler, s *MatchSnapshot) {
	m.ckMu.Lock()
	m.ckPending[h.UUID] = &pendingCheckpoint{h: h, snap: s}
	m.ckMu.Unlock()
	select {
	case m.ckKick <- struct{}{}:
	default:
	}
}

// runCheckpointer writes queued snapshots. With checkpoint_interval_ms > 0 a
// match is written at most that often; 0 writes every change; < 0 disables
// checkpoints (no crash failover).
func (m *MatchHost) runCheckpointer(ctx context.Context) {
	if m.cfg.Cluster.CheckpointIntervalMs < 0 {
		return
	}
	interval := time.Duration(m.cfg.Cluster.CheckpointIntervalMs) * time.Millisecond
	lastWrite := map[string]time.Time{}
	sweep := time.NewTicker(time.Second)
	defer sweep.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.ckKick:
		case <-sweep.C:
			// Retry matches whose last write failed.
			for _, h := range m.LocalHandlers() {
				if h.dirty.Load() {
					cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
					if s, err := h.Checkpoint(cctx, false); err == nil && s != nil {
						m.queueCheckpoint(h, s)
					}
					cancel()
				}
			}
			for u, t := range lastWrite {
				if time.Since(t) > 10*time.Minute {
					delete(lastWrite, u)
				}
			}
		}
		m.ckMu.Lock()
		batch := m.ckPending
		m.ckPending = map[string]*pendingCheckpoint{}
		m.ckMu.Unlock()
		var deferred []*pendingCheckpoint
		var wg sync.WaitGroup
		sem := make(chan struct{}, 8)
		for u, p := range batch {
			if interval > 0 && time.Since(lastWrite[u]) < interval {
				deferred = append(deferred, p)
				continue
			}
			if p.h.stopped.Load() {
				continue
			}
			lastWrite[u] = time.Now()
			wg.Add(1)
			sem <- struct{}{}
			go func(p *pendingCheckpoint) {
				defer func() { <-sem; wg.Done() }()
				cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
				err := m.writeCheckpoint(cctx, p.h, p.snap)
				cancel()
				switch {
				case errors.Is(err, errFenced):
					m.logger.Error("lost ownership of match, stopping local copy", "match_id", p.h.ID)
					p.h.completeMove("")
					m.mu.Lock()
					if m.local[p.h.UUID] == p.h {
						delete(m.local, p.h.UUID)
					}
					m.mu.Unlock()
				case err != nil:
					m.logger.Warn("checkpoint failed", "match_id", p.h.ID, "error", err)
					p.h.dirty.Store(true)
					p.h.forceNextCheckpoint()
				}
			}(p)
		}
		wg.Wait()
		if len(deferred) > 0 {
			m.ckMu.Lock()
			for _, p := range deferred {
				if _, newer := m.ckPending[p.h.UUID]; !newer {
					m.ckPending[p.h.UUID] = p
				}
			}
			m.ckMu.Unlock()
			go func() {
				time.Sleep(interval / 2)
				select {
				case m.ckKick <- struct{}{}:
				default:
				}
			}()
		}
	}
}

var errFenced = errors.New("match ownership lost")

func (m *MatchHost) writeCheckpoint(ctx context.Context, h *MatchHandler, s *MatchSnapshot) error {
	pres, _ := json.Marshal(struct {
		Presences     []game.Presence  `json:"presences"`
		PendingLeaves map[string]int64 `json:"pending_leaves,omitempty"`
		Module        string           `json:"module"`
		Schema        int              `json:"schema"`
		TickRate      int              `json:"tick_rate"`
		Label         string           `json:"label"`
		CreateTime    int64            `json:"create_time"`
		MatchID       string           `json:"match_id"`
	}{s.Presences, s.PendingLeaves, s.Module, s.Schema, s.TickRate, s.Label, s.CreateTime, s.MatchID})
	tag, err := m.db.Exec(ctx, `WITH own AS (SELECT 1 FROM cluster_match WHERE id = $1 AND node = $2 AND epoch = $3)
		INSERT INTO cluster_match_state (id, epoch, tick, state, presences, saved_at)
		SELECT $1, $3, $4, $5, $6, now() FROM own
		ON CONFLICT (id) DO UPDATE SET epoch = EXCLUDED.epoch, tick = EXCLUDED.tick, state = EXCLUDED.state,
			presences = EXCLUDED.presences, saved_at = now()
		WHERE cluster_match_state.epoch <= EXCLUDED.epoch`, h.UUID, m.node, h.epoch.Load(), s.Tick, s.State, pres)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		var n int
		_ = m.db.QueryRow(ctx, "SELECT count(*) FROM cluster_match WHERE id = $1 AND node = $2 AND epoch = $3", h.UUID, m.node, h.epoch.Load()).Scan(&n)
		if n == 0 {
			return errFenced
		}
	}
	m.metrics.CounterAdd("match_checkpoints_total", nil, 1)
	return nil
}

func decodeCheckpoint(u string, state, presences []byte, tick int64) (*MatchSnapshot, error) {
	var meta struct {
		Presences     []game.Presence  `json:"presences"`
		PendingLeaves map[string]int64 `json:"pending_leaves"`
		Module        string           `json:"module"`
		Schema        int              `json:"schema"`
		TickRate      int              `json:"tick_rate"`
		Label         string           `json:"label"`
		CreateTime    int64            `json:"create_time"`
		MatchID       string           `json:"match_id"`
	}
	if err := json.Unmarshal(presences, &meta); err != nil {
		return nil, err
	}
	return &MatchSnapshot{MatchID: meta.MatchID, Module: meta.Module, Schema: meta.Schema, State: state, Tick: tick, TickRate: meta.TickRate,
		Label: meta.Label, CreateTime: meta.CreateTime, Presences: meta.Presences, PendingLeaves: meta.PendingLeaves}, nil
}

// ---------------------------------------------------------------- handover

// Handover moves one local match to target without stopping it.
func (m *MatchHost) Handover(ctx context.Context, h *MatchHandler, target string) error {
	start := time.Now()
	snap, err := h.beginMove(ctx)
	if err != nil {
		return err
	}
	abort := func(err error) error {
		h.abortMove(snap.Inputs)
		return err
	}
	raw, err := json.Marshal(snap)
	if err != nil {
		return abort(err)
	}
	oldEpoch := h.epoch.Load()
	newEpoch := oldEpoch + 1
	resp, err := m.bus.Request(ctx, target, &busMsg{Kind: kHandover, MatchID: h.ID, Epoch: newEpoch, Data: raw})
	if err != nil {
		return abort(err)
	}
	if err := resp.error(); err != nil {
		return abort(err)
	}
	// Commit ownership and a checkpoint of the moved state atomically, so a
	// crash of the target right after the commit still loses nothing.
	pres, _ := json.Marshal(struct {
		Presences     []game.Presence  `json:"presences"`
		PendingLeaves map[string]int64 `json:"pending_leaves,omitempty"`
		Module        string           `json:"module"`
		Schema        int              `json:"schema"`
		TickRate      int              `json:"tick_rate"`
		Label         string           `json:"label"`
		CreateTime    int64            `json:"create_time"`
		MatchID       string           `json:"match_id"`
	}{snap.Presences, snap.PendingLeaves, snap.Module, snap.Schema, snap.TickRate, snap.Label, snap.CreateTime, snap.MatchID})
	err = ExecuteInTx(ctx, m.db, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, "UPDATE cluster_match SET node = $2, epoch = $3, update_time = now() WHERE id = $1 AND node = $4 AND epoch = $5", h.UUID, target, newEpoch, m.node, oldEpoch)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return errFenced
		}
		_, err = tx.Exec(ctx, `INSERT INTO cluster_match_state (id, epoch, tick, state, presences, saved_at) VALUES ($1, $2, $3, $4, $5, now())
			ON CONFLICT (id) DO UPDATE SET epoch = EXCLUDED.epoch, tick = EXCLUDED.tick, state = EXCLUDED.state, presences = EXCLUDED.presences, saved_at = now()`,
			h.UUID, newEpoch, snap.Tick, snap.State, pres)
		return err
	})
	if err != nil {
		_ = m.bus.Send(target, &busMsg{Kind: kHandoverAbort, MatchID: h.ID})
		return abort(err)
	}
	// From here the target owns the match.
	m.mu.Lock()
	m.forwards[h.UUID] = target
	delete(m.local, h.UUID)
	m.mu.Unlock()
	if _, err := m.bus.Request(ctx, target, &busMsg{Kind: kActivate, MatchID: h.ID, Epoch: newEpoch}); err != nil {
		// The target will activate on its own takeover path if it crashed;
		// if it is alive it activates on the next request for the match.
		m.logger.Warn("handover activate failed", "match_id", h.ID, "target", target, "error", err)
	}
	buffered := h.completeMove(target)
	for _, msg := range buffered {
		_ = m.bus.Send(target, &busMsg{Kind: kData, MatchID: h.ID, Presence: msg.Sender, OpCode: msg.OpCode, Data: msg.Data, HasData: msg.HasData, Reliable: msg.Reliable, ReceiveTimeMs: msg.ReceiveTimeMs})
	}
	m.metrics.GaugeAdd("matches_local", nil, -1)
	m.bus.Broadcast(&busMsg{Kind: kMoved, MatchID: h.ID, Node: target})
	m.metrics.CounterAdd("match_handovers_total", nil, 1)
	m.metrics.TimerRecord("match_handover_duration", nil, time.Since(start).Nanoseconds())
	m.logger.Info("match handed over", "match_id", h.ID, "target", target, "pause_ms", time.Since(start).Milliseconds())
	time.AfterFunc(2*time.Minute, func() {
		m.mu.Lock()
		if m.forwards[h.UUID] == target {
			delete(m.forwards, h.UUID)
		}
		m.mu.Unlock()
	})
	return nil
}

// pendingHandovers holds restored-but-inactive matches on a target.
type pendingHandover struct {
	h     *MatchHandler
	snap  *MatchSnapshot
	epoch int64
}

func (m *MatchHost) acceptHandover(raw []byte, epoch int64) (*pendingHandover, error) {
	var s MatchSnapshot
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, err
	}
	f := m.factories[s.Module]
	if f == nil {
		return nil, fmt.Errorf("match handler %q not found", s.Module)
	}
	if s.Schema > f.MaxSchemaVersion() {
		return nil, fmt.Errorf("match state schema %d is newer than this build supports (%d)", s.Schema, f.MaxSchemaVersion())
	}
	if _, err := f.Restore(m.nk, s.Schema, s.State); err != nil {
		return nil, err
	}
	return &pendingHandover{snap: &s, epoch: epoch}, nil
}

func (m *MatchHost) activateHandover(p *pendingHandover) error {
	if m.localHandler(strings.SplitN(p.snap.MatchID, ".", 2)[0]) != nil {
		return nil
	}
	_, err := m.restore(p.snap, p.epoch, nil)
	return err
}
