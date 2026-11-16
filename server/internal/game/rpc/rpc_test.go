package rpc

import (
	"context"
	"testing"

	"github.com/dev-sssss/tango_backend/server/internal/game"
	"github.com/dev-sssss/tango_backend/server/internal/game/memnk"
)

const uid = "a0000000-0000-4000-8000-000000000001"

func setup(env map[string]string) (*Service, *memnk.NK, *Context) {
	nk := memnk.New(1, 1_700_000_000_000, env)
	nk.AddUser(uid, "player1", "", nil)
	return NewService(nk, nil), nk, &Context{Ctx: context.Background(), UserID: uid, Username: "player1"}
}

func call(t *testing.T, s *Service, c *Context, id, payload string) map[string]any {
	t.Helper()
	r := s.Registry()[id]
	if r == nil {
		t.Fatalf("no rpc %s", id)
	}
	out, err := r.Fn(c, payload)
	if err != nil {
		t.Fatalf("%s: %v", id, err)
	}
	m, _ := game.DecodeObject(out)
	return m
}

func TestCatalogValid(t *testing.T) {
	cat, err := DefaultCatalog()
	if err != nil || len(cat.Items) == 0 {
		t.Fatalf("catalog: %v", err)
	}
	seen := map[string]bool{}
	for _, it := range cat.Items {
		if seen[it.ID] {
			t.Fatalf("duplicate %s", it.ID)
		}
		seen[it.ID] = true
	}
	for cat2, id := range game.DefaultLoadout {
		it := cat.item(id)
		if it == nil || !it.Default || it.Category != cat2 {
			t.Fatalf("default %s/%s missing from catalog", cat2, id)
		}
	}
}

func TestProfileStartingWalletOnce(t *testing.T) {
	s, nk, c := setup(nil)
	r := call(t, s, c, "tango.profile.get", "")
	if r["success"] != true {
		t.Fatalf("%v", r)
	}
	call(t, s, c, "tango.profile.get", "")
	if w := nk.Wallet(uid); w["coins"] != 2000 || w["diamonds"] != 20 {
		t.Fatalf("starting wallet granted wrong: %v", w)
	}
}

func TestUpdateName(t *testing.T) {
	s, _, c := setup(nil)
	if r := call(t, s, c, "tango.profile.update", `{"name":"x"}`); r["error"] != "INVALID_NAME" {
		t.Fatalf("%v", r)
	}
	r := call(t, s, c, "tango.profile.update", `{"name":"Robin 07"}`)
	if r["profile"].(map[string]any)["name"] != "Robin 07" {
		t.Fatalf("%v", r)
	}
}

func TestPurchaseAndEquip(t *testing.T) {
	s, nk, c := setup(nil)
	call(t, s, c, "tango.profile.get", "")
	if r := call(t, s, c, "tango.inventory.equip", `{"category":"frame","item_id":"frame_silver"}`); r["error"] != "NOT_OWNED" {
		t.Fatalf("%v", r)
	}
	if r := call(t, s, c, "tango.shop.purchase", `{"item_id":"frame_gold"}`); r["error"] != "INSUFFICIENT_FUNDS" {
		t.Fatalf("%v", r)
	}
	_, _, _ = nk.WalletUpdate(context.Background(), uid, map[string]int64{"coins": 5000}, nil, false)
	r := call(t, s, c, "tango.shop.purchase", `{"item_id":"frame_silver"}`)
	if r["success"] != true || nk.Wallet(uid)["coins"] != 4000 {
		t.Fatalf("%v %v", r, nk.Wallet(uid))
	}
	if r := call(t, s, c, "tango.shop.purchase", `{"item_id":"frame_silver"}`); r["error"] != "ALREADY_OWNED" {
		t.Fatalf("%v", r)
	}
	r = call(t, s, c, "tango.inventory.equip", `{"category":"frame","item_id":"frame_silver"}`)
	if r["profile"].(map[string]any)["frame"] != "frame_silver" {
		t.Fatalf("%v", r)
	}
	inv := call(t, s, c, "tango.inventory.get", "")
	if inv["equipped"].(map[string]any)["frame"] != "frame_silver" {
		t.Fatalf("%v", inv)
	}
}

func TestAdRewardLimits(t *testing.T) {
	s, nk, c := setup(nil)
	r := call(t, s, c, "tango.ads.claim_reward", "")
	coins := int64(r["coins"].(float64))
	if coins < 250 || coins > 300 {
		t.Fatalf("reward %d", coins)
	}
	if r := call(t, s, c, "tango.ads.claim_reward", ""); r["error"] != "TOO_SOON" {
		t.Fatalf("%v", r)
	}
	for i := 0; i < 20; i++ {
		nk.Advance(31_000)
		call(t, s, c, "tango.ads.claim_reward", "")
	}
	if r := call(t, s, c, "tango.ads.claim_reward", ""); r["error"] != "DAILY_LIMIT" && r["error"] != "TOO_SOON" {
		t.Fatalf("%v", r)
	}
	if w := nk.Wallet(uid)["coins"]; w > 10*300 {
		t.Fatalf("daily cap exceeded: %d", w)
	}
}

func TestDevGrantOnlyWithCheats(t *testing.T) {
	s, _, c := setup(nil)
	if r := call(t, s, c, "tango.dev.grant", ""); r["error"] != "NOT_AVAILABLE" {
		t.Fatalf("%v", r)
	}
	s2, nk2, c2 := setup(map[string]string{"TANGO_ALLOW_CHEATS": "true", "DEPLOYMENT_ENVIRONMENT": "nonprod"})
	call(t, s2, c2, "tango.dev.grant", "")
	if nk2.Wallet(uid)["coins"] != 10000 {
		t.Fatal("dev grant failed")
	}
	s3, _, c3 := setup(map[string]string{"TANGO_ALLOW_CHEATS": "true", "DEPLOYMENT_ENVIRONMENT": "production"})
	if r := call(t, s3, c3, "tango.dev.grant", ""); r["error"] != "NOT_AVAILABLE" {
		t.Fatalf("production must refuse: %v", r)
	}
}

func TestQuickJoinValidation(t *testing.T) {
	s, nk, c := setup(nil)
	created := 0
	nk.MatchCreateFn = func(ctx context.Context, module string, params map[string]any) (string, error) {
		created++
		return "match-1.node1", nil
	}
	nk.MatchGetFn = func(ctx context.Context, id string) (*game.MatchInfo, error) {
		return &game.MatchInfo{MatchID: id, Label: `{"status":"WAITING","players":1,"max":2}`}, nil
	}
	if r := call(t, s, c, "tango.match.quick_join", `{"mode":"classic","format":"1v1","fee":300}`); r["error"] != "INVALID_REQUEST" {
		t.Fatalf("fee not in list must fail: %v", r)
	}
	if r := call(t, s, c, "tango.match.quick_join", `{"mode":"classic","format":"1v1","fee":5000}`); r["error"] != "INSUFFICIENT_COINS" {
		t.Fatalf("%v", r)
	}
	r := call(t, s, c, "tango.match.quick_join", `{"mode":"classic","format":"1v1","fee":200}`)
	if r["status"] != "created" || created != 1 {
		t.Fatalf("%v", r)
	}
	r = call(t, s, c, "tango.match.quick_join", `{"mode":"classic","format":"1v1","fee":200}`)
	if r["status"] != "matched" || created != 1 {
		t.Fatalf("second player should be matched into the waiting match: %v", r)
	}
}

func TestRoomCreateAndJoin(t *testing.T) {
	s, nk, c := setup(nil)
	var params map[string]any
	nk.MatchCreateFn = func(ctx context.Context, module string, p map[string]any) (string, error) {
		params = p
		return "room-1.node1", nil
	}
	nk.MatchGetFn = func(ctx context.Context, id string) (*game.MatchInfo, error) {
		return &game.MatchInfo{MatchID: id, Label: `{"status":"WAITING","players":1,"max":4,"fee":0,"mode":"no_mercy","format":"2v2"}`}, nil
	}
	r := call(t, s, c, "tango.room.create", `{"mode":"no_mercy","format":"2v2","settings":{"turn_seconds":20,"emotes":false,"spectators":true}}`)
	code, _ := r["room_code"].(string)
	if len(code) != 6 || params["private"] != true {
		t.Fatalf("%v", r)
	}
	st := params["settings"].(map[string]any)
	if st["turn_seconds"] != int64(20) || st["emotes"] != false || st["spectators"] != true {
		t.Fatalf("settings %v", st)
	}
	j := call(t, s, c, "tango.room.join", `{"room_code":"`+code+`"}`)
	if j["match_id"] != "room-1.node1" {
		t.Fatalf("%v", j)
	}
	if r := call(t, s, c, "tango.room.join", `{"room_code":"000000"}`); r["error"] != "ROOM_NOT_FOUND" {
		t.Fatalf("%v", r)
	}
}
