package e2e

import (
	"bytes"
	"io"
	"net/http"
	"syscall"
	"testing"
	"time"
)

func rpcStatus(t *testing.T, base, id, query, auth, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", base+"/v2/rpc/"+id+"?unwrap=true"+query, bytes.NewBufferString(body))
	req.Header.Set("Authorization", auth)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func newBots(t *testing.T, gws []string, names ...string) []*Bot {
	var bots []*Bot
	for _, n := range names {
		b := NewBot(n, serverKey, gws)
		if err := b.Authenticate(); err != nil {
			t.Fatalf("auth %s: %v", n, err)
		}
		if r, err := b.RPC("tango.profile.get", map[string]any{}); err != nil || r["success"] != true {
			t.Fatalf("profile %s: %v %v", n, r, err)
		}
		bots = append(bots, b)
	}
	return bots
}

func coins(t *testing.T, b *Bot) int64 {
	r, err := b.RPC("tango.wallet.get", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	return int64(r["wallet"].(map[string]any)["coins"].(float64))
}

// TestSingleNodeMatch: two players matchmake into a 1v1 with a 200-coin fee
// through the real HTTP API and socket, play to the end, and the coins add up.
func TestSingleNodeMatch(t *testing.T) {
	c := newCluster(t)
	defer c.stopAll()
	n := &node{name: "solo", roles: "gateway,game", base: 17450, log: c.dir + "/solo.log"}
	c.start(n)
	gws := []string{gatewayURL(n)}
	bots := newBots(t, gws, "alice", "bob")
	defer func() {
		for _, b := range bots {
			b.Stop()
		}
	}()
	if a, b := coins(t, bots[0]), coins(t, bots[1]); a != 2000 || b != 2000 {
		t.Fatalf("starting wallets %d %d", a, b)
	}
	r1, err := bots[0].RPC("tango.match.quick_join", map[string]any{"mode": "classic", "format": "1v1", "fee": 200})
	if err != nil || r1["status"] != "created" {
		t.Fatalf("quick join 1: %v %v", r1, err)
	}
	if err := bots[0].Play(r1["match_id"].(string)); err != nil {
		t.Fatal(err)
	}
	r2, err := bots[1].RPC("tango.match.quick_join", map[string]any{"mode": "classic", "format": "1v1", "fee": 200})
	if err != nil || r2["status"] != "matched" || r2["match_id"] != r1["match_id"] {
		t.Fatalf("quick join 2: %v %v", r2, err)
	}
	if err := bots[1].Play(r2["match_id"].(string)); err != nil {
		t.Fatal(err)
	}
	waitGameOver(t, bots, 4*time.Minute)
	total := coins(t, bots[0]) + coins(t, bots[1])
	if total != 4000 {
		t.Fatalf("1v1 payout must be zero-sum, total %d\n%s", total, tail(n.log))
	}
	res := bots[0].Result()["results"].([]any)
	first := res[0].(map[string]any)
	if first["coins"].(float64) != 200 || first["trophies"].(float64) != 30 {
		t.Fatalf("winner row %v", first)
	}
	if bots[0].Regressions.Load() != 0 || bots[1].Regressions.Load() != 0 {
		t.Fatal("state went backwards")
	}

	// Access rules: admin RPCs need server credentials; the dev grant is off.
	auth := "Bearer " + bots[0].token
	if code, _ := rpcStatus(t, gws[0], "tango.admin.grant", "", auth, `{}`); code == 200 {
		t.Fatal("admin RPC must refuse a player session")
	}
	if r, _ := bots[0].RPC("tango.dev.grant", map[string]any{}); r["error"] != "NOT_AVAILABLE" {
		t.Fatalf("dev grant must be off by default: %v", r)
	}
	if code, body := rpcStatus(t, gws[0], "tango.admin.grant", "&http_key="+httpKey, "", `{"user_id":"`+bots[0].UserID+`","coins":5}`); code != 200 {
		t.Fatalf("admin RPC with http key: %d %s", code, body)
	}
}

// TestBotFill: a lone player gets bots after the 15 s wait and the match
// plays to the end.
func TestBotFill(t *testing.T) {
	c := newCluster(t)
	defer c.stopAll()
	n := &node{name: "solo", roles: "gateway,game", base: 17460, log: c.dir + "/solo.log"}
	c.start(n)
	bots := newBots(t, []string{gatewayURL(n)}, "lonely")
	defer bots[0].Stop()
	r, err := bots[0].RPC("tango.match.quick_join", map[string]any{"mode": "classic", "format": "2v2", "fee": 400})
	if err != nil {
		t.Fatal(err)
	}
	if err := bots[0].Play(r["match_id"].(string)); err != nil {
		t.Fatal(err)
	}
	waitGameOver(t, bots, 10*time.Minute)
	res := bots[0].Result()["results"].([]any)
	if len(res) != 4 {
		t.Fatalf("expected 4 players, got %d", len(res))
	}
}

// TestClusterRollingDeploy: two gateways and two game nodes. Four players
// in a private 2v2 room connect through different gateways. During the game
// the game node hosting the match is drained (deploy) and the match must
// move live to the other node; a gateway is restarted; then the owner is
// killed with SIGKILL and the survivor must resume from the checkpoint.
// The game must finish with no state rollback.
func TestClusterRollingDeploy(t *testing.T) {
	c := newCluster(t)
	defer c.stopAll()
	gw1 := &node{name: "gw1", roles: "gateway", base: 17500, log: c.dir + "/gw1.log"}
	gw2 := &node{name: "gw2", roles: "gateway", base: 17510, log: c.dir + "/gw2.log"}
	g1 := &node{name: "game1", roles: "game", base: 17520, log: c.dir + "/game1.log", extra: []string{"--socket.port", "17520"}}
	g2 := &node{name: "game2", roles: "game", base: 17530, log: c.dir + "/game2.log"}
	for _, n := range []*node{g1, g2, gw1, gw2} {
		c.start(n)
	}
	gwsA := []string{gatewayURL(gw1), gatewayURL(gw2)}
	gwsB := []string{gatewayURL(gw2), gatewayURL(gw1)}
	host := newBots(t, gwsA, "host")[0]
	r, err := host.RPC("tango.room.create", map[string]any{"mode": "no_mercy", "format": "2v2", "settings": map[string]any{"turn_seconds": 15}})
	if err != nil || r["success"] != true {
		t.Fatalf("room: %v %v", r, err)
	}
	code := r["room_code"].(string)
	matchID := r["match_id"].(string)
	bots := []*Bot{host}
	for i, name := range []string{"p2", "p3", "p4"} {
		gws := gwsA
		if i%2 == 0 {
			gws = gwsB
		}
		b := newBots(t, gws, name)[0]
		j, err := b.RPC("tango.room.join", map[string]any{"room_code": code})
		if err != nil || j["match_id"] != matchID {
			t.Fatalf("join %s: %v %v", name, j, err)
		}
		bots = append(bots, b)
	}
	defer func() {
		for _, b := range bots {
			b.Stop()
		}
	}()
	for _, b := range bots {
		if err := b.Play(matchID); err != nil {
			t.Fatal(err)
		}
	}
	waitProgress(t, "start", bots, 5, 60*time.Second)

	// 1. Rolling deploy of the game node that owns the match.
	owner, epoch := c.owner(matchID)
	var src *node
	if owner == g1.name {
		src = g1
	} else {
		src = g2
	}
	took := c.stop(src, syscall.SIGTERM, 30*time.Second)
	newOwner, newEpoch := c.owner(matchID)
	t.Logf("drained %s in %v; match now on %s (epoch %d -> %d)", src.name, took, newOwner, epoch, newEpoch)
	if newOwner == owner || newEpoch <= epoch {
		t.Fatalf("match did not move: %s epoch %d", newOwner, newEpoch)
	}
	waitProgress(t, "after handover", bots, 5, 60*time.Second)
	c.start(src)

	// 2. Restart a gateway: its players reconnect through the other one.
	c.stop(gw1, syscall.SIGTERM, 30*time.Second)
	c.start(gw1)
	waitProgress(t, "after gateway restart", bots, 5, 60*time.Second)

	// 3. Crash the owner: the survivor restores the match from its checkpoint.
	owner, _ = c.owner(matchID)
	victim := g1
	if owner == g2.name {
		victim = g2
	}
	c.stop(victim, syscall.SIGKILL, 10*time.Second)
	waitProgress(t, "after crash", bots, 5, 90*time.Second)
	after, _ := c.owner(matchID)
	t.Logf("after kill -9 of %s the match runs on %s", victim.name, after)

	waitGameOver(t, bots, 6*time.Minute)
	for _, b := range bots {
		if b.MaxRollback.Load() > 2 {
			t.Fatalf("%s saw a rollback of %d revisions", b.Name, b.MaxRollback.Load())
		}
	}
}
