package server

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	pgUniqueViolation      = "23505"
	pgSerializationFailure = "40001"
	pgDeadlockDetected     = "40P01"
)

// dbURL converts Nakama's "user:pass@host:port/db" address format into a
// postgres URL. Full URLs are passed through unchanged.
func dbURL(address string) string {
	if strings.HasPrefix(address, "postgres://") || strings.HasPrefix(address, "postgresql://") {
		return address
	}
	u := "postgres://" + address
	if !strings.Contains(address, "/") {
		u += "/nakama"
	}
	parsed, err := url.Parse(u)
	if err == nil && parsed.Query().Get("sslmode") == "" {
		q := parsed.Query()
		q.Set("sslmode", "prefer")
		parsed.RawQuery = q.Encode()
		return parsed.String()
	}
	return u
}

func OpenDB(ctx context.Context, cfg *Config) (*pgxpool.Pool, error) {
	if len(cfg.Database.Address) == 0 {
		return nil, errors.New("database.address is required")
	}
	pcfg, err := pgxpool.ParseConfig(dbURL(cfg.Database.Address[0]))
	if err != nil {
		return nil, fmt.Errorf("database address: %w", err)
	}
	if cfg.Database.MaxOpenConns > 0 {
		pcfg.MaxConns = int32(cfg.Database.MaxOpenConns)
	}
	pcfg.MaxConnLifetime = 30 * time.Minute
	pcfg.ConnConfig.RuntimeParams["application_name"] = "tango-server/" + cfg.Name
	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for i := 0; i < 30; i++ {
		pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		lastErr = pool.Ping(pctx)
		cancel()
		if lastErr == nil {
			return pool, nil
		}
		select {
		case <-ctx.Done():
			pool.Close()
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
	pool.Close()
	return nil, fmt.Errorf("database unreachable: %w", lastErr)
}

// Migrate creates the Nakama-compatible tables used by the game (no-ops on an
// existing Nakama database) plus the cluster coordination tables.
func Migrate(ctx context.Context, db *pgxpool.Pool) error {
	// Serialise concurrent migrations from nodes starting together.
	conn, err := db.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock(727372)"); err != nil {
		return err
	}
	defer conn.Exec(context.Background(), "SELECT pg_advisory_unlock(727372)")
	for i, stmt := range schemaStatements {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("migration statement %d: %w", i, err)
		}
	}
	return nil
}

var schemaStatements = []string{
	`CREATE TABLE IF NOT EXISTS users (
		PRIMARY KEY (id),
		id            UUID          NOT NULL,
		username      VARCHAR(128)  NOT NULL CONSTRAINT users_username_key UNIQUE,
		display_name  VARCHAR(255),
		avatar_url    VARCHAR(512),
		lang_tag      VARCHAR(18)   NOT NULL DEFAULT 'en',
		location      VARCHAR(255),
		timezone      VARCHAR(255),
		metadata      JSONB         NOT NULL DEFAULT '{}',
		wallet        JSONB         NOT NULL DEFAULT '{}',
		email         VARCHAR(255)  UNIQUE,
		password      BYTEA         CHECK (length(password) < 32000),
		facebook_id   VARCHAR(128)  UNIQUE,
		google_id     VARCHAR(128)  UNIQUE,
		gamecenter_id VARCHAR(128)  UNIQUE,
		steam_id      VARCHAR(128)  UNIQUE,
		custom_id     VARCHAR(128)  UNIQUE,
		edge_count    INT           NOT NULL DEFAULT 0 CHECK (edge_count >= 0),
		create_time   TIMESTAMPTZ   NOT NULL DEFAULT now(),
		update_time   TIMESTAMPTZ   NOT NULL DEFAULT now(),
		verify_time   TIMESTAMPTZ   NOT NULL DEFAULT '1970-01-01 00:00:00 UTC',
		disable_time  TIMESTAMPTZ   NOT NULL DEFAULT '1970-01-01 00:00:00 UTC'
	)`,
	`ALTER TABLE users ADD COLUMN IF NOT EXISTS facebook_instant_game_id VARCHAR(128) UNIQUE`,
	`ALTER TABLE users ADD COLUMN IF NOT EXISTS apple_id VARCHAR(128) UNIQUE`,
	`INSERT INTO users (id, username) VALUES ('00000000-0000-0000-0000-000000000000', '') ON CONFLICT(id) DO NOTHING`,
	`CREATE TABLE IF NOT EXISTS user_device (
		PRIMARY KEY (id),
		FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE,
		id      VARCHAR(128) NOT NULL,
		user_id UUID         NOT NULL,
		UNIQUE (user_id, id)
	)`,
	`CREATE TABLE IF NOT EXISTS user_tombstone (
		PRIMARY KEY (create_time, user_id),
		user_id        UUID        NOT NULL UNIQUE,
		create_time    TIMESTAMPTZ NOT NULL DEFAULT now()
	)`,
	`CREATE TABLE IF NOT EXISTS storage (
		PRIMARY KEY (collection, key, user_id),
		FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE,
		collection  VARCHAR(128) NOT NULL,
		key         VARCHAR(128) NOT NULL,
		user_id     UUID         NOT NULL,
		value       JSONB        NOT NULL DEFAULT '{}',
		version     VARCHAR(32)  NOT NULL,
		read        SMALLINT     NOT NULL DEFAULT 1 CHECK (read >= 0),
		write       SMALLINT     NOT NULL DEFAULT 1 CHECK (write >= 0),
		create_time TIMESTAMPTZ  NOT NULL DEFAULT now(),
		update_time TIMESTAMPTZ  NOT NULL DEFAULT now()
	)`,
	`CREATE INDEX IF NOT EXISTS collection_read_user_id_key_idx ON storage (collection, read, user_id, key)`,
	`CREATE INDEX IF NOT EXISTS collection_read_key_user_id_idx ON storage (collection, read, key, user_id)`,
	`CREATE INDEX IF NOT EXISTS collection_user_id_read_key_idx ON storage (collection, user_id, read, key)`,
	`CREATE INDEX IF NOT EXISTS storage_auto_index_fk_user_id_ref_users ON storage (user_id)`,
	`CREATE TABLE IF NOT EXISTS leaderboard (
		PRIMARY KEY (id),
		id             VARCHAR(128) NOT NULL,
		authoritative  BOOLEAN      NOT NULL DEFAULT FALSE,
		sort_order     SMALLINT     NOT NULL DEFAULT 1,
		operator       SMALLINT     NOT NULL DEFAULT 0,
		reset_schedule VARCHAR(64),
		metadata       JSONB        NOT NULL DEFAULT '{}',
		create_time    TIMESTAMPTZ  NOT NULL DEFAULT now(),
		category       SMALLINT     NOT NULL DEFAULT 0 CHECK (category >= 0),
		description    VARCHAR(255) NOT NULL DEFAULT '',
		duration       INT          NOT NULL DEFAULT 0 CHECK (duration >= 0),
		end_time       TIMESTAMPTZ  NOT NULL DEFAULT '1970-01-01 00:00:00 UTC',
		join_required  BOOLEAN      NOT NULL DEFAULT FALSE,
		max_size       INT          NOT NULL DEFAULT 100000000 CHECK (max_size > 0),
		max_num_score  INT          NOT NULL DEFAULT 1000000 CHECK (max_num_score > 0),
		title          VARCHAR(255) NOT NULL DEFAULT '',
		size           INT          NOT NULL DEFAULT 0,
		start_time     TIMESTAMPTZ  NOT NULL DEFAULT now()
	)`,
	`ALTER TABLE leaderboard ADD COLUMN IF NOT EXISTS enable_ranks boolean DEFAULT true`,
	`CREATE TABLE IF NOT EXISTS leaderboard_record (
		PRIMARY KEY (leaderboard_id, expiry_time, score, subscore, owner_id),
		FOREIGN KEY (leaderboard_id) REFERENCES leaderboard (id) ON DELETE CASCADE,
		leaderboard_id VARCHAR(128)  NOT NULL,
		owner_id       UUID          NOT NULL,
		username       VARCHAR(128),
		score          BIGINT        NOT NULL DEFAULT 0 CHECK (score >= 0),
		subscore       BIGINT        NOT NULL DEFAULT 0 CHECK (subscore >= 0),
		num_score      INT           NOT NULL DEFAULT 1 CHECK (num_score >= 0),
		metadata       JSONB         NOT NULL DEFAULT '{}',
		create_time    TIMESTAMPTZ   NOT NULL DEFAULT now(),
		update_time    TIMESTAMPTZ   NOT NULL DEFAULT now(),
		expiry_time    TIMESTAMPTZ   NOT NULL DEFAULT '1970-01-01 00:00:00 UTC',
		max_num_score  INT           NOT NULL DEFAULT 1000000 CHECK (max_num_score > 0),
		UNIQUE (owner_id, leaderboard_id, expiry_time)
	)`,
	`CREATE INDEX IF NOT EXISTS owner_id_expiry_time_leaderboard_id_idx ON leaderboard_record (owner_id, expiry_time, leaderboard_id)`,
	`CREATE TABLE IF NOT EXISTS wallet_ledger (
		PRIMARY KEY (user_id, create_time, id),
		FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE,
		id          UUID        NOT NULL UNIQUE,
		user_id     UUID        NOT NULL,
		changeset   JSONB       NOT NULL,
		metadata    JSONB       NOT NULL,
		create_time TIMESTAMPTZ NOT NULL DEFAULT now(),
		update_time TIMESTAMPTZ NOT NULL DEFAULT now()
	)`,
	`CREATE TABLE IF NOT EXISTS purchase (
		PRIMARY KEY (transaction_id),
		create_time    TIMESTAMPTZ  NOT NULL DEFAULT now(),
		environment    SMALLINT     NOT NULL DEFAULT 0,
		product_id     VARCHAR(512) NOT NULL,
		purchase_time  TIMESTAMPTZ  NOT NULL DEFAULT now(),
		raw_response   JSONB        NOT NULL DEFAULT '{}',
		store          SMALLINT     NOT NULL DEFAULT 0,
		transaction_id VARCHAR(512) NOT NULL CHECK (length(transaction_id) > 0),
		update_time    TIMESTAMPTZ  NOT NULL DEFAULT now(),
		user_id        UUID         NOT NULL,
		refund_time    TIMESTAMPTZ  NOT NULL DEFAULT '1970-01-01 00:00:00 UTC'
	)`,

	// ---- Cluster coordination (new; not part of Nakama) ----
	`CREATE TABLE IF NOT EXISTS cluster_node (
		name         TEXT        PRIMARY KEY,
		address      TEXT        NOT NULL,
		started_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
		heartbeat_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		draining     BOOLEAN     NOT NULL DEFAULT FALSE,
		sessions     INT         NOT NULL DEFAULT 0,
		matches      INT         NOT NULL DEFAULT 0
	)`,
	// One row per live authoritative match. "epoch" increases on every
	// ownership change and fences checkpoint writes from a stale owner.
	`CREATE TABLE IF NOT EXISTS cluster_match (
		id          UUID        PRIMARY KEY,
		node        TEXT        NOT NULL,
		epoch       BIGINT      NOT NULL DEFAULT 1,
		module      TEXT        NOT NULL,
		label       TEXT        NOT NULL DEFAULT '',
		tick_rate   INT         NOT NULL DEFAULT 1,
		size        INT         NOT NULL DEFAULT 0,
		create_time TIMESTAMPTZ NOT NULL DEFAULT now(),
		update_time TIMESTAMPTZ NOT NULL DEFAULT now()
	)`,
	`CREATE INDEX IF NOT EXISTS cluster_match_node_idx ON cluster_match (node)`,
	// Match state checkpoints are ephemeral: UNLOGGED skips the WAL, which
	// makes frequent small writes cheap. Losing them on a database crash only
	// disables failover for in-flight matches.
	`CREATE UNLOGGED TABLE IF NOT EXISTS cluster_match_state (
		id        UUID        PRIMARY KEY,
		epoch     BIGINT      NOT NULL,
		tick      BIGINT      NOT NULL,
		state     BYTEA       NOT NULL,
		presences BYTEA       NOT NULL,
		saved_at  TIMESTAMPTZ NOT NULL DEFAULT now()
	)`,
	// Request counts per API version and app build, used to decide when an
	// old API version can be removed.
	`CREATE TABLE IF NOT EXISTS api_client_usage (
		api_version TEXT        NOT NULL,
		client      TEXT        NOT NULL,
		day         DATE        NOT NULL,
		requests    BIGINT      NOT NULL DEFAULT 0,
		last_seen   TIMESTAMPTZ NOT NULL,
		PRIMARY KEY (api_version, client, day)
	)`,

	// ---- Reporting (new; not part of Nakama). Written by analytics.go. ----
	// One row per started match and one per seat, for games and players per
	// hour/day; one row per user per active hour, for active users and
	// retention. Grafana reads them through the read-only "reporting" views.
	`CREATE TABLE IF NOT EXISTS analytics_match (
		match_id    TEXT        PRIMARY KEY,
		mode        TEXT        NOT NULL,
		private     BOOLEAN     NOT NULL DEFAULT FALSE,
		humans      INT         NOT NULL DEFAULT 0,
		bots        INT         NOT NULL DEFAULT 0,
		entry_fee   BIGINT      NOT NULL DEFAULT 0,
		started_at  TIMESTAMPTZ NOT NULL,
		ended_at    TIMESTAMPTZ,
		duration_s  INT,
		end_reason  TEXT
	)`,
	`CREATE INDEX IF NOT EXISTS analytics_match_started_idx ON analytics_match (started_at)`,
	`CREATE TABLE IF NOT EXISTS analytics_match_player (
		match_id   TEXT        NOT NULL,
		seat       INT         NOT NULL,
		user_id    TEXT        NOT NULL,
		is_bot     BOOLEAN     NOT NULL DEFAULT FALSE,
		started_at TIMESTAMPTZ NOT NULL,
		rank       INT,
		forfeited  BOOLEAN,
		prize      BIGINT,
		PRIMARY KEY (match_id, seat)
	)`,
	`CREATE INDEX IF NOT EXISTS analytics_match_player_started_idx ON analytics_match_player (started_at)`,
	`CREATE INDEX IF NOT EXISTS analytics_match_player_user_idx ON analytics_match_player (user_id, started_at)`,
	`CREATE TABLE IF NOT EXISTS analytics_user_hour (
		hour    TIMESTAMPTZ NOT NULL,
		user_id UUID        NOT NULL,
		client  TEXT        NOT NULL DEFAULT '',
		PRIMARY KEY (hour, user_id)
	)`,
	`CREATE INDEX IF NOT EXISTS analytics_user_hour_user_idx ON analytics_user_hour (user_id, hour)`,
	// Games played on the device (vs computer, pass-and-play), reported by
	// the client once online. game_id is client-generated and dedupes retries.
	`CREATE TABLE IF NOT EXISTS analytics_offline_game (
		user_id     UUID        NOT NULL,
		game_id     TEXT        NOT NULL,
		mode        TEXT        NOT NULL,
		humans      INT         NOT NULL,
		bots        INT         NOT NULL,
		difficulty  TEXT        NOT NULL DEFAULT '',
		started_at  TIMESTAMPTZ NOT NULL,
		duration_s  INT         NOT NULL,
		outcome     TEXT        NOT NULL,
		client      TEXT        NOT NULL DEFAULT '',
		received_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		PRIMARY KEY (user_id, game_id)
	)`,
	`CREATE INDEX IF NOT EXISTS analytics_offline_game_started_idx ON analytics_offline_game (started_at)`,
	// ---- Friends, invites and presence (new; friends.go). Two rows per
	// relation, one from each side. Deleting an account removes them.
	`CREATE TABLE IF NOT EXISTS friendship (
		user_id     UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
		friend_id   UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
		state       SMALLINT    NOT NULL,
		update_time TIMESTAMPTZ NOT NULL DEFAULT now(),
		PRIMARY KEY (user_id, friend_id)
	)`,
	`CREATE INDEX IF NOT EXISTS friendship_friend_idx ON friendship (friend_id)`,
	`CREATE TABLE IF NOT EXISTS friend_code (
		user_id     UUID        PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
		code        TEXT        NOT NULL UNIQUE,
		create_time TIMESTAMPTZ NOT NULL DEFAULT now()
	)`,
	`CREATE TABLE IF NOT EXISTS referral (
		invitee_id    UUID        PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
		referrer_id   UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
		device_id     TEXT        NOT NULL DEFAULT '',
		source        TEXT        NOT NULL DEFAULT '',
		state         SMALLINT    NOT NULL DEFAULT 0,
		create_time   TIMESTAMPTZ NOT NULL DEFAULT now(),
		complete_time TIMESTAMPTZ
	)`,
	`CREATE INDEX IF NOT EXISTS referral_referrer_idx ON referral (referrer_id, complete_time)`,
	`CREATE INDEX IF NOT EXISTS referral_device_idx ON referral (device_id) WHERE device_id <> ''`,
	`CREATE TABLE IF NOT EXISTS room_invite (
		from_id     UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
		to_id       UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
		room_code   TEXT        NOT NULL,
		mode        TEXT        NOT NULL DEFAULT '',
		create_time TIMESTAMPTZ NOT NULL DEFAULT now(),
		PRIMARY KEY (from_id, to_id)
	)`,
	`CREATE INDEX IF NOT EXISTS room_invite_to_idx ON room_invite (to_id, create_time)`,
	`CREATE TABLE IF NOT EXISTS user_presence (
		user_id        UUID        PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
		last_seen      TIMESTAMPTZ NOT NULL,
		in_match_until TIMESTAMPTZ
	)`,
	// Read-only views without personal data (no names, emails, devices,
	// receipts). The Grafana database role may read only this schema.
	`CREATE SCHEMA IF NOT EXISTS reporting`,
	`CREATE OR REPLACE VIEW reporting.users AS
		SELECT id AS user_id, create_time AS created_at,
			CASE WHEN google_id IS NOT NULL THEN 'google' WHEN apple_id IS NOT NULL THEN 'apple'
				WHEN email IS NOT NULL THEN 'email' ELSE 'guest' END AS account_type,
			disable_time > '1970-01-01 00:00:00+00' AS disabled
		FROM users WHERE id <> '00000000-0000-0000-0000-000000000000'`,
	`CREATE OR REPLACE VIEW reporting.deleted_users AS SELECT create_time AS deleted_at FROM user_tombstone`,
	`CREATE OR REPLACE VIEW reporting.matches AS
		SELECT match_id, mode, private, humans, bots, entry_fee, started_at, ended_at, duration_s, end_reason FROM analytics_match`,
	`CREATE OR REPLACE VIEW reporting.match_players AS
		SELECT match_id, seat, user_id, is_bot, started_at, rank, forfeited, prize FROM analytics_match_player`,
	`CREATE OR REPLACE VIEW reporting.user_hours AS SELECT hour, user_id, client FROM analytics_user_hour`,
	`CREATE OR REPLACE VIEW reporting.referrals AS
		SELECT invitee_id, referrer_id, source, state, create_time, complete_time FROM referral`,
	`CREATE OR REPLACE VIEW reporting.friendships AS
		SELECT user_id, friend_id, state, update_time FROM friendship`,
	`CREATE OR REPLACE VIEW reporting.offline_games AS
		SELECT user_id, mode, humans, bots, difficulty, started_at, duration_s, outcome, client FROM analytics_offline_game`,
	`CREATE OR REPLACE VIEW reporting.purchases AS
		SELECT purchase_time, product_id, store, environment, user_id, refund_time > '1970-01-01 00:00:00+00' AS refunded FROM purchase`,
	`CREATE OR REPLACE VIEW reporting.wallet_events AS
		SELECT create_time, user_id, COALESCE(metadata->>'reason', '') AS reason,
			COALESCE(metadata->>'item_id', '') AS item_id,
			COALESCE(CASE WHEN jsonb_typeof(changeset->'coins') = 'number' THEN (changeset->>'coins')::numeric END, 0)::bigint AS coins,
			COALESCE(CASE WHEN jsonb_typeof(changeset->'diamonds') = 'number' THEN (changeset->>'diamonds')::numeric END, 0)::bigint AS diamonds
		FROM wallet_ledger`,
}

func isPgCode(err error, code string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == code
}

// ExecuteInTx runs fn in a transaction, retrying on serialization failures
// and deadlocks.
func ExecuteInTx(ctx context.Context, db *pgxpool.Pool, fn func(pgx.Tx) error) error {
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		err = pgx.BeginFunc(ctx, db, fn)
		if err == nil {
			return nil
		}
		if !isPgCode(err, pgSerializationFailure) && !isPgCode(err, pgDeadlockDetected) {
			return err
		}
		time.Sleep(time.Duration(attempt*10+5) * time.Millisecond)
	}
	return err
}

// dbtx is satisfied by both the pool and a transaction.
type dbtx interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}
