package server

import (
	"context"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dev-sssss/tango_backend/server/internal/game"
)

// Analytics persists reporting facts (see the analytics_* tables) off the
// request and match paths: callers only touch memory, a background loop
// writes. Losing a few rows on a crash is acceptable; slowing a match is not.
type Analytics struct {
	db      *pgxpool.Pool
	logger  *slog.Logger
	metrics *Metrics

	mu      sync.Mutex
	pending map[activityKey]string // user-hours not yet written -> client tag
	seen    map[activityKey]bool   // user-hours written by this node
	writes  chan func(context.Context) error

	// gamesFinished is told which players finished a game (invite rewards).
	gamesFinished func(ctx context.Context, userIDs []string)
}

type activityKey struct {
	hour   int64 // unix seconds, truncated to the hour
	userID string
}

func NewAnalytics(db *pgxpool.Pool, logger *slog.Logger, metrics *Metrics) *Analytics {
	return &Analytics{db: db, logger: logger, metrics: metrics,
		pending: map[activityKey]string{}, seen: map[activityKey]bool{},
		writes: make(chan func(context.Context) error, 4096)}
}

// Active marks the user as active in the current hour.
func (a *Analytics) Active(userID, client string) {
	if a == nil || userID == "" {
		return
	}
	k := activityKey{time.Now().UTC().Truncate(time.Hour).Unix(), userID}
	a.mu.Lock()
	if !a.seen[k] {
		if _, ok := a.pending[k]; !ok || client != "legacy" {
			a.pending[k] = client
		}
	}
	a.mu.Unlock()
}

func (a *Analytics) enqueue(kind string, fn func(context.Context) error) {
	select {
	case a.writes <- fn:
	default:
		a.metrics.CounterAdd("analytics_dropped_total", map[string]string{"kind": kind}, 1)
	}
}

func (a *Analytics) RecordMatchStart(f game.MatchStartFact) {
	if a == nil || f.MatchID == "" {
		return
	}
	humans, bots := 0, 0
	seats := make([]int32, 0, len(f.Players))
	users := make([]string, 0, len(f.Players))
	isBot := make([]bool, 0, len(f.Players))
	for _, p := range f.Players {
		if p.Bot {
			bots++
		} else {
			humans++
		}
		seats, users, isBot = append(seats, int32(p.Seat)), append(users, p.UserID), append(isBot, p.Bot)
	}
	a.enqueue("match_start", func(ctx context.Context) error {
		tx, err := a.db.Begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(ctx, `INSERT INTO analytics_match (match_id, mode, private, humans, bots, entry_fee, started_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7) ON CONFLICT (match_id) DO NOTHING`,
			f.MatchID, f.Mode, f.Private, humans, bots, f.EntryFee, f.StartedAt); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO analytics_match_player (match_id, seat, user_id, is_bot, started_at)
			SELECT $1, s, u, b, $5 FROM unnest($2::int[], $3::text[], $4::bool[]) AS t(s, u, b)
			ON CONFLICT (match_id, seat) DO NOTHING`, f.MatchID, seats, users, isBot, f.StartedAt); err != nil {
			return err
		}
		return tx.Commit(ctx)
	})
}

func (a *Analytics) RecordMatchEnd(f game.MatchEndFact) {
	if a == nil || f.MatchID == "" {
		return
	}
	seats := make([]int32, 0, len(f.Results))
	ranks := make([]int32, 0, len(f.Results))
	forfeited := make([]bool, 0, len(f.Results))
	prizes := make([]int64, 0, len(f.Results))
	for _, r := range f.Results {
		seats, ranks = append(seats, int32(r.Seat)), append(ranks, int32(r.Rank))
		forfeited, prizes = append(forfeited, r.Forfeited), append(prizes, r.Prize)
	}
	a.enqueue("match_end", func(ctx context.Context) error {
		tx, err := a.db.Begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(ctx, `UPDATE analytics_match SET ended_at = $2, duration_s = $3, end_reason = $4 WHERE match_id = $1`,
			f.MatchID, f.EndedAt, f.DurationSec, f.EndReason); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE analytics_match_player p SET rank = NULLIF(t.r, 0), forfeited = t.f, prize = t.z
			FROM unnest($2::int[], $3::int[], $4::bool[], $5::bigint[]) AS t(s, r, f, z)
			WHERE p.match_id = $1 AND p.seat = t.s`, f.MatchID, seats, ranks, forfeited, prizes); err != nil {
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
		if a.gamesFinished != nil && f.EndReason != "test_win" {
			rows, err := a.db.Query(ctx, `SELECT user_id FROM analytics_match_player
				WHERE match_id = $1 AND NOT is_bot AND NOT coalesce(forfeited, false)`, f.MatchID)
			if err != nil {
				return err
			}
			var users []string
			for rows.Next() {
				var u string
				if rows.Scan(&u) == nil {
					users = append(users, u)
				}
			}
			rows.Close()
			a.gamesFinished(ctx, users)
		}
		return nil
	})
}

func (a *Analytics) RecordOfflineGames(userID string, games []game.OfflineGameFact) {
	if a == nil || !isUUID(userID) || len(games) == 0 {
		return
	}
	n := len(games)
	ids, modes, diffs, outcomes, clients := make([]string, n), make([]string, n), make([]string, n), make([]string, n), make([]string, n)
	humans, bots, durs := make([]int32, n), make([]int32, n), make([]int32, n)
	started := make([]time.Time, n)
	for i, g := range games {
		ids[i], modes[i], diffs[i], outcomes[i], clients[i] = g.GameID, g.Mode, g.Difficulty, g.Outcome, g.Client
		humans[i], bots[i], durs[i] = int32(g.Humans), int32(g.Bots), int32(g.DurationSec)
		started[i] = g.StartedAt
	}
	a.enqueue("offline_game", func(ctx context.Context) error {
		_, err := a.db.Exec(ctx, `INSERT INTO analytics_offline_game
			(user_id, game_id, mode, humans, bots, difficulty, started_at, duration_s, outcome, client)
			SELECT $1::uuid, g, m, h, b, d, s, t, o, c
			FROM unnest($2::text[], $3::text[], $4::int[], $5::int[], $6::text[], $7::timestamptz[], $8::int[], $9::text[], $10::text[])
				AS x(g, m, h, b, d, s, t, o, c)
			ON CONFLICT (user_id, game_id) DO NOTHING`,
			userID, ids, modes, humans, bots, diffs, started, durs, outcomes, clients)
		if err == nil && a.gamesFinished != nil {
			for _, g := range games {
				if g.Outcome != "quit" {
					a.gamesFinished(ctx, []string{userID})
					break
				}
			}
		}
		return err
	})
}

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func isUUID(s string) bool { return uuidPattern.MatchString(s) }

// Run writes queued facts and flushes active user-hours every 30 s.
func (a *Analytics) Run(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			fctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			a.drain(fctx)
			a.FlushActivity(fctx)
			cancel()
			return
		case fn := <-a.writes:
			a.write(ctx, fn)
		case <-t.C:
			a.FlushActivity(ctx)
		}
	}
}

func (a *Analytics) drain(ctx context.Context) {
	for {
		select {
		case fn := <-a.writes:
			a.write(ctx, fn)
		default:
			return
		}
	}
}

func (a *Analytics) write(ctx context.Context, fn func(context.Context) error) {
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := fn(wctx); err != nil {
		a.metrics.CounterAdd("analytics_write_errors_total", nil, 1)
		a.logger.Warn("analytics write failed", "error", err)
	}
}

func (a *Analytics) FlushActivity(ctx context.Context) {
	a.mu.Lock()
	pending := a.pending
	a.pending = map[activityKey]string{}
	a.mu.Unlock()
	if len(pending) == 0 {
		return
	}
	hours := make([]time.Time, 0, len(pending))
	users := make([]string, 0, len(pending))
	clients := make([]string, 0, len(pending))
	for k, c := range pending {
		hours, users, clients = append(hours, time.Unix(k.hour, 0).UTC()), append(users, k.userID), append(clients, c)
	}
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err := a.db.Exec(wctx, `INSERT INTO analytics_user_hour (hour, user_id, client)
		SELECT h, u::uuid, c FROM unnest($1::timestamptz[], $2::text[], $3::text[]) AS t(h, u, c)
		WHERE u ~* '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
		ON CONFLICT (hour, user_id) DO UPDATE SET client = EXCLUDED.client
		WHERE analytics_user_hour.client IN ('', 'legacy') AND EXCLUDED.client NOT IN ('', 'legacy')`, hours, users, clients)
	a.mu.Lock()
	defer a.mu.Unlock()
	if err != nil {
		a.metrics.CounterAdd("analytics_write_errors_total", nil, 1)
		a.logger.Warn("activity flush failed", "error", err)
		for k, c := range pending {
			if _, ok := a.pending[k]; !ok {
				a.pending[k] = c
			}
		}
		return
	}
	current := time.Now().UTC().Truncate(time.Hour).Unix()
	for k := range a.seen {
		if k.hour < current {
			delete(a.seen, k)
		}
	}
	for k := range pending {
		if k.hour >= current {
			a.seen[k] = true
		}
	}
}

// trackSignin counts sign-ins (game_signins_total{method,is_new}) and marks
// the user active.
func (srv *Server) trackSignin(r *http.Request, method, userID string, created bool) {
	srv.metrics.CounterAdd("game_signins_total", map[string]string{"method": method, "is_new": strconv.FormatBool(created)}, 1)
	srv.analytics.Active(userID, clientTag(r))
}

// PgNK forwards match facts to the server's Analytics (game.Analytics).
func (n *PgNK) RecordMatchStart(f game.MatchStartFact) { n.analytics.RecordMatchStart(f) }
func (n *PgNK) RecordMatchEnd(f game.MatchEndFact)     { n.analytics.RecordMatchEnd(f) }
func (n *PgNK) RecordOfflineGames(userID string, games []game.OfflineGameFact) {
	n.analytics.RecordOfflineGames(userID, games)
}

var _ game.Analytics = (*PgNK)(nil)
