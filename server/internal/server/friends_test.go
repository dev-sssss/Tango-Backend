package server

import (
	"context"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dev-sssss/tango_backend/server/internal/game"
)

func friendsTestDB(t *testing.T, name string) *pgxpool.Pool {
	dsn := os.Getenv("TANGO_TEST_DB")
	if dsn == "" {
		t.Skip("set TANGO_TEST_DB to run")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("database unavailable: %v", err)
	}
	t.Cleanup(func() { admin.Close(context.Background()) })
	if _, err := admin.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)") })
	u, _ := url.Parse(dsn)
	u.Path = "/" + name
	db, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	return db
}

func addUser(t *testing.T, db *pgxpool.Pool, id, name string, age time.Duration) {
	if _, err := db.Exec(context.Background(), `INSERT INTO users (id, username, create_time, wallet) VALUES ($1, $2, now() - $3::interval, '{"coins":0,"diamonds":0}')`,
		id, name, age.String()); err != nil {
		t.Fatal(err)
	}
}

func wallet(t *testing.T, db *pgxpool.Pool, id string) (coins, gems int64) {
	if err := db.QueryRow(context.Background(), `SELECT coalesce((wallet->>'coins')::bigint, 0), coalesce((wallet->>'diamonds')::bigint, 0) FROM users WHERE id = $1`, id).Scan(&coins, &gems); err != nil {
		t.Fatal(err)
	}
	return
}

func list(t *testing.T, f *Friends, id string) map[string]any {
	m, err := f.FriendsList(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func ids(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(map[string]any)["user_id"].(string))
	}
	return out
}

func TestFriendsRequestsBlocksAndPresence(t *testing.T) {
	db := friendsTestDB(t, "tango_friends_test")
	ctx := context.Background()
	f := NewFriends(db, slog.New(slog.NewTextHandler(io.Discard, nil)), NewMetrics(""))
	a, b, c := "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222", "33333333-3333-4333-8333-333333333333"
	addUser(t, db, a, "Alice", time.Hour)
	addUser(t, db, b, "Bob", time.Hour)
	addUser(t, db, c, "Carol", time.Hour)

	if r, _ := f.FriendAdd(ctx, a, "nobody"); r["error"] != "not_found" {
		t.Fatalf("unknown: %v", r)
	}
	if r, err := f.FriendFind(ctx, a, "bob"); err != nil || r["player"].(map[string]any)["user_id"] != b {
		t.Fatalf("find: %v %v", r, err)
	}
	if r, _ := f.FriendFind(ctx, a, "alice"); r["error"] != "self" {
		t.Fatalf("find self: %v", r)
	}
	if got := ids(list(t, f, b)["incoming"]); len(got) != 0 {
		t.Fatalf("finding sent a request: %v", got)
	}
	if r, _ := f.FriendAdd(ctx, a, "alice"); r["error"] != "self" {
		t.Fatalf("self: %v", r)
	}
	if r, _ := f.FriendAdd(ctx, a, "bob"); r["state"] != "sent" {
		t.Fatalf("send: %v", r)
	}
	if got := ids(list(t, f, b)["incoming"]); len(got) != 1 || got[0] != a {
		t.Fatalf("incoming: %v", got)
	}
	if r, _ := f.FriendAction(ctx, b, a, "accept"); r["success"] != true {
		t.Fatalf("accept: %v", r)
	}
	if got := ids(list(t, f, a)["friends"]); len(got) != 1 || got[0] != b {
		t.Fatalf("friends: %v", got)
	}
	// Friend code works too, and a request back is an accept.
	code := list(t, f, c)["code"].(string)
	if r, _ := f.FriendAdd(ctx, b, code); r["state"] != "sent" {
		t.Fatalf("by code: %v", r)
	}
	if r, _ := f.FriendAdd(ctx, c, "Bob"); r["state"] != "friends" {
		t.Fatalf("mutual: %v", r)
	}
	// A player picked from a list is added by id.
	if r, _ := f.FriendAdd(ctx, a, strings.ToUpper(c)); r["state"] != "sent" {
		t.Fatalf("by id: %v", r)
	}
	if r, _ := f.FriendAdd(ctx, a, "44444444-4444-4444-8444-444444444444"); r["error"] != "not_found" {
		t.Fatalf("unknown id: %v", r)
	}
	if r, _ := f.FriendAction(ctx, a, c, "cancel"); r["success"] != true {
		t.Fatalf("cancel: %v", r)
	}
	// Presence.
	f.flush(ctx, map[string]bool{b: true})
	for _, p := range list(t, f, a)["friends"].([]any) {
		if m := p.(map[string]any); m["user_id"] == b && m["status"] != "in_game" {
			t.Fatalf("presence: %v", m)
		}
	}
	// Room invite to a friend only.
	if r, _ := f.RoomInvite(ctx, a, b, "ROOM42", "duel"); r["success"] != true {
		t.Fatalf("invite: %v", r)
	}
	if r, _ := f.RoomInvite(ctx, a, c, "ROOM42", "duel"); r["error"] != "not_friends" {
		t.Fatalf("invite stranger: %v", r)
	}
	if inv := list(t, f, b)["invites"].([]any); len(inv) != 1 {
		t.Fatalf("invites: %v", inv)
	}
	// The invite inbox carries the room's format; answering removes it.
	if r, _ := f.RoomInvite(ctx, a, b, "123456", "classic|2v2"); r["success"] != true {
		t.Fatalf("invite with format: %v", r)
	}
	inbox, err := f.RoomInvites(ctx, b)
	if err != nil || len(inbox) != 1 || inbox[0]["room_code"] != "123456" || inbox[0]["mode"] != "classic" || inbox[0]["format"] != "2v2" {
		t.Fatalf("inbox %v %v", inbox, err)
	}
	if err := f.RoomInviteDismiss(ctx, b, a); err != nil {
		t.Fatal(err)
	}
	if inbox, _ := f.RoomInvites(ctx, b); len(inbox) != 0 {
		t.Fatalf("after reply: %v", inbox)
	}
	// Player IDs are permanent: asking twice gives the same ID.
	id1, _ := f.codeFor(ctx, a)
	id2, _ := f.codeFor(ctx, a)
	if !friendCodeRe.MatchString(id1) || id1 != id2 {
		t.Fatalf("player id %q %q", id1, id2)
	}
	// Block: B disappears for A's searches, the friendship and invites go.
	if r, _ := f.FriendAction(ctx, b, a, "block"); r["success"] != true {
		t.Fatalf("block: %v", r)
	}
	if got := list(t, f, a)["friends"].([]any); len(got) != 0 {
		t.Fatalf("after block: %v", got)
	}
	if r, _ := f.FriendAdd(ctx, a, "bob"); r["error"] != "not_found" {
		t.Fatalf("blocked search: %v", r)
	}
	if got := ids(list(t, f, b)["blocked"]); len(got) != 1 || got[0] != a {
		t.Fatalf("blocked list: %v", got)
	}
	if inv := list(t, f, b)["invites"].([]any); len(inv) != 0 {
		t.Fatalf("invites after block: %v", inv)
	}
	if r, _ := f.FriendAction(ctx, b, a, "unblock"); r["success"] != true {
		t.Fatalf("unblock: %v", r)
	}
	if r, _ := f.FriendAdd(ctx, a, "bob"); r["state"] != "sent" {
		t.Fatalf("after unblock: %v", r)
	}
	if r, _ := f.FriendAction(ctx, a, b, "cancel"); r["success"] != true {
		t.Fatalf("cancel: %v", r)
	}
	if got := list(t, f, b)["incoming"].([]any); len(got) != 0 {
		t.Fatalf("cancelled: %v", got)
	}
	// Account deletion removes everything.
	if _, err := db.Exec(ctx, `DELETE FROM users WHERE id = $1`, c); err != nil {
		t.Fatal(err)
	}
	if got := list(t, f, b)["friends"].([]any); len(got) != 0 {
		t.Fatalf("deleted friend: %v", got)
	}
}

func TestReferralWelcomeAndRewardOnFirstGame(t *testing.T) {
	db := friendsTestDB(t, "tango_referral_test")
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	f := NewFriends(db, logger, NewMetrics(""))
	a := NewAnalytics(db, logger, NewMetrics(""))
	a.gamesFinished = f.GamesFinished
	ref, inv, inv2, old := "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222",
		"33333333-3333-4333-8333-333333333333", "44444444-4444-4444-8444-444444444444"
	addUser(t, db, ref, "Ref", 30*24*time.Hour)
	addUser(t, db, inv, "New", time.Hour)
	addUser(t, db, inv2, "New2", time.Hour)
	addUser(t, db, old, "Old", 10*24*time.Hour)
	code := list(t, f, ref)["code"].(string)

	if r, _ := f.ReferralClaim(ctx, ref, code, "", "manual"); r["error"] != "own_code" {
		t.Fatalf("own: %v", r)
	}
	if r, _ := f.ReferralClaim(ctx, old, code, "", "manual"); r["error"] != "too_late" {
		t.Fatalf("old account: %v", r)
	}
	if r, _ := f.ReferralClaim(ctx, inv, "ZZZZZZZZ", "", "manual"); r["error"] != "invalid_code" {
		t.Fatalf("bad code: %v", r)
	}
	r, err := f.ReferralClaim(ctx, inv, code, "device-1", "install_referrer")
	if err != nil || r["success"] != true || r["welcome_coins"] != int64(welcomeCoins) {
		t.Fatalf("claim: %v %v", r, err)
	}
	if r, _ := f.ReferralClaim(ctx, inv, code, "device-1", "manual"); r["error"] != "already_claimed" {
		t.Fatalf("twice: %v", r)
	}
	if coins, _ := wallet(t, db, inv); coins != welcomeCoins {
		t.Fatalf("welcome coins %d", coins)
	}
	if got := ids(list(t, f, ref)["friends"]); len(got) != 1 || got[0] != inv {
		t.Fatalf("invite makes friends: %v", got)
	}
	// Same device again: recorded, but no bonus and no reward.
	if r, _ := f.ReferralClaim(ctx, inv2, code, "device-1", "manual"); r["success"] != true || r["welcome_coins"] != int64(0) {
		t.Fatalf("same device: %v", r)
	}
	// A quit offline game does not count; a finished one pays once.
	a.RecordOfflineGames(inv, []game.OfflineGameFact{{GameID: "game-0001", Mode: "vs_computer", Humans: 1, Bots: 1, StartedAt: time.Now(), Outcome: "quit"}})
	a.drain(ctx)
	if coins, gems := wallet(t, db, ref); coins != 0 || gems != 0 {
		t.Fatalf("paid on quit: %d %d", coins, gems)
	}
	a.RecordOfflineGames(inv, []game.OfflineGameFact{{GameID: "game-0002", Mode: "vs_computer", Humans: 1, Bots: 1, StartedAt: time.Now(), Outcome: "lost"}})
	a.RecordOfflineGames(inv2, []game.OfflineGameFact{{GameID: "game-0003", Mode: "vs_computer", Humans: 1, Bots: 1, StartedAt: time.Now(), Outcome: "won"}})
	a.drain(ctx)
	f.GamesFinished(ctx, []string{inv}) // another game: no second reward
	if coins, gems := wallet(t, db, ref); coins != referralRewardCoins || gems != referralRewardGems {
		t.Fatalf("reward: %d %d", coins, gems)
	}
	ref2 := list(t, f, ref)["referral"].(map[string]any)
	if ref2["joined"] != 2 || ref2["rewarded"] != 1 || ref2["coins_earned"] != referralRewardCoins {
		t.Fatalf("progress: %v", ref2)
	}
	var reason string
	if err := db.QueryRow(ctx, `SELECT metadata->>'reason' FROM wallet_ledger WHERE user_id = $1`, ref).Scan(&reason); err != nil || reason != "referral_reward" {
		t.Fatalf("ledger: %q %v", reason, err)
	}
	// An online game finishes the referral too.
	inv3 := "55555555-5555-4555-8555-555555555555"
	addUser(t, db, inv3, "New3", time.Hour)
	if r, _ := f.ReferralClaim(ctx, inv3, code, "device-3", "manual"); r["success"] != true {
		t.Fatalf("claim 3: %v", r)
	}
	start := time.Now().Add(-5 * time.Minute)
	a.RecordMatchStart(game.MatchStartFact{MatchID: "m9.node", Mode: "duel_1v1", StartedAt: start,
		Players: []game.MatchPlayerFact{{Seat: 1, UserID: inv3}, {Seat: 3, UserID: "bot-1", Bot: true}}})
	a.RecordMatchEnd(game.MatchEndFact{MatchID: "m9.node", EndedAt: time.Now(), DurationSec: 300, EndReason: "normal",
		Results: []game.MatchResultFact{{Seat: 1, Rank: 2}, {Seat: 3, Rank: 1}}})
	a.drain(ctx)
	if coins, gems := wallet(t, db, ref); coins != 2*referralRewardCoins || gems != 2*referralRewardGems {
		t.Fatalf("online reward: %d %d", coins, gems)
	}
}
