package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dev-sssss/tango_backend/server/internal/game"
	"github.com/dev-sssss/tango_backend/server/internal/game/rpc"
	"github.com/dev-sssss/tango_backend/server/internal/game/tango"
)

// Server is one node. Its roles decide what it does:
//   - gateway: serves the client HTTP API and realtime sockets;
//   - game: hosts authoritative matches.
//
// Every node joins the cluster bus and can route match traffic, so a gateway
// talks to matches wherever they run.
type Server struct {
	cfg       *Config
	logger    *slog.Logger
	db        *pgxpool.Pool
	metrics   *Metrics
	tokens    *TokenService
	google    *GoogleVerifier
	nk        *PgNK
	host      *MatchHost
	cluster   *Cluster
	bus       *Bus
	sessions  *SessionRegistry
	rpcs      map[string]*rpc.RPC
	versions  *APIVersions
	usage     *UsageTracker
	analytics *Analytics
	friends   *Friends
	guard     *Guard

	cancel   context.CancelFunc
	wg       sync.WaitGroup
	servers  []*http.Server
	clientLn net.Listener
	stopOnce sync.Once
}

func New(cfg *Config, logger *slog.Logger) *Server {
	return &Server{cfg: cfg, logger: logger}
}

// Start connects to the database, joins the cluster and starts listeners.
func (srv *Server) Start(ctx context.Context) error {
	cfg := srv.cfg
	db, err := OpenDB(ctx, cfg)
	if err != nil {
		return fmt.Errorf("database: %w", err)
	}
	srv.db = db
	if err := Migrate(ctx, db); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	srv.metrics = NewMetrics(cfg.Metrics.Namespace)
	srv.tokens = NewTokenService(cfg)
	srv.google = NewGoogleVerifier(cfg.Social.Google.ClientIDs)
	srv.versions = NewAPIVersions(apiVersionTable)
	if p := srv.versions.Validate(); len(p) > 0 {
		return fmt.Errorf("api version table: %v", p)
	}
	srv.usage = NewUsageTracker(db, srv.logger)
	srv.analytics = NewAnalytics(db, srv.logger, srv.metrics)
	srv.friends = NewFriends(db, srv.logger, srv.metrics)
	srv.analytics.gamesFinished = srv.friends.GamesFinished
	srv.guard = NewGuard(cfg, srv.metrics)

	srv.host = NewMatchHost(cfg, srv.logger, db, srv.metrics)
	srv.nk = NewPgNK(cfg, db, srv.logger, srv.metrics, srv.host)
	srv.nk.guard = srv.guard
	srv.nk.analytics = srv.analytics
	srv.nk.friends = srv.friends
	srv.host.nk = srv.nk
	ann := &game.Announcements{}
	srv.host.RegisterFactory(tango.Factory{})
	svc := rpc.NewService(srv.nk, ann)
	srv.rpcs = svc.Registry()
	// Leaderboards used by match settlement and the leaderboard RPC.
	for _, lb := range []struct{ id, op, reset string }{{tango.LeaderboardTrophies, "set", ""}, {tango.LeaderboardWeekly, "incr", "0 0 * * 1"}, {tango.LeaderboardWins, "set", ""}} {
		if err := srv.nk.LeaderboardCreate(ctx, lb.id, false, true, lb.op, lb.reset); err != nil {
			return fmt.Errorf("leaderboard %s: %w", lb.id, err)
		}
	}

	srv.sessions = NewSessionRegistry(srv.metrics)
	srv.bus = NewBus(cfg.Name, cfg.Cluster.Secret, srv.logger, srv.metrics)
	srv.cluster = NewCluster(cfg, srv.logger, db, srv.metrics, cfg.Cluster.Roles)
	srv.cluster.host, srv.cluster.sessions, srv.cluster.bus = srv.host, srv.sessions, srv.bus
	srv.host.sessions, srv.host.bus, srv.host.cluster = srv.sessions, srv.bus, srv.cluster
	srv.bus.resolve, srv.bus.peers, srv.bus.handler = srv.cluster.peerAddr, srv.cluster.livePeers, srv.cluster.handleBus

	busAddr := cfg.Cluster.BusAddress
	if busAddr == "" {
		busAddr = cfg.Socket.Address
	}
	if err := srv.bus.Listen(net.JoinHostPort(busAddr, strconv.Itoa(cfg.Cluster.BusPort))); err != nil {
		return fmt.Errorf("cluster bus: %w", err)
	}
	if err := srv.cluster.Register(ctx); err != nil {
		return fmt.Errorf("cluster register: %w", err)
	}

	runCtx, cancel := context.WithCancel(context.Background())
	srv.cancel = cancel
	srv.goRun(func() { srv.cluster.Run(runCtx) })
	srv.goRun(func() { srv.host.runLabelWriter(runCtx) })
	srv.goRun(func() { srv.host.runCheckpointer(runCtx) })
	srv.goRun(func() { srv.usage.Run(runCtx) })
	srv.goRun(func() { srv.analytics.Run(runCtx) })
	srv.goRun(func() { srv.friends.Run(runCtx, srv.sessions.presenceSnapshot) })
	srv.goRun(func() { srv.guard.Run(runCtx) })

	if srv.cluster.IsGateway() {
		addr := net.JoinHostPort(cfg.Socket.Address, strconv.Itoa(cfg.Socket.Port))
		if err := srv.listen(addr, srv.clientMux(), "client"); err != nil {
			return err
		}
	}
	if cfg.Cluster.AdminPort > 0 {
		if err := srv.listen(net.JoinHostPort(cfg.Cluster.AdminAddress, strconv.Itoa(cfg.Cluster.AdminPort)), srv.adminMux(), "admin"); err != nil {
			return err
		}
	}
	if cfg.Metrics.PrometheusPort > 0 {
		mux := http.NewServeMux()
		mux.Handle("GET /metrics", srv.metrics.Handler())
		maddr := cfg.Metrics.Address
		if maddr == "" {
			maddr = cfg.Socket.Address
		}
		if err := srv.listen(net.JoinHostPort(maddr, strconv.Itoa(cfg.Metrics.PrometheusPort)), mux, "metrics"); err != nil {
			return err
		}
	}
	srv.logger.Info("node started", "name", cfg.Name, "roles", cfg.Cluster.Roles, "version", BuildVersion,
		"bus", cfg.Cluster.AdvertiseAddress, "port", cfg.Socket.Port)
	return nil
}

func (srv *Server) goRun(fn func()) {
	srv.wg.Add(1)
	go func() {
		defer srv.wg.Done()
		fn()
	}()
}

func (srv *Server) listen(addr string, h http.Handler, name string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("%s listener: %w", name, err)
	}
	hs := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second, MaxHeaderBytes: 16 << 10,
		ReadTimeout:  time.Duration(srv.cfg.Socket.ReadTimeoutMs) * time.Millisecond,
		WriteTimeout: time.Duration(srv.cfg.Socket.WriteTimeoutMs) * time.Millisecond,
		IdleTimeout:  time.Duration(srv.cfg.Socket.IdleTimeoutMs) * time.Millisecond}
	if name != "client" {
		hs.WriteTimeout = 5 * time.Minute // drain can take a while
	} else {
		hs.ReadTimeout = 0 // websockets manage their own deadlines
		hs.WriteTimeout = 0
	}
	srv.servers = append(srv.servers, hs)
	go func() {
		if err := hs.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			srv.logger.Error("listener stopped", "name", name, "error", err)
		}
	}()
	srv.logger.Info("listening", "name", name, "addr", ln.Addr().String())
	return nil
}

// Shutdown drains the node: matches move live to other game nodes, client
// sockets are closed with 1012 so apps reconnect elsewhere, then the node
// leaves the cluster.
func (srv *Server) Shutdown(ctx context.Context) {
	srv.stopOnce.Do(func() {
		dctx, cancel := context.WithTimeout(ctx, time.Duration(srv.cfg.Cluster.DrainTimeoutMs)*time.Millisecond)
		_ = srv.cluster.Drain(dctx)
		cancel()
		// Whatever could not move is checkpointed so a peer can resume it.
		for _, h := range srv.host.LocalHandlers() {
			cctx, c2 := context.WithTimeout(context.Background(), 3*time.Second)
			if s, err := h.Checkpoint(cctx, true); err == nil && s != nil {
				_ = srv.host.writeCheckpoint(cctx, h, s)
			}
			c2()
		}
		for _, hs := range srv.servers {
			sctx, c3 := context.WithTimeout(context.Background(), 3*time.Second)
			_ = hs.Shutdown(sctx)
			c3()
		}
		srv.cancel()
		srv.wg.Wait()
		dctx2, c4 := context.WithTimeout(context.Background(), 3*time.Second)
		srv.cluster.Deregister(dctx2)
		c4()
		srv.bus.Close()
		srv.db.Close()
		srv.logger.Info("node stopped")
	})
}

// ---------------------------------------------------------------- admin

func (srv *Server) adminAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok, ok := parseBearer(r.Header.Get("Authorization"))
		secret := srv.cfg.Cluster.Secret
		if secret == "" {
			// Without a secret, only loopback callers are trusted.
			host, _, _ := net.SplitHostPort(r.RemoteAddr)
			if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
		} else if !ok || subtle.ConstantTimeCompare([]byte(tok), []byte(secret)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (srv *Server) adminMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /admin/health", func(w http.ResponseWriter, r *http.Request) {
		writeV2(w, map[string]any{"node": srv.cfg.Name, "draining": srv.cluster.Draining(), "matches": srv.host.LocalCount(), "sessions": srv.sessions.Count(), "version": BuildVersion})
	})
	mux.HandleFunc("POST /admin/drain", srv.adminAuth(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ctx, cancel := context.WithTimeout(r.Context(), time.Duration(srv.cfg.Cluster.DrainTimeoutMs)*time.Millisecond)
		defer cancel()
		_ = srv.cluster.Drain(ctx)
		writeV2(w, map[string]any{"node": srv.cfg.Name, "remaining_matches": srv.host.LocalCount(), "sessions": srv.sessions.Count(), "took_ms": time.Since(start).Milliseconds()})
	}))
	mux.HandleFunc("GET /admin/cluster", srv.adminAuth(func(w http.ResponseWriter, r *http.Request) {
		rows, err := srv.db.Query(r.Context(), `SELECT n.name, n.address, n.roles, n.version, n.draining, n.sessions,
			(SELECT count(*) FROM cluster_match m WHERE m.node = n.name), extract(epoch FROM now() - n.heartbeat_at)::float8
			FROM cluster_node n ORDER BY n.name`)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		defer rows.Close()
		var out []map[string]any
		for rows.Next() {
			var name, addr, roles, ver string
			var draining bool
			var sessions int
			var matches int64
			var age float64
			if err := rows.Scan(&name, &addr, &roles, &ver, &draining, &sessions, &matches, &age); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			out = append(out, map[string]any{"name": name, "address": addr, "roles": roles, "version": ver, "draining": draining,
				"sessions": sessions, "matches": matches, "heartbeat_age_s": age})
		}
		writeV2(w, map[string]any{"nodes": out})
	}))
	mux.HandleFunc("GET /admin/api-usage", srv.adminAuth(func(w http.ResponseWriter, r *http.Request) {
		rep, err := srv.usage.Report(r.Context(), srv.versions, time.Now())
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(map[string]any{"min_support_days": int(MinSupportWindow.Hours() / 24), "versions": rep})
	}))
	return mux
}

// Addr helpers for tests.
func (srv *Server) Host() *MatchHost           { return srv.host }
func (srv *Server) Cluster() *Cluster          { return srv.cluster }
func (srv *Server) Sessions() *SessionRegistry { return srv.sessions }
