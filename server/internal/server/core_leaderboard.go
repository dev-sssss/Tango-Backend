package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dev-sssss/tango_backend/server/internal/game"
)

const (
	lbOpBest = 0
	lbOpSet  = 1
	lbOpIncr = 2
	lbOpDecr = 3
)

type leaderboard struct {
	ID            string
	Authoritative bool
	SortDesc      bool
	Operator      int
	Reset         *Cron
}

// Leaderboards caches leaderboard definitions (they rarely change).
type Leaderboards struct {
	db *pgxpool.Pool
	mu sync.RWMutex
	m  map[string]*leaderboard
}

func NewLeaderboards(db *pgxpool.Pool) *Leaderboards {
	return &Leaderboards{db: db, m: map[string]*leaderboard{}}
}

func (l *Leaderboards) get(ctx context.Context, id string) (*leaderboard, error) {
	l.mu.RLock()
	lb, ok := l.m[id]
	l.mu.RUnlock()
	if ok {
		return lb, nil
	}
	var sortOrder, op int
	var auth bool
	var reset *string
	err := l.db.QueryRow(ctx, "SELECT authoritative, sort_order, operator, reset_schedule FROM leaderboard WHERE id = $1", id).Scan(&auth, &sortOrder, &op, &reset)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, statusErr(CodeNotFound, "Leaderboard not found.")
	}
	if err != nil {
		return nil, err
	}
	lb = &leaderboard{ID: id, Authoritative: auth, SortDesc: sortOrder == 1, Operator: op}
	if reset != nil && *reset != "" {
		c, err := ParseCron(*reset)
		if err != nil {
			return nil, err
		}
		lb.Reset = c
	}
	l.mu.Lock()
	l.m[id] = lb
	l.mu.Unlock()
	return lb, nil
}

func operatorNumber(op string) int {
	switch op {
	case "set":
		return lbOpSet
	case "incr", "increment":
		return lbOpIncr
	case "decr", "decrement":
		return lbOpDecr
	}
	return lbOpBest
}

func (l *Leaderboards) Create(ctx context.Context, id string, authoritative, sortDesc bool, operator, reset string) error {
	if reset != "" {
		if _, err := ParseCron(reset); err != nil {
			return err
		}
	}
	so := 0
	if sortDesc {
		so = 1
	}
	var rs any
	if reset != "" {
		rs = reset
	}
	_, err := l.db.Exec(ctx, `INSERT INTO leaderboard (id, authoritative, sort_order, operator, reset_schedule, metadata, enable_ranks)
		VALUES ($1, $2, $3, $4, $5, '{}', false) ON CONFLICT (id) DO NOTHING`, id, authoritative, so, operatorNumber(operator), rs)
	return err
}

func (lb *leaderboard) expiry(now time.Time) time.Time {
	if lb.Reset == nil {
		return time.Unix(0, 0).UTC()
	}
	return lb.Reset.Next(now)
}

func (l *Leaderboards) Write(ctx context.Context, id, owner, username string, score, subscore int64, metadata map[string]any) (*game.LeaderboardRecord, error) {
	lb, err := l.get(ctx, id)
	if err != nil {
		return nil, err
	}
	exp := lb.expiry(time.Now())
	meta := []byte("{}")
	if metadata != nil {
		meta, _ = json.Marshal(metadata)
	}
	var user any
	if username != "" {
		user = username
	}
	var scoreExpr, subExpr string
	switch lb.Operator {
	case lbOpIncr:
		scoreExpr, subExpr = "leaderboard_record.score + $4", "leaderboard_record.subscore + $5"
	case lbOpDecr:
		scoreExpr, subExpr = "GREATEST(leaderboard_record.score - $4, 0)", "GREATEST(leaderboard_record.subscore - $5, 0)"
	case lbOpSet:
		scoreExpr, subExpr = "$4", "$5"
	default:
		if lb.SortDesc {
			scoreExpr, subExpr = "GREATEST(leaderboard_record.score, $4)", "CASE WHEN $4 > leaderboard_record.score THEN $5 ELSE leaderboard_record.subscore END"
		} else {
			scoreExpr, subExpr = "LEAST(leaderboard_record.score, $4)", "CASE WHEN $4 < leaderboard_record.score THEN $5 ELSE leaderboard_record.subscore END"
		}
	}
	initScore, initSub := score, subscore
	if lb.Operator == lbOpDecr {
		initScore, initSub = 0, 0
	}
	r := &game.LeaderboardRecord{LeaderboardID: id, OwnerID: owner}
	var uname *string
	var ct, ut time.Time
	err = l.db.QueryRow(ctx, `INSERT INTO leaderboard_record (leaderboard_id, owner_id, username, score, subscore, metadata, expiry_time)
		VALUES ($1, $2, $3, $6, $7, $8, $9)
		ON CONFLICT (owner_id, leaderboard_id, expiry_time) DO UPDATE SET
			score = `+scoreExpr+`, subscore = `+subExpr+`, num_score = leaderboard_record.num_score + 1,
			username = COALESCE($3, leaderboard_record.username), metadata = COALESCE($8, leaderboard_record.metadata), update_time = now()
		RETURNING username, score, subscore, num_score, create_time, update_time`,
		id, owner, user, score, subscore, initScore, initSub, meta, exp).Scan(&uname, &r.Score, &r.Subscore, &r.NumScore, &ct, &ut)
	if err != nil {
		return nil, err
	}
	r.Username = deref(uname)
	r.CreateTime, r.UpdateTime = ct.Unix(), ut.Unix()
	if lb.Reset != nil {
		r.ExpiryTime = exp.Unix()
	}
	return r, nil
}

type lbCursor struct {
	Score, Subscore int64
	Owner           string
	Rank            int64
}

// List returns ranked records (top or after a cursor) and, for ownerIDs,
// those owners' records with their exact rank.
func (l *Leaderboards) List(ctx context.Context, id string, ownerIDs []string, limit int, cursor string) ([]*game.LeaderboardRecord, []*game.LeaderboardRecord, string, string, error) {
	lb, err := l.get(ctx, id)
	if err != nil {
		return nil, nil, "", "", err
	}
	exp := lb.expiry(time.Now())
	dir, cmp := "DESC", "<"
	if !lb.SortDesc {
		dir, cmp = "ASC", ">"
	}
	cols := "owner_id::text, username, score, subscore, num_score, max_num_score, metadata::text, create_time, update_time, expiry_time"
	scan := func(rows pgx.Rows) ([]*game.LeaderboardRecord, error) {
		defer rows.Close()
		var out []*game.LeaderboardRecord
		for rows.Next() {
			r := &game.LeaderboardRecord{LeaderboardID: id}
			var uname *string
			var meta string
			var maxNum int64
			var ct, ut, et time.Time
			if err := rows.Scan(&r.OwnerID, &uname, &r.Score, &r.Subscore, &r.NumScore, &maxNum, &meta, &ct, &ut, &et); err != nil {
				return nil, err
			}
			r.Username = deref(uname)
			_ = json.Unmarshal([]byte(meta), &r.Metadata)
			r.CreateTime, r.UpdateTime = ct.Unix(), ut.Unix()
			if lb.Reset != nil {
				r.ExpiryTime = et.Unix()
			}
			out = append(out, r)
		}
		return out, rows.Err()
	}
	var records []*game.LeaderboardRecord
	next := ""
	if ownerIDs == nil || limit > 0 {
		if limit <= 0 {
			limit = 1
		}
		var c *lbCursor
		if cursor != "" {
			if b, err := base64.RawURLEncoding.DecodeString(cursor); err == nil {
				c = &lbCursor{}
				if json.Unmarshal(b, c) != nil {
					c = nil
				}
			}
			if c == nil {
				return nil, nil, "", "", statusErr(CodeInvalidArgument, "Invalid cursor.")
			}
		}
		var rows pgx.Rows
		if c == nil {
			rows, err = l.db.Query(ctx, "SELECT "+cols+" FROM leaderboard_record WHERE leaderboard_id = $1 AND expiry_time = $2 ORDER BY score "+dir+", subscore "+dir+", owner_id "+dir+" LIMIT $3", id, exp, limit+1)
		} else {
			rows, err = l.db.Query(ctx, "SELECT "+cols+" FROM leaderboard_record WHERE leaderboard_id = $1 AND expiry_time = $2 AND (score, subscore, owner_id) "+cmp+" ($3, $4, $5::uuid) ORDER BY score "+dir+", subscore "+dir+", owner_id "+dir+" LIMIT $6", id, exp, c.Score, c.Subscore, c.Owner, limit+1)
		}
		if err != nil {
			return nil, nil, "", "", err
		}
		if records, err = scan(rows); err != nil {
			return nil, nil, "", "", err
		}
		start := int64(0)
		if c != nil {
			start = c.Rank
		}
		for i, r := range records {
			r.Rank = start + int64(i) + 1
		}
		if len(records) > limit {
			records = records[:limit]
			last := records[len(records)-1]
			b, _ := json.Marshal(lbCursor{Score: last.Score, Subscore: last.Subscore, Owner: last.OwnerID, Rank: last.Rank})
			next = base64.RawURLEncoding.EncodeToString(b)
		}
		if ownerIDs != nil && cursor == "" && limit > 0 && len(ownerIDs) > 0 {
			// Nakama returns top records too when both are requested.
		}
	}
	var owners []*game.LeaderboardRecord
	if len(ownerIDs) > 0 {
		rows, err := l.db.Query(ctx, "SELECT "+cols+" FROM leaderboard_record WHERE leaderboard_id = $1 AND expiry_time = $2 AND owner_id = ANY($3::uuid[])", id, exp, ownerIDs)
		if err != nil {
			return nil, nil, "", "", err
		}
		if owners, err = scan(rows); err != nil {
			return nil, nil, "", "", err
		}
		for _, r := range owners {
			var better int64
			_ = l.db.QueryRow(ctx, "SELECT count(*) FROM leaderboard_record WHERE leaderboard_id = $1 AND expiry_time = $2 AND (score, subscore, owner_id) "+map[bool]string{true: ">", false: "<"}[lb.SortDesc]+" ($3, $4, $5::uuid)", id, exp, r.Score, r.Subscore, r.OwnerID).Scan(&better)
			r.Rank = better + 1
		}
	}
	if ownerIDs != nil && len(ownerIDs) > 0 && limit <= 1 && cursor == "" {
		// Lua callers ask for owners with limit 1; they only read owner records.
	}
	return records, owners, next, "", nil
}
