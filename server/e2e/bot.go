// Package e2e runs real tango-server processes (single node and cluster)
// against PostgreSQL and plays Tango Cards with bot clients that speak the
// same protocol as the app (Nakama HTTP API + protobuf realtime socket).
package e2e

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/heroiclabs/nakama-common/rtapi"
	"google.golang.org/protobuf/proto"
)

type Bot struct {
	Name      string
	ServerKey string
	DeviceID  string

	mu       sync.Mutex
	gateways []string // base URLs, tried in order on reconnect
	gwIndex  int
	token    string
	UserID   string
	conn     *websocket.Conn
	cid      atomic.Int64
	waiters  map[string]chan *rtapi.Envelope
	MatchID  string
	state    map[string]any
	last     map[string]any
	result   map[string]any
	lastRev  int64
	actedRev int64
	actedAt  time.Time

	// Observations.
	MaxRevision  atomic.Int64
	Regressions  atomic.Int64 // times the state went back
	MaxRollback  atomic.Int64 // deepest rollback in revisions
	lastMsgAt    atomic.Int64 // unix ms of the last match message
	maxGap       atomic.Int64 // longest silence between match messages (ms)
	Messages     atomic.Int64
	Reconnects   atomic.Int64
	LastSnapshot atomic.Int64 // unix ms
	Actions      atomic.Int64
	CloseCodes   sync.Map // code -> count
	Errors       []string
	stop         chan struct{}
	stopped      atomic.Bool
}

func NewBot(name, serverKey string, gateways []string) *Bot {
	return &Bot{Name: name, ServerKey: serverKey, DeviceID: "e2e-device-" + name + "-" + strconv.FormatInt(time.Now().UnixNano(), 36),
		gateways: gateways, waiters: map[string]chan *rtapi.Envelope{}, stop: make(chan struct{})}
}

func (b *Bot) gateway() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.gateways[b.gwIndex%len(b.gateways)]
}

func (b *Bot) rotateGateway() {
	b.mu.Lock()
	b.gwIndex++
	b.mu.Unlock()
}

func (b *Bot) logf(format string, args ...any) {
	b.mu.Lock()
	b.Errors = append(b.Errors, time.Now().Format("15:04:05.000")+" "+fmt.Sprintf(format, args...))
	b.mu.Unlock()
}

func (b *Bot) post(path string, auth string, body []byte) ([]byte, int, error) {
	var lastErr error
	for i := 0; i < len(b.gateways)*2; i++ {
		req, _ := http.NewRequest(http.MethodPost, b.gateway()+path, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", auth)
		resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
		if err != nil {
			lastErr = err
			b.rotateGateway()
			time.Sleep(200 * time.Millisecond)
			continue
		}
		out, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusServiceUnavailable {
			lastErr = fmt.Errorf("503 %s", out)
			b.rotateGateway()
			continue
		}
		return out, resp.StatusCode, nil
	}
	return nil, 0, lastErr
}

func (b *Bot) Authenticate() error {
	basic := "Basic " + base64.StdEncoding.EncodeToString([]byte(b.ServerKey+":"))
	body, _ := json.Marshal(map[string]any{"id": b.DeviceID})
	out, code, err := b.post("/v2/account/authenticate/device?create=true&username="+url.QueryEscape(b.Name), basic, body)
	if err != nil {
		return err
	}
	if code != 200 {
		return fmt.Errorf("auth %d: %s", code, out)
	}
	var s struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(out, &s); err != nil {
		return err
	}
	b.token = s.Token
	parts := bytes.Split([]byte(s.Token), []byte("."))
	claims, _ := base64.RawURLEncoding.DecodeString(string(parts[1]))
	var c struct {
		UID string `json:"uid"`
	}
	_ = json.Unmarshal(claims, &c)
	b.UserID = c.UID
	return nil
}

func (b *Bot) RPC(id string, payload any) (map[string]any, error) {
	body, _ := json.Marshal(payload)
	out, code, err := b.post("/v2/rpc/"+id+"?unwrap=true", "Bearer "+b.token, body)
	if err != nil {
		return nil, err
	}
	if code != 200 {
		return nil, fmt.Errorf("rpc %s: %d %s", id, code, out)
	}
	var m map[string]any
	_ = json.Unmarshal(out, &m)
	return m, nil
}

// ---------------------------------------------------------------- socket

func (b *Bot) connect() error {
	u, _ := url.Parse(b.gateway())
	u.Scheme = "ws"
	u.Path = "/ws"
	u.RawQuery = url.Values{"token": {b.token}, "format": {"protobuf"}}.Encode()
	conn, _, err := (&websocket.Dialer{HandshakeTimeout: 3 * time.Second}).Dial(u.String(), nil)
	if err != nil {
		return err
	}
	b.mu.Lock()
	b.conn = conn
	b.mu.Unlock()
	go b.readLoop(conn)
	return nil
}

func (b *Bot) send(env *rtapi.Envelope) error {
	raw, _ := proto.Marshal(env)
	b.mu.Lock()
	conn := b.conn
	b.mu.Unlock()
	if conn == nil {
		return errors.New("not connected")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	_ = conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
	return conn.WriteMessage(websocket.BinaryMessage, raw)
}

func (b *Bot) request(env *rtapi.Envelope) (*rtapi.Envelope, error) {
	cid := strconv.FormatInt(b.cid.Add(1), 10)
	env.Cid = cid
	ch := make(chan *rtapi.Envelope, 1)
	b.mu.Lock()
	b.waiters[cid] = ch
	b.mu.Unlock()
	if err := b.send(env); err != nil {
		return nil, err
	}
	select {
	case r := <-ch:
		if r == nil {
			return nil, errors.New("socket closed")
		}
		if e := r.GetError(); e != nil {
			return nil, fmt.Errorf("server error %d: %s", e.Code, e.Message)
		}
		return r, nil
	case <-time.After(8 * time.Second):
		return nil, errors.New("request timeout")
	}
}

func (b *Bot) readLoop(conn *websocket.Conn) {
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			code := -1
			var ce *websocket.CloseError
			if errors.As(err, &ce) {
				code = ce.Code
			}
			v, _ := b.CloseCodes.LoadOrStore(code, new(atomic.Int64))
			v.(*atomic.Int64).Add(1)
			b.mu.Lock()
			if b.conn == conn {
				b.conn = nil
			}
			for k, ch := range b.waiters {
				close(ch)
				delete(b.waiters, k)
			}
			b.mu.Unlock()
			return
		}
		env := &rtapi.Envelope{}
		if proto.Unmarshal(data, env) != nil {
			continue
		}
		if env.Cid != "" {
			b.mu.Lock()
			ch := b.waiters[env.Cid]
			delete(b.waiters, env.Cid)
			b.mu.Unlock()
			if ch != nil {
				ch <- env
			}
			continue
		}
		if md := env.GetMatchData(); md != nil {
			b.Messages.Add(1)
			now := time.Now().UnixMilli()
			if prev := b.lastMsgAt.Swap(now); prev > 0 && now-prev > b.maxGap.Load() {
				b.maxGap.Store(now - prev)
			}
			var m map[string]any
			if json.Unmarshal(md.Data, &m) != nil {
				continue
			}
			if m["type"] == "error" {
				b.logf("server error: %v", m["code"])
			}
			if m["type"] == "game_over" {
				b.mu.Lock()
				b.result = m
				b.mu.Unlock()
			}
			if st, ok := m["state"].(map[string]any); ok {
				b.onState(st)
			}
		}
	}
}

func (b *Bot) onState(st map[string]any) {
	rev := int64(num(st["rev"]))
	if prev := b.MaxRevision.Load(); rev < prev {
		if b.lastRev >= prev {
			b.Regressions.Add(1)
			b.logf("revision went back %d -> %d", prev, rev)
		}
		if d := prev - rev; d > b.MaxRollback.Load() {
			b.MaxRollback.Store(d)
		}
	} else {
		b.MaxRevision.Store(rev)
	}
	b.lastRev = rev
	b.LastSnapshot.Store(time.Now().UnixMilli())
	b.mu.Lock()
	b.state = st
	b.last = st
	b.mu.Unlock()
}

func num(v any) float64 {
	f, _ := v.(float64)
	return f
}

func (b *Bot) JoinMatch(id string) error {
	r, err := b.request(&rtapi.Envelope{Message: &rtapi.Envelope_MatchJoin{MatchJoin: &rtapi.MatchJoin{Id: &rtapi.MatchJoin_MatchId{MatchId: id}}}})
	if err != nil {
		return err
	}
	if r.GetMatch() == nil {
		return errors.New("join: no match in response")
	}
	b.MatchID = id
	return nil
}

func (b *Bot) sendOp(op int64, m map[string]any) {
	raw, _ := json.Marshal(m)
	_ = b.send(&rtapi.Envelope{Message: &rtapi.Envelope_MatchDataSend{MatchDataSend: &rtapi.MatchDataSend{MatchId: b.MatchID, OpCode: op, Data: raw, Reliable: true}}})
}

// Play connects, joins and plays until Stop. It reconnects through the next
// gateway whenever the socket drops, like the app does.
func (b *Bot) Play(matchID string) error {
	if err := b.connect(); err != nil {
		return err
	}
	if err := b.JoinMatch(matchID); err != nil {
		return err
	}
	go b.loop()
	return nil
}

func (b *Bot) Stop() {
	if b.stopped.CompareAndSwap(false, true) {
		close(b.stop)
		b.mu.Lock()
		if b.conn != nil {
			_ = b.conn.Close()
		}
		b.mu.Unlock()
	}
}

func (b *Bot) connected() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.conn != nil
}

func (b *Bot) loop() {
	t := time.NewTicker(200 * time.Millisecond)
	defer t.Stop()
	n := 0
	for {
		select {
		case <-b.stop:
			return
		case <-t.C:
		}
		if !b.connected() {
			b.reconnect()
			continue
		}
		n++
		if n%3 == 0 {
			b.sendOp(9, map[string]any{}) // REQUEST_SNAPSHOT
		}
		b.act()
	}
}

func (b *Bot) reconnect() {
	for attempt := 0; ; attempt++ {
		select {
		case <-b.stop:
			return
		default:
		}
		b.rotateGateway()
		if err := b.connect(); err != nil {
			time.Sleep(150 * time.Millisecond)
			continue
		}
		if err := b.JoinMatch(b.MatchID); err != nil {
			b.logf("rejoin via %s failed: %v", b.gateway(), err)
			b.mu.Lock()
			if b.conn != nil {
				_ = b.conn.Close()
			}
			b.mu.Unlock()
			time.Sleep(200 * time.Millisecond)
			if attempt > 100 {
				return
			}
			continue
		}
		b.Reconnects.Add(1)
		return
	}
}

// act plays like a simple human from the state the server sent: first
// playable card (pressing TANGO! when going to one card), otherwise draw.
func (b *Bot) act() {
	b.mu.Lock()
	st := b.state
	b.mu.Unlock()
	if st == nil || st["status"] != "PLAYING" {
		return
	}
	g, _ := st["game"].(map[string]any)
	if g == nil || int(num(g["phase_seat"])) != int(num(st["you"])) {
		return
	}
	rev := int64(num(st["rev"]))
	if rev == b.actedRev && time.Since(b.actedAt) < 3*time.Second {
		return // already answered this state (retry if nothing changed for 3 s)
	}
	b.actedRev, b.actedAt = rev, time.Now()
	color := b.bestColor(st)
	switch g["phase"] {
	case "color":
		b.sendOp(11, map[string]any{"color": color})
	case "keep":
		b.sendOp(3, map[string]any{"play": true, "color": color})
	case "turn":
		ids, _ := g["playable"].([]any)
		if len(ids) == 0 {
			b.sendOp(2, map[string]any{})
		} else {
			b.sendOp(1, map[string]any{"card_id": ids[0], "color": color, "tango": true})
		}
	}
	b.Actions.Add(1)
}

func (b *Bot) bestColor(st map[string]any) string {
	you := int(num(st["you"]))
	count := map[string]int{}
	players, _ := st["players"].([]any)
	for _, pv := range players {
		p, _ := pv.(map[string]any)
		if int(num(p["seat"])) != you {
			continue
		}
		hand, _ := p["hand"].([]any)
		for _, cv := range hand {
			c, _ := cv.(map[string]any)
			if col, _ := c["color"].(string); col != "wild" {
				count[col]++
			}
		}
	}
	best, n := "red", -1
	for _, col := range []string{"red", "yellow", "green", "blue"} {
		if count[col] > n {
			best, n = col, count[col]
		}
	}
	return best
}

func (b *Bot) Last() map[string]any {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.last
}

func (b *Bot) Result() map[string]any {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.result
}

func (b *Bot) Log() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.Errors...)
}

func (b *Bot) ResetGap() {
	b.maxGap.Store(0)
	b.lastMsgAt.Store(time.Now().UnixMilli())
}

func (b *Bot) MaxGap() time.Duration { return time.Duration(b.maxGap.Load()) * time.Millisecond }
