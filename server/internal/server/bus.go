package server

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/gob"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dev-sssss/tango_backend/server/internal/game"
)

// The cluster bus carries match traffic between nodes over persistent TCP
// connections. Each node dials every peer it talks to; responses come back
// on the same connection. One connection per direction keeps messages from
// one node to another in order.

type busKind uint8

const (
	kHello busKind = iota + 1
	kResponse
	kCreate
	kJoinAttempt
	kSignal
	kData
	kLeave
	kDeliver
	kKick
	kMoved
	kHandover
	kHandoverAbort
	kActivate
	kPing
)

type busMsg struct {
	Kind          busKind
	ReqID         uint64
	From          string
	Node          string
	MatchID       string
	Epoch         int64
	Module        string
	Presence      game.Presence
	Presences     []game.Presence
	Sessions      []string
	OpCode        int64
	Data          []byte
	HasData       bool
	Reliable      bool
	Migrating     bool
	ReceiveTimeMs int64
	Metadata      map[string]string
	Allow         bool
	Reason        string
	Label         string
	Err           string
	ErrKind       string
	MAC           string
	TS            int64
}

var (
	errNotOwner        = errors.New("not the match owner")
	errPeerUnavailable = errors.New("peer unavailable")
)

func (m *busMsg) error() error {
	switch m.ErrKind {
	case "":
		if m.Err != "" {
			return errors.New(m.Err)
		}
		return nil
	case "not_owner":
		return errNotOwner
	case "moved":
		return errMoved
	case "not_found":
		return errMatchNotFound
	}
	return errors.New(m.Err)
}

func errResponse(err error) *busMsg {
	r := &busMsg{Kind: kResponse, Err: err.Error()}
	switch {
	case errors.Is(err, errNotOwner):
		r.ErrKind = "not_owner"
	case errors.Is(err, errMoved):
		r.ErrKind = "moved"
	case errors.Is(err, errMatchNotFound):
		r.ErrKind = "not_found"
	}
	return r
}

type BusHandler func(from string, msg *busMsg) *busMsg

type Bus struct {
	node    string
	secret  []byte
	logger  *slog.Logger
	metrics *Metrics
	resolve func(node string) (addr string, ok bool)
	peers   func() []string
	handler BusHandler

	mu    sync.Mutex
	conns map[string]*peerConn
	ln    net.Listener
	reqID atomic.Uint64
	stop  chan struct{}
}

type peerConn struct {
	bus     *Bus
	node    string
	conn    net.Conn
	wmu     sync.Mutex
	enc     *gob.Encoder
	bw      *bufio.Writer
	pending sync.Map // reqID -> chan *busMsg
	closed  atomic.Bool
}

func NewBus(node, secret string, logger *slog.Logger, metrics *Metrics) *Bus {
	return &Bus{node: node, secret: []byte(secret), logger: logger, metrics: metrics, conns: map[string]*peerConn{}, stop: make(chan struct{})}
}

func (b *Bus) mac(from string, ts int64) string {
	h := hmac.New(sha256.New, b.secret)
	h.Write([]byte(from + "|" + strconv.FormatInt(ts, 10)))
	return hex.EncodeToString(h.Sum(nil))
}

func (b *Bus) Listen(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	b.ln = ln
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				select {
				case <-b.stop:
					return
				default:
				}
				time.Sleep(50 * time.Millisecond)
				continue
			}
			go b.serveIncoming(c)
		}
	}()
	return nil
}

func (b *Bus) Close() {
	close(b.stop)
	if b.ln != nil {
		_ = b.ln.Close()
	}
	b.mu.Lock()
	conns := make([]*peerConn, 0, len(b.conns))
	for _, c := range b.conns {
		conns = append(conns, c)
	}
	b.mu.Unlock()
	for _, c := range conns {
		c.close()
	}
}

func (b *Bus) serveIncoming(c net.Conn) {
	defer c.Close()
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
		_ = tc.SetKeepAlive(true)
	}
	dec := gob.NewDecoder(bufio.NewReader(c))
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	var hello busMsg
	if err := dec.Decode(&hello); err != nil || hello.Kind != kHello {
		return
	}
	if d := time.Since(time.Unix(hello.TS, 0)); d > time.Minute || d < -time.Minute || !hmac.Equal([]byte(hello.MAC), []byte(b.mac(hello.From, hello.TS))) {
		b.logger.Warn("rejected cluster bus connection", "remote", c.RemoteAddr().String(), "from", hello.From)
		return
	}
	_ = c.SetReadDeadline(time.Time{})
	bw := bufio.NewWriter(c)
	enc := gob.NewEncoder(bw)
	var wmu sync.Mutex
	from := hello.From
	for {
		var msg busMsg
		if err := dec.Decode(&msg); err != nil {
			return
		}
		m := msg
		if m.ReqID == 0 {
			// One-way messages are handled in arrival order.
			b.handler(from, &m)
			continue
		}
		// Requests may block (e.g. a join waiting for its match goroutine);
		// handle them concurrently so they never stall one-way traffic.
		go func() {
			resp := b.handler(from, &m)
			if resp == nil {
				resp = &busMsg{Kind: kResponse}
			}
			resp.Kind = kResponse
			resp.ReqID = m.ReqID
			wmu.Lock()
			_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
			err := enc.Encode(resp)
			if err == nil {
				err = bw.Flush()
			}
			wmu.Unlock()
			if err != nil {
				_ = c.Close()
			}
		}()
	}
}

func (b *Bus) peer(node string) (*peerConn, error) {
	b.mu.Lock()
	if c, ok := b.conns[node]; ok && !c.closed.Load() {
		b.mu.Unlock()
		return c, nil
	}
	b.mu.Unlock()
	addr, ok := b.resolve(node)
	if !ok {
		return nil, fmt.Errorf("%w: %s unknown", errPeerUnavailable, node)
	}
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errPeerUnavailable, err)
	}
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
		_ = tc.SetKeepAlive(true)
	}
	bw := bufio.NewWriter(conn)
	pc := &peerConn{bus: b, node: node, conn: conn, bw: bw, enc: gob.NewEncoder(bw)}
	ts := time.Now().Unix()
	if err := pc.write(&busMsg{Kind: kHello, From: b.node, TS: ts, MAC: b.mac(b.node, ts)}); err != nil {
		conn.Close()
		return nil, fmt.Errorf("%w: %v", errPeerUnavailable, err)
	}
	go pc.readResponses()
	b.mu.Lock()
	if old, ok := b.conns[node]; ok && !old.closed.Load() {
		b.mu.Unlock()
		pc.close()
		return old, nil
	}
	b.conns[node] = pc
	b.mu.Unlock()
	return pc, nil
}

func (p *peerConn) write(m *busMsg) error {
	p.wmu.Lock()
	defer p.wmu.Unlock()
	_ = p.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := p.enc.Encode(m); err != nil {
		return err
	}
	return p.bw.Flush()
}

func (p *peerConn) readResponses() {
	dec := gob.NewDecoder(bufio.NewReader(p.conn))
	for {
		var m busMsg
		if err := dec.Decode(&m); err != nil {
			p.close()
			return
		}
		if ch, ok := p.pending.LoadAndDelete(m.ReqID); ok {
			mm := m
			ch.(chan *busMsg) <- &mm
		}
	}
}

func (p *peerConn) close() {
	if !p.closed.CompareAndSwap(false, true) {
		return
	}
	_ = p.conn.Close()
	p.pending.Range(func(k, v any) bool {
		p.pending.Delete(k)
		close(v.(chan *busMsg))
		return true
	})
	p.bus.mu.Lock()
	if p.bus.conns[p.node] == p {
		delete(p.bus.conns, p.node)
	}
	p.bus.mu.Unlock()
}

// Send delivers a one-way message, preserving order per destination node.
func (b *Bus) Send(node string, m *busMsg) error {
	if node == b.node {
		go b.handler(b.node, m)
		return nil
	}
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		var p *peerConn
		if p, err = b.peer(node); err != nil {
			continue
		}
		if err = p.write(m); err != nil {
			p.close()
			continue
		}
		b.metrics.CounterAdd("bus_messages_sent_total", map[string]string{"kind": strconv.Itoa(int(m.Kind))}, 1)
		return nil
	}
	return fmt.Errorf("%w: %v", errPeerUnavailable, err)
}

// Request sends a message and waits for the response.
func (b *Bus) Request(ctx context.Context, node string, m *busMsg) (*busMsg, error) {
	if node == b.node {
		r := b.handler(b.node, m)
		if r == nil {
			r = &busMsg{Kind: kResponse}
		}
		return r, nil
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
	}
	p, err := b.peer(node)
	if err != nil {
		return nil, err
	}
	id := b.reqID.Add(1)
	ch := make(chan *busMsg, 1)
	p.pending.Store(id, ch)
	m.ReqID = id
	if err := p.write(m); err != nil {
		p.pending.Delete(id)
		p.close()
		return nil, fmt.Errorf("%w: %v", errPeerUnavailable, err)
	}
	select {
	case r, ok := <-ch:
		if !ok {
			return nil, errPeerUnavailable
		}
		return r, nil
	case <-ctx.Done():
		p.pending.Delete(id)
		return nil, ctx.Err()
	}
}

// Broadcast sends a one-way message to every known live peer.
func (b *Bus) Broadcast(m *busMsg) {
	for _, n := range b.peers() {
		if n != b.node {
			mm := *m
			_ = b.Send(n, &mm)
		}
	}
}

// DropPeer closes connections to a node (it died or left).
func (b *Bus) DropPeer(node string) {
	b.mu.Lock()
	c := b.conns[node]
	b.mu.Unlock()
	if c != nil {
		c.close()
	}
}
