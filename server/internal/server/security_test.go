package server

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"
)

func testGuard(t *testing.T, mutate func(*Config)) *Guard {
	t.Helper()
	cfg := NewConfig()
	cfg.Socket.TrustedProxyHeader = "X-Forwarded-For"
	if mutate != nil {
		mutate(cfg)
	}
	return NewGuard(cfg, NewMetrics(""))
}

func TestClientIPIgnoresSpoofedForwardingHeaders(t *testing.T) {
	g := testGuard(t, nil)
	// Direct internet client: the header is the client's own claim.
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "203.0.113.9:5555"
	r.Header.Set("X-Forwarded-For", "1.2.3.4")
	if got := g.ClientIP(r); got != "203.0.113.9" {
		t.Fatalf("untrusted peer: got %s", got)
	}
	// Through nginx (trusted): the right-most untrusted hop is the client,
	// whatever the client prepended.
	r.RemoteAddr = "172.18.0.5:4444"
	r.Header.Set("X-Forwarded-For", "1.2.3.4, 198.51.100.7")
	if got := g.ClientIP(r); got != "198.51.100.7" {
		t.Fatalf("via proxy: got %s", got)
	}
	r.Header.Set("X-Forwarded-For", "198.51.100.7, 10.0.0.2")
	if got := g.ClientIP(r); got != "198.51.100.7" {
		t.Fatalf("via two proxies: got %s", got)
	}
}

func TestLimiterRefillsAndBlocks(t *testing.T) {
	l := NewLimiter(10, 3)
	for i := 0; i < 3; i++ {
		if !l.Allow("k") {
			t.Fatalf("burst %d rejected", i)
		}
	}
	ok, wait := l.Take("k")
	if ok || wait <= 0 || wait > 200*time.Millisecond {
		t.Fatalf("expected a short wait, got %v %v", ok, wait)
	}
	if !l.Allow("other") {
		t.Fatal("keys are independent")
	}
	time.Sleep(120 * time.Millisecond)
	if !l.Allow("k") {
		t.Fatal("bucket refills over time")
	}
}

func TestPasswordLoginLimitsPerAccount(t *testing.T) {
	g := testGuard(t, func(c *Config) { c.Security.PasswordAuthPerAccountPerMin = 3 })
	allowed := 0
	for i := 0; i < 10; i++ {
		r := httptest.NewRequest("POST", "/", nil)
		r.RemoteAddr = "203.0.113." + string(rune('1'+i)) + ":1" // a botnet: new IP each try
		w := httptest.NewRecorder()
		if g.LimitAuth(w, r, "Victim@Example.com", false) {
			allowed++
		} else if w.Code != 429 || w.Header().Get("Retry-After") == "" {
			t.Fatalf("rejection should be 429 with Retry-After, got %d", w.Code)
		}
	}
	if allowed != 3 {
		t.Fatalf("one account should get 3 password attempts per minute across IPs, got %d", allowed)
	}
}

func TestSocketCaps(t *testing.T) {
	g := testGuard(t, func(c *Config) { c.Security.SocketsPerUser = 2; c.Security.MaxSessions = 10 })
	if ok, _ := g.OpenSocket("1.1.1.1", "u", 0); !ok {
		t.Fatal("first socket")
	}
	if ok, _ := g.OpenSocket("1.1.1.1", "u", 1); !ok {
		t.Fatal("second socket")
	}
	if ok, why := g.OpenSocket("1.1.1.1", "u", 2); ok || why != "sockets_per_user" {
		t.Fatalf("third socket for one user: %v %s", ok, why)
	}
	g.CloseSocket("u")
	if ok, _ := g.OpenSocket("1.1.1.1", "u", 1); !ok {
		t.Fatal("a closed socket frees the slot")
	}
	if ok, why := g.OpenSocket("1.1.1.2", "v", 10); ok || why != "server_full" {
		t.Fatalf("node cap: %v %s", ok, why)
	}
}

func TestSocketFloodIsDroppedThenClosed(t *testing.T) {
	r := newSocketRate(25)
	processed, closed := 0, false
	for i := 0; i < 1000 && !closed; i++ {
		p, abusive := r.allow(200)
		if p {
			processed++
		}
		closed = abusive
	}
	if processed > 60 || !closed {
		t.Fatalf("flood: processed %d, closed %v", processed, closed)
	}
	// Normal play (a few messages a second) is never limited.
	n := newSocketRate(25)
	for i := 0; i < 20; i++ {
		if p, _ := n.allow(200); !p {
			t.Fatal("normal traffic dropped")
		}
	}
}

func TestPasswordHashingIsBounded(t *testing.T) {
	g := testGuard(t, func(c *Config) { c.Security.PasswordHashConcurrency = 1 })
	release, err := g.HashSlot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := g.HashSlot(ctx); err == nil {
		t.Fatal("second hash must wait while the only slot is busy")
	}
	release()
	if r2, err := g.HashSlot(context.Background()); err != nil {
		t.Fatal(err)
	} else {
		r2()
	}
}

func TestProductionRefusesCheats(t *testing.T) {
	if _, err := LoadConfig([]string{"--runtime.env", "DEPLOYMENT_ENVIRONMENT=production,TANGO_ALLOW_CHEATS=true"}); err == nil {
		t.Fatal("cheats in production must be refused")
	}
	if _, err := LoadConfig([]string{"--runtime.env", "DEPLOYMENT_ENVIRONMENT=nonprod,TANGO_ALLOW_CHEATS=true"}); err != nil {
		t.Fatalf("cheats stay available for internal testing: %v", err)
	}
}

func TestAdminListensOnLoopbackByDefault(t *testing.T) {
	if NewConfig().Cluster.AdminAddress != "127.0.0.1" {
		t.Fatal("admin must default to loopback")
	}
}
