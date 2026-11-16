package server

import (
	"context"
	"math"
	"net"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Abuse protection for the client API. Volumetric (network-level) floods are
// stopped upstream (cloud provider, load balancer, nginx); these limits stop
// what reaches the application: credential stuffing, account spam, request
// and message floods, and connection hoarding.
//
// Many Indian mobile users share one public IP (carrier-grade NAT), so
// per-IP limits are generous and only guard expensive or unauthenticated
// work; per-user and per-socket limits do the fine-grained work.

// SecurityConfig is the `security:` config section.
type SecurityConfig struct {
	// TrustedProxies may set the client address header (X-Forwarded-For).
	// Requests from anywhere else are identified by their socket address.
	TrustedProxies []string `yaml:"trusted_proxies"`

	AuthPerIPPerMinute           int `yaml:"auth_per_ip_per_minute"`
	PasswordAuthPerIPPerMinute   int `yaml:"password_auth_per_ip_per_minute"`
	PasswordAuthPerAccountPerMin int `yaml:"password_auth_per_account_per_minute"`
	RPCPerUserPerSecond          int `yaml:"rpc_per_user_per_second"`
	RPCPerIPPerSecond            int `yaml:"rpc_per_ip_per_second"`
	SocketConnectsPerIPPerMin    int `yaml:"socket_connects_per_ip_per_minute"`
	SocketsPerUser               int `yaml:"sockets_per_user"`
	SocketMessagesPerSecond      int `yaml:"socket_messages_per_second"`
	// SocketAbuseDrops closes a socket after this many dropped messages
	// within 10 seconds.
	SocketAbuseDrops int `yaml:"socket_abuse_drops"`
	// MaxSessions caps sockets per gateway node (0 = no cap).
	MaxSessions int `yaml:"max_sessions"`
	// PasswordHashConcurrency caps simultaneous bcrypt operations, which
	// cost ~100 ms of CPU each (0 = number of CPUs).
	PasswordHashConcurrency int `yaml:"password_hash_concurrency"`
}

func defaultSecurity() SecurityConfig {
	return SecurityConfig{
		TrustedProxies:               []string{"127.0.0.1/32", "::1/128", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"},
		AuthPerIPPerMinute:           120,
		PasswordAuthPerIPPerMinute:   30,
		PasswordAuthPerAccountPerMin: 10,
		RPCPerUserPerSecond:          20,
		RPCPerIPPerSecond:            300,
		SocketConnectsPerIPPerMin:    240,
		SocketsPerUser:               4,
		SocketMessagesPerSecond:      25,
		SocketAbuseDrops:             200,
		MaxSessions:                  50000,
	}
}

// ---------------------------------------------------------------- token buckets

type bucket struct {
	tokens float64
	last   time.Time
}

// Limiter is a keyed token bucket: rate tokens per second, up to burst.
type Limiter struct {
	rate, burst float64
	mu          sync.Mutex
	buckets     map[string]*bucket
}

func NewLimiter(perSecond, burst float64) *Limiter {
	return &Limiter{rate: perSecond, burst: burst, buckets: map[string]*bucket{}}
}

func perMinute(n int) *Limiter { return NewLimiter(float64(n)/60, float64(n)) }

// Allow takes one token for key. A nil limiter or a zero rate allows all.
func (l *Limiter) Allow(key string) bool {
	ok, _ := l.Take(key)
	return ok
}

// Take returns whether the call is allowed and, if not, how long to wait.
func (l *Limiter) Take(key string) (bool, time.Duration) {
	if l == nil || l.rate <= 0 {
		return true, 0
	}
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	b.tokens = math.Min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.rate)
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	return false, time.Duration((1 - b.tokens) / l.rate * float64(time.Second))
}

// sweep drops buckets that have refilled completely (idle keys).
func (l *Limiter) sweep() {
	if l == nil || l.rate <= 0 {
		return
	}
	now := time.Now()
	full := time.Duration(l.burst / l.rate * float64(time.Second))
	l.mu.Lock()
	for k, b := range l.buckets {
		if now.Sub(b.last) > full {
			delete(l.buckets, k)
		}
	}
	l.mu.Unlock()
}

func (l *Limiter) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

// ---------------------------------------------------------------- guard

// Guard holds the limiters and the trusted-proxy list.
type Guard struct {
	cfg     SecurityConfig
	metrics *Metrics
	proxies []*net.IPNet
	header  string

	auth, passIP, passAccount, rpcUser, rpcIP, socketIP *Limiter

	hashSlots chan struct{}

	mu          sync.Mutex
	userSockets map[string]int
}

func NewGuard(cfg *Config, metrics *Metrics) *Guard {
	s := cfg.Security
	g := &Guard{cfg: s, metrics: metrics, header: cfg.Socket.TrustedProxyHeader,
		auth: perMinute(s.AuthPerIPPerMinute), passIP: perMinute(s.PasswordAuthPerIPPerMinute),
		passAccount: perMinute(s.PasswordAuthPerAccountPerMin),
		rpcUser:     NewLimiter(float64(s.RPCPerUserPerSecond), float64(s.RPCPerUserPerSecond*2)),
		rpcIP:       NewLimiter(float64(s.RPCPerIPPerSecond), float64(s.RPCPerIPPerSecond*2)),
		socketIP:    perMinute(s.SocketConnectsPerIPPerMin),
		userSockets: map[string]int{}}
	for _, c := range s.TrustedProxies {
		if _, n, err := net.ParseCIDR(strings.TrimSpace(c)); err == nil {
			g.proxies = append(g.proxies, n)
		}
	}
	n := s.PasswordHashConcurrency
	if n <= 0 {
		n = runtime.NumCPU()
	}
	g.hashSlots = make(chan struct{}, n)
	return g
}

func (g *Guard) Run(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		for _, l := range []*Limiter{g.auth, g.passIP, g.passAccount, g.rpcUser, g.rpcIP, g.socketIP} {
			l.sweep()
		}
	}
}

func (g *Guard) trusted(ip net.IP) bool {
	for _, n := range g.proxies {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// ClientIP is the address of the client. The forwarding header is honoured
// only when the connection comes from a trusted proxy, and then the
// right-most address that is not itself a trusted proxy is used (the left
// part of the header is whatever the client sent).
func (g *Guard) ClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	remote := net.ParseIP(host)
	if remote == nil || g.header == "" || !g.trusted(remote) {
		return host
	}
	hops := strings.Split(strings.Join(r.Header.Values(g.header), ","), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		ip := net.ParseIP(strings.TrimSpace(hops[i]))
		if ip == nil {
			break
		}
		if !g.trusted(ip) || i == 0 {
			return ip.String()
		}
	}
	return host
}

func (g *Guard) reject(w http.ResponseWriter, kind string, wait time.Duration, v2 bool) {
	g.metrics.CounterAdd("security_rejected_total", map[string]string{"kind": kind}, 1)
	secs := int(math.Ceil(wait.Seconds()))
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	if v2 {
		writeV2Error(w, CodeResourceExhausted, "Too many requests, please slow down")
		return
	}
	writeNakamaError(w, CodeResourceExhausted, "Too many requests, please slow down")
}

// LimitAuth guards an authentication endpoint. passwordAccount is the
// normalised email for password logins, empty otherwise.
func (g *Guard) LimitAuth(w http.ResponseWriter, r *http.Request, passwordAccount string, v2 bool) bool {
	ip := g.ClientIP(r)
	if ok, wait := g.auth.Take(ip); !ok {
		g.reject(w, "auth_ip", wait, v2)
		return false
	}
	if passwordAccount != "" {
		if ok, wait := g.passIP.Take(ip); !ok {
			g.reject(w, "password_ip", wait, v2)
			return false
		}
		if ok, wait := g.passAccount.Take(strings.ToLower(strings.TrimSpace(passwordAccount))); !ok {
			g.reject(w, "password_account", wait, v2)
			return false
		}
	}
	return true
}

// AllowRPC applies the per-user (or, without a user, per-IP) RPC limit.
func (g *Guard) AllowRPC(userID, ip string) (bool, time.Duration) {
	if userID != "" {
		if ok, wait := g.rpcUser.Take(userID); !ok {
			g.metrics.CounterAdd("security_rejected_total", map[string]string{"kind": "rpc_user"}, 1)
			return false, wait
		}
	}
	if ok, wait := g.rpcIP.Take(ip); !ok {
		g.metrics.CounterAdd("security_rejected_total", map[string]string{"kind": "rpc_ip"}, 1)
		return false, wait
	}
	return true, 0
}

// OpenSocket admits a new socket for userID, or reports why not.
func (g *Guard) OpenSocket(ip, userID string, current int) (ok bool, reason string) {
	if g.cfg.MaxSessions > 0 && current >= g.cfg.MaxSessions {
		return false, "server_full"
	}
	if !g.socketIP.Allow(ip) {
		return false, "socket_ip"
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.cfg.SocketsPerUser > 0 && g.userSockets[userID] >= g.cfg.SocketsPerUser {
		return false, "sockets_per_user"
	}
	g.userSockets[userID]++
	return true, ""
}

func (g *Guard) CloseSocket(userID string) {
	g.mu.Lock()
	if g.userSockets[userID] <= 1 {
		delete(g.userSockets, userID)
	} else {
		g.userSockets[userID]--
	}
	g.mu.Unlock()
}

// HashSlot bounds concurrent password hashing; it waits up to 3 s for a slot.
func (g *Guard) HashSlot(ctx context.Context) (release func(), err error) {
	if g == nil {
		return func() {}, nil
	}
	t := time.NewTimer(3 * time.Second)
	defer t.Stop()
	select {
	case g.hashSlots <- struct{}{}:
		return func() { <-g.hashSlots }, nil
	case <-t.C:
		g.metrics.CounterAdd("security_rejected_total", map[string]string{"kind": "password_hash_busy"}, 1)
		return nil, statusErr(CodeUnavailable, "Server busy, please try again")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// socketRate is a per-socket message limiter (used only by the socket's
// read goroutine, so it needs no lock).
type socketRate struct {
	rate, burst, tokens float64
	last                time.Time
	drops               int
	windowStart         time.Time
}

func newSocketRate(perSecond int) *socketRate {
	return &socketRate{rate: float64(perSecond), burst: float64(perSecond * 2), tokens: float64(perSecond * 2), last: time.Now(), windowStart: time.Now()}
}

// allow reports whether to process a message and whether the socket is
// abusive enough to close.
func (s *socketRate) allow(abuseDrops int) (process, abusive bool) {
	if s.rate <= 0 {
		return true, false
	}
	now := time.Now()
	s.tokens = math.Min(s.burst, s.tokens+now.Sub(s.last).Seconds()*s.rate)
	s.last = now
	if s.tokens >= 1 {
		s.tokens--
		return true, false
	}
	if now.Sub(s.windowStart) > 10*time.Second {
		s.windowStart, s.drops = now, 0
	}
	s.drops++
	return false, abuseDrops > 0 && s.drops >= abuseDrops
}
