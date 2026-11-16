package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dev-sssss/tango_backend/server/internal/game"
)

// Build information, set with -ldflags at release time.
var (
	BuildVersion = "dev"
	BuildTime    = "0" // unix seconds; newer builds win placement
)

type peerInfo struct {
	Name      string
	Address   string
	Roles     map[string]bool
	BuildTime int64
	Version   string
	Schemas   map[string]int
	Matches   int
	Sessions  int
	Draining  bool
	Alive     bool
}

// Cluster tracks node membership, places new matches, takes over matches
// from dead nodes and drains this node for zero-downtime deploys.
type Cluster struct {
	cfg         *Config
	logger      *slog.Logger
	db          *pgxpool.Pool
	host        *MatchHost
	sessions    *SessionRegistry
	bus         *Bus
	metrics     *Metrics
	node        string
	incarnation string
	roles       map[string]bool

	draining atomic.Bool
	fenced   atomic.Bool
	lastBeat atomic.Int64

	mu      sync.RWMutex
	peers   map[string]*peerInfo
	placed  map[string]int // optimistic per-node placements since last refresh
	pending map[string]*pendingHandover
}

func NewCluster(cfg *Config, logger *slog.Logger, db *pgxpool.Pool, metrics *Metrics, roles []string) *Cluster {
	r := map[string]bool{}
	for _, x := range roles {
		r[strings.TrimSpace(x)] = true
	}
	return &Cluster{cfg: cfg, logger: logger, db: db, metrics: metrics, node: cfg.Name, roles: r,
		incarnation: uuid.Must(uuid.NewV4()).String(), peers: map[string]*peerInfo{}, placed: map[string]int{}, pending: map[string]*pendingHandover{}}
}

func (c *Cluster) IsGame() bool    { return c.roles["game"] }
func (c *Cluster) IsGateway() bool { return c.roles["gateway"] }
func (c *Cluster) Draining() bool  { return c.draining.Load() }

func (c *Cluster) rolesString() string {
	var r []string
	for k := range c.roles {
		r = append(r, k)
	}
	sort.Strings(r)
	return strings.Join(r, ",")
}

func (c *Cluster) schemas() map[string]int {
	out := map[string]int{}
	for name, f := range c.host.factories {
		out[name] = f.MaxSchemaVersion()
	}
	return out
}

func buildTimeInt() int64 {
	var n int64
	for _, ch := range BuildTime {
		if ch < '0' || ch > '9' {
			return 0
		}
		n = n*10 + int64(ch-'0')
	}
	return n
}

// Register announces this node and restores matches it owned before a
// restart (same node name).
func (c *Cluster) Register(ctx context.Context) error {
	if err := c.ensureColumns(ctx); err != nil {
		return err
	}
	schemas, _ := json.Marshal(c.schemas())
	_, err := c.db.Exec(ctx, `INSERT INTO cluster_node (name, address, started_at, heartbeat_at, draining, sessions, matches, incarnation, roles, version, build_time, schemas)
		VALUES ($1, $2, now(), now(), false, 0, 0, $3, $4, $5, $6, $7)
		ON CONFLICT (name) DO UPDATE SET address = $2, started_at = now(), heartbeat_at = now(), draining = false, incarnation = $3,
			roles = $4, version = $5, build_time = $6, schemas = $7`,
		c.node, c.cfg.Cluster.AdvertiseAddress, c.incarnation, c.rolesString(), BuildVersion, buildTimeInt(), schemas)
	if err != nil {
		return err
	}
	c.lastBeat.Store(time.Now().UnixNano())
	if err := c.refreshPeers(ctx); err != nil {
		return err
	}
	// Matches still registered to this node name belong to a previous run
	// of this node; restore them from their checkpoints.
	if c.IsGame() {
		c.takeover(ctx, []string{c.node}, true)
	}
	return nil
}

func (c *Cluster) ensureColumns(ctx context.Context) error {
	for _, stmt := range []string{
		"ALTER TABLE cluster_node ADD COLUMN IF NOT EXISTS incarnation TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE cluster_node ADD COLUMN IF NOT EXISTS roles TEXT NOT NULL DEFAULT 'gateway,game'",
		"ALTER TABLE cluster_node ADD COLUMN IF NOT EXISTS version TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE cluster_node ADD COLUMN IF NOT EXISTS build_time BIGINT NOT NULL DEFAULT 0",
		"ALTER TABLE cluster_node ADD COLUMN IF NOT EXISTS schemas JSONB NOT NULL DEFAULT '{}'",
	} {
		if _, err := c.db.Exec(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

func (c *Cluster) refreshPeers(ctx context.Context) error {
	rows, err := c.db.Query(ctx, `SELECT name, address, roles, build_time, version, schemas::text, matches, sessions, draining,
		heartbeat_at > now() - make_interval(secs => $1::double precision / 1000) FROM cluster_node`, c.cfg.Cluster.NodeTimeoutMs)
	if err != nil {
		return err
	}
	defer rows.Close()
	peers := map[string]*peerInfo{}
	for rows.Next() {
		p := &peerInfo{Roles: map[string]bool{}}
		var roles, schemas string
		if err := rows.Scan(&p.Name, &p.Address, &roles, &p.BuildTime, &p.Version, &schemas, &p.Matches, &p.Sessions, &p.Draining, &p.Alive); err != nil {
			return err
		}
		for _, r := range strings.Split(roles, ",") {
			p.Roles[strings.TrimSpace(r)] = true
		}
		_ = json.Unmarshal([]byte(schemas), &p.Schemas)
		peers[p.Name] = p
	}
	if err := rows.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	c.peers = peers
	c.placed = map[string]int{}
	c.mu.Unlock()
	return nil
}

func (c *Cluster) peerAddr(node string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	p, ok := c.peers[node]
	if !ok || p.Address == "" {
		return "", false
	}
	return p.Address, true
}

func (c *Cluster) livePeers() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var out []string
	for n, p := range c.peers {
		if p.Alive {
			out = append(out, n)
		}
	}
	return out
}

// PlaceMatch picks the node for a new match: a live, non-draining game node
// on the newest build, with the fewest matches.
func (c *Cluster) PlaceMatch(module string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var best *peerInfo
	var newest int64 = -1
	for _, p := range c.peers {
		if p.Alive && !p.Draining && p.Roles["game"] && p.BuildTime > newest {
			if _, ok := p.Schemas[module]; ok {
				newest = p.BuildTime
			}
		}
	}
	for _, p := range c.peers {
		if !p.Alive || p.Draining || !p.Roles["game"] || p.BuildTime != newest {
			continue
		}
		if _, ok := p.Schemas[module]; !ok {
			continue
		}
		load := p.Matches + c.placed[p.Name]
		if p.Name == c.node {
			load = c.host.LocalCount() + c.placed[p.Name]
		}
		if best == nil {
			best = p
			continue
		}
		bl := best.Matches + c.placed[best.Name]
		if best.Name == c.node {
			bl = c.host.LocalCount() + c.placed[best.Name]
		}
		if load < bl || (load == bl && p.Name == c.node) {
			best = p
		}
	}
	if best == nil {
		if c.IsGame() && !c.Draining() && !c.fenced.Load() {
			return c.node, nil
		}
		return "", errors.New("no game node available")
	}
	c.placed[best.Name]++
	return best.Name, nil
}

// handoverTarget picks where to move a match during a drain.
func (c *Cluster) handoverTarget(module string, schema int, exclude map[string]bool) string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var best *peerInfo
	for _, p := range c.peers {
		if p.Name == c.node || !p.Alive || p.Draining || !p.Roles["game"] || exclude[p.Name] {
			continue
		}
		if s, ok := p.Schemas[module]; !ok || s < schema {
			continue
		}
		if best == nil || p.BuildTime > best.BuildTime || (p.BuildTime == best.BuildTime && p.Matches+c.placed[p.Name] < best.Matches+c.placed[best.Name]) {
			best = p
		}
	}
	if best == nil {
		return ""
	}
	return best.Name
}

// Run drives heartbeats, peer refresh and dead-node takeover.
func (c *Cluster) Run(ctx context.Context) {
	t := time.NewTicker(time.Duration(c.cfg.Cluster.HeartbeatMs) * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		c.heartbeat(ctx)
	}
}

func (c *Cluster) heartbeat(ctx context.Context) {
	hctx, cancel := context.WithTimeout(ctx, time.Duration(c.cfg.Cluster.HeartbeatMs)*time.Millisecond*2)
	defer cancel()
	tag, err := c.db.Exec(hctx, "UPDATE cluster_node SET heartbeat_at = now(), sessions = $3, matches = $4, draining = $5 WHERE name = $1 AND incarnation = $2",
		c.node, c.incarnation, c.sessions.Count(), c.host.LocalCount(), c.draining.Load())
	if err == nil && tag.RowsAffected() == 0 {
		err = errors.New("node row missing or replaced")
	}
	if err != nil {
		since := time.Since(time.Unix(0, c.lastBeat.Load()))
		c.logger.Warn("heartbeat failed", "error", err, "since_last_ok_ms", since.Milliseconds())
		if since > time.Duration(c.cfg.Cluster.NodeTimeoutMs/2)*time.Millisecond && !c.fenced.Load() {
			c.selfFence()
		}
		return
	}
	c.lastBeat.Store(time.Now().UnixNano())
	if c.fenced.Load() {
		// We were cut off long enough for others to take our matches.
		// Re-register as a fresh incarnation.
		c.fenced.Store(false)
		c.incarnation = uuid.Must(uuid.NewV4()).String()
		if err := c.Register(ctx); err != nil {
			c.logger.Error("re-register failed", "error", err)
		}
		return
	}
	if err := c.refreshPeers(hctx); err != nil {
		c.logger.Warn("peer refresh failed", "error", err)
		return
	}
	c.metrics.GaugeSet("cluster_nodes_alive", nil, float64(len(c.livePeers())))
	var dead []string
	c.mu.RLock()
	for n, p := range c.peers {
		if !p.Alive && n != c.node {
			dead = append(dead, n)
		}
	}
	c.mu.RUnlock()
	if len(dead) == 0 {
		return
	}
	deadSet := map[string]bool{}
	for _, n := range dead {
		deadSet[n] = true
		c.bus.DropPeer(n)
	}
	// Presences whose gateway died leave their matches.
	for _, h := range c.host.LocalHandlers() {
		for n := range deadSet {
			h.RemoveNodePresences(n)
		}
	}
	if c.IsGame() && !c.draining.Load() {
		c.takeover(ctx, dead, false)
	}
	_, _ = c.db.Exec(ctx, `DELETE FROM cluster_node n WHERE n.name = ANY($1) AND n.heartbeat_at < now() - make_interval(secs => $2::double precision / 1000)
		AND NOT EXISTS (SELECT 1 FROM cluster_match m WHERE m.node = n.name)`, dead, c.cfg.Cluster.NodeTimeoutMs)
}

// selfFence stops local matches when this node cannot prove it is alive, so
// that a match is never run by two nodes at once.
func (c *Cluster) selfFence() {
	c.fenced.Store(true)
	c.logger.Error("lost contact with the database; stopping local matches (they will be taken over)")
	for _, h := range c.host.LocalHandlers() {
		h.completeMove("")
		c.host.mu.Lock()
		delete(c.host.local, h.UUID)
		c.host.mu.Unlock()
	}
}

// takeover claims matches registered to the given nodes and restores them
// from their checkpoints. self=true restores this node's own previous run.
func (c *Cluster) takeover(ctx context.Context, nodes []string, self bool) {
	deadSet := map[string]bool{}
	for _, n := range nodes {
		if n != c.node || !self {
			deadSet[n] = true
		}
	}
	// Rows claimed in this pass are excluded from later batches: when this
	// node restores its own previous run, claimed rows keep its name.
	seen := []string{}
	for {
		rows, err := c.db.Query(ctx, `UPDATE cluster_match SET node = $1, epoch = epoch + 1, update_time = now()
			WHERE id IN (SELECT id FROM cluster_match WHERE node = ANY($2) AND id <> ALL($3::uuid[]) ORDER BY id LIMIT 20 FOR UPDATE SKIP LOCKED)
			RETURNING id::text, epoch`, c.node, nodes, seen)
		if err != nil {
			c.logger.Error("takeover claim failed", "error", err)
			return
		}
		type claim struct {
			id    string
			epoch int64
		}
		var claims []claim
		for rows.Next() {
			var cl claim
			if err := rows.Scan(&cl.id, &cl.epoch); err == nil {
				claims = append(claims, cl)
			}
		}
		rows.Close()
		if len(claims) == 0 {
			return
		}
		for _, cl := range claims {
			seen = append(seen, cl.id)
		}
		for _, cl := range claims {
			if c.host.localHandler(cl.id) != nil {
				continue
			}
			var state, pres []byte
			var tick int64
			err := c.db.QueryRow(ctx, "SELECT state, presences, tick FROM cluster_match_state WHERE id = $1", cl.id).Scan(&state, &pres, &tick)
			if err != nil {
				c.logger.Warn("no checkpoint for orphaned match; removing it", "match_id", cl.id, "error", err)
				_, _ = c.db.Exec(ctx, "DELETE FROM cluster_match WHERE id = $1 AND node = $2", cl.id, c.node)
				continue
			}
			snap, err := decodeCheckpoint(cl.id, state, pres, tick)
			if err == nil {
				// A restarted node's own sessions are gone as well.
				dead := deadSet
				if self {
					dead = map[string]bool{c.node: true}
				}
				_, err = c.host.restore(snap, cl.epoch, dead)
			}
			if err != nil {
				c.logger.Error("restore failed; removing match", "match_id", cl.id, "error", err)
				_, _ = c.db.Exec(ctx, "DELETE FROM cluster_match WHERE id = $1 AND node = $2", cl.id, c.node)
				continue
			}
			c.metrics.CounterAdd("match_takeovers_total", nil, 1)
			c.logger.Info("took over match", "match_id", cl.id, "from", nodes, "tick", tick)
			c.bus.Broadcast(&busMsg{Kind: kMoved, MatchID: cl.id, Node: c.node})
		}
	}
}

// Drain moves every local match to other game nodes and closes client
// sessions, so this node can be stopped without interrupting any game.
func (c *Cluster) Drain(ctx context.Context) error {
	c.draining.Store(true)
	_, _ = c.db.Exec(ctx, "UPDATE cluster_node SET draining = true WHERE name = $1", c.node)
	_ = c.refreshPeers(ctx)
	c.logger.Info("draining node", "matches", c.host.LocalCount(), "sessions", c.sessions.Count())
	failed := map[string]int{}
	var noTargetSince time.Time
	for c.host.LocalCount() > 0 {
		if ctx.Err() != nil {
			break
		}
		progressed := false
		noTarget := false
		for _, h := range c.host.LocalHandlers() {
			if h.stopped.Load() {
				continue
			}
			target := c.handoverTarget(h.Module, h.match.SchemaVersion(), nil)
			if target == "" {
				noTarget = true
				break
			}
			hctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := c.host.Handover(hctx, h, target)
			cancel()
			if err != nil {
				failed[h.ID]++
				c.logger.Warn("handover failed; will retry", "match_id", h.ID, "target", target, "error", err)
				continue
			}
			c.mu.Lock()
			c.placed[target]++
			c.mu.Unlock()
			progressed = true
		}
		if noTarget {
			if noTargetSince.IsZero() {
				noTargetSince = time.Now()
			} else if time.Since(noTargetSince) > 3*time.Second {
				// No other game node: stop waiting. The matches are
				// checkpointed on shutdown and resume when a game node
				// (this one after its restart, or a new one) claims them.
				c.logger.Warn("no game node available to take matches; they will resume from checkpoints", "matches", c.host.LocalCount())
				break
			}
		} else {
			noTargetSince = time.Time{}
		}
		if !progressed {
			select {
			case <-ctx.Done():
			case <-time.After(500 * time.Millisecond):
			}
			_ = c.refreshPeers(ctx)
		}
	}
	if n := c.host.LocalCount(); n > 0 && ctx.Err() != nil {
		c.logger.Error("drain timed out with matches still local; they will resume from checkpoints", "matches", n)
	}
	// Let clients reconnect elsewhere; matches keep their seats for the
	// presence grace period.
	c.sessions.CloseAll(1012, "server restarting, reconnect")
	return nil
}

// Deregister removes this node after a successful drain.
func (c *Cluster) Deregister(ctx context.Context) {
	_, _ = c.db.Exec(ctx, "DELETE FROM cluster_node WHERE name = $1 AND incarnation = $2 AND NOT EXISTS (SELECT 1 FROM cluster_match WHERE node = $1)", c.node, c.incarnation)
}

// ---------------------------------------------------------------- bus dispatch

func (c *Cluster) handleBus(from string, m *busMsg) *busMsg {
	h := c.host
	ctx := context.Background()
	local := func() (*MatchHandler, *busMsg) {
		u, ok := splitMatchID(m.MatchID)
		if !ok {
			return nil, errResponse(errMatchNotFound)
		}
		if lh := h.localHandler(u); lh != nil {
			return lh, nil
		}
		// A match handed to us whose activation has not arrived yet.
		// Only activate once the ownership commit is visible, so a match is
		// never live on two nodes.
		c.mu.Lock()
		p := c.pending[u]
		c.mu.Unlock()
		if p != nil {
			c.activateIfOwner(u, p.epoch)
			if lh := h.localHandler(u); lh != nil {
				return lh, nil
			}
		}
		h.mu.RLock()
		fwd := h.forwards[u]
		h.mu.RUnlock()
		if fwd != "" {
			return nil, errResponse(errMoved)
		}
		return nil, errResponse(errNotOwner)
	}
	switch m.Kind {
	case kPing:
		return &busMsg{}
	case kCreate:
		var params map[string]any
		_ = json.Unmarshal(m.Data, &params)
		if !c.IsGame() || c.Draining() {
			return &busMsg{Err: "node does not host matches"}
		}
		id, err := h.createLocal(ctx, m.Module, params)
		if err != nil {
			return &busMsg{Err: err.Error()}
		}
		return &busMsg{MatchID: id}
	case kJoinAttempt:
		lh, errResp := local()
		if lh == nil {
			return errResp
		}
		jctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		r, err := lh.JoinAttempt(jctx, m.Presence, m.Metadata)
		if err != nil {
			return errResponse(err)
		}
		return &busMsg{Allow: r.Allow, Reason: r.Reason, Label: r.Label, Presences: r.Presences}
	case kSignal:
		lh, errResp := local()
		if lh == nil {
			return errResp
		}
		sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		resp, err := lh.Signal(sctx, string(m.Data))
		if err != nil {
			return errResponse(err)
		}
		return &busMsg{Data: []byte(resp)}
	case kData:
		msg := game.MatchMessage{Sender: m.Presence, OpCode: m.OpCode, Data: m.Data, HasData: m.HasData, Reliable: m.Reliable, ReceiveTimeMs: m.ReceiveTimeMs}
		lh, _ := local()
		if lh == nil || errors.Is(lh.QueueData(msg), errMoved) {
			// Moved or unknown: route it again from here (forwarding).
			go h.SendData(ctx, m.MatchID, msg)
		}
	case kLeave:
		lh, _ := local()
		if lh == nil || errors.Is(lh.QueueLeave(m.Presences, m.Migrating), errMoved) {
			go h.Leave(ctx, m.MatchID, m.Presences, m.Migrating)
		}
	case kDeliver:
		var env = decodeBusEnvelope(m.Data)
		if env != nil {
			c.sessions.SendEnvelope(m.Sessions, env)
		}
	case kKick:
		c.sessions.DropMembership(m.Sessions, m.MatchID)
	case kMoved:
		if u, ok := splitMatchID(m.MatchID); ok {
			h.mu.Lock()
			if _, isLocal := h.local[u]; !isLocal {
				h.routes[u] = m.Node
			}
			h.mu.Unlock()
		}
	case kHandover:
		p, err := h.acceptHandover(m.Data, m.Epoch)
		if err != nil {
			return &busMsg{Err: err.Error()}
		}
		u, _ := splitMatchID(m.MatchID)
		c.mu.Lock()
		c.pending[u] = p
		c.mu.Unlock()
		// Safety net if the activation message is lost.
		time.AfterFunc(3*time.Second, func() { c.activateIfOwner(u, p.epoch) })
		return &busMsg{}
	case kHandoverAbort:
		if u, ok := splitMatchID(m.MatchID); ok {
			c.mu.Lock()
			delete(c.pending, u)
			c.mu.Unlock()
		}
	case kActivate:
		u, ok := splitMatchID(m.MatchID)
		if !ok {
			return &busMsg{Err: "bad match id"}
		}
		c.mu.Lock()
		p := c.pending[u]
		delete(c.pending, u)
		c.mu.Unlock()
		if p == nil {
			if h.localHandler(u) != nil {
				return &busMsg{}
			}
			return &busMsg{Err: "no pending handover"}
		}
		if err := h.activateHandover(p); err != nil {
			return &busMsg{Err: err.Error()}
		}
		return &busMsg{}
	}
	return nil
}

func (c *Cluster) activateIfOwner(u string, epoch int64) {
	c.mu.Lock()
	p := c.pending[u]
	c.mu.Unlock()
	if p == nil {
		return
	}
	var node string
	var e int64
	err := c.db.QueryRow(context.Background(), "SELECT node, epoch FROM cluster_match WHERE id = $1", u).Scan(&node, &e)
	c.mu.Lock()
	delete(c.pending, u)
	c.mu.Unlock()
	if err == nil && node == c.node && e == epoch {
		if err := c.host.activateHandover(p); err != nil {
			c.logger.Error("late handover activation failed", "match_id", u, "error", err)
		}
	}
}
