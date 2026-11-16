package server

import (
	"context"
	"io"
	"log/slog"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dev-sssss/tango_backend/server/internal/game"
)

// Runs against a real database: TANGO_TEST_DB=postgres://... go test ./internal/server -run Analytics
func TestAnalyticsWritesReportingRows(t *testing.T) {
	dsn := os.Getenv("TANGO_TEST_DB")
	if dsn == "" {
		t.Skip("set TANGO_TEST_DB to run")
	}
	ctx := context.Background()
	// Own database: the e2e package runs at the same time against TANGO_TEST_DB
	// and resets its schema.
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("database unavailable: %v", err)
	}
	defer admin.Close(ctx)
	const name = "tango_analytics_test"
	if _, err := admin.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	db, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, db); err != nil { // idempotent on every node start
		t.Fatal(err)
	}
	a := NewAnalytics(db, slog.New(slog.NewTextHandler(io.Discard, nil)), NewMetrics(""))
	human, other := "6f1d7c1e-5a77-4a4e-9a51-7c2a3e9f0001", "6f1d7c1e-5a77-4a4e-9a51-7c2a3e9f0002"
	start := time.Now().Add(-5 * time.Minute).Truncate(time.Second)
	a.RecordMatchStart(game.MatchStartFact{MatchID: "m1.node", Mode: "duel_1v1", EntryFee: 100, StartedAt: start,
		Players: []game.MatchPlayerFact{{Seat: 1, UserID: human}, {Seat: 3, UserID: "bot-1", Bot: true}}})
	a.RecordMatchStart(game.MatchStartFact{MatchID: "m1.node", Mode: "duel_1v1", StartedAt: start}) // replay is ignored
	a.RecordMatchEnd(game.MatchEndFact{MatchID: "m1.node", EndedAt: start.Add(4 * time.Minute), DurationSec: 240, EndReason: "normal",
		Results: []game.MatchResultFact{{Seat: 1, Rank: 1, Prize: 180}, {Seat: 3, Rank: 2}}})
	a.RecordOfflineGames(human, []game.OfflineGameFact{
		{GameID: "game-0001", Mode: "vs_computer", Humans: 1, Bots: 1, Difficulty: "hard", StartedAt: start, DurationSec: 300, Outcome: "won", Client: "android-1.0.9"},
		{GameID: "game-0002", Mode: "pass_and_play", Humans: 2, StartedAt: start, DurationSec: 60, Outcome: "quit"},
	})
	a.RecordOfflineGames(human, []game.OfflineGameFact{{GameID: "game-0001", Mode: "vs_computer", Humans: 1, Bots: 1, StartedAt: start, Outcome: "lost"}}) // resend is ignored
	a.RecordOfflineGames("not-a-uuid", []game.OfflineGameFact{{GameID: "game-0009", Mode: "vs_computer", Humans: 1, Bots: 1, StartedAt: start, Outcome: "won"}})
	a.Active(human, "legacy")
	a.Active(human, "android-1.0.7")
	a.Active(other, "legacy")
	a.Active("not-a-uuid", "x")
	a.drain(ctx)
	a.FlushActivity(ctx)
	a.Active(human, "android-1.0.8") // already written this hour: no new row
	a.FlushActivity(ctx)

	var humans, bots, dur int
	var reason string
	if err := db.QueryRow(ctx, `SELECT humans, bots, duration_s, end_reason FROM reporting.matches WHERE match_id = 'm1.node'`).Scan(&humans, &bots, &dur, &reason); err != nil {
		t.Fatal(err)
	}
	if humans != 1 || bots != 1 || dur != 240 || reason != "normal" {
		t.Fatalf("match row: humans=%d bots=%d dur=%d reason=%s", humans, bots, dur, reason)
	}
	var rank, prize int
	if err := db.QueryRow(ctx, `SELECT rank, prize FROM reporting.match_players WHERE match_id = 'm1.node' AND NOT is_bot`).Scan(&rank, &prize); err != nil {
		t.Fatal(err)
	}
	if rank != 1 || prize != 180 {
		t.Fatalf("player row: rank=%d prize=%d", rank, prize)
	}
	rows, err := db.Query(ctx, `SELECT user_id::text, client FROM reporting.user_hours ORDER BY user_id`)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for rows.Next() {
		var u, c string
		if err := rows.Scan(&u, &c); err != nil {
			t.Fatal(err)
		}
		got[u] = c
	}
	if len(got) != 2 || got[human] != "android-1.0.7" || got[other] != "legacy" {
		t.Fatalf("user hours: %v", got)
	}
	var offline, won int
	if err := db.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE outcome = 'won' AND difficulty = 'hard') FROM reporting.offline_games`).Scan(&offline, &won); err != nil {
		t.Fatal(err)
	}
	if offline != 2 || won != 1 {
		t.Fatalf("offline games: %d (won %d)", offline, won)
	}
	for _, v := range []string{"users", "deleted_users", "matches", "match_players", "user_hours", "purchases", "wallet_events", "offline_games"} {
		if _, err := db.Exec(ctx, "SELECT * FROM reporting."+v+" LIMIT 1"); err != nil {
			t.Fatalf("view %s: %v", v, err)
		}
	}
}
