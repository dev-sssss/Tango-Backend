package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/heroiclabs/nakama-common/api"
	"github.com/heroiclabs/nakama-common/rtapi"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/dev-sssss/tango_backend/server/internal/game"
)

// The realtime socket speaks Nakama's rtapi protocol: protobuf frames for
// format=protobuf (Dart/Unity SDKs) and protojson text frames otherwise
// (JavaScript SDK). Only the parts the game uses are implemented: match
// join/leave/data, RPC and ping. Everything else gets an error envelope.

var (
	jsonMarshal   = protojson.MarshalOptions{UseProtoNames: true, UseEnumNumbers: true, EmitUnpopulated: false}
	jsonUnmarshal = protojson.UnmarshalOptions{DiscardUnknown: true}
)

func decodeBusEnvelope(b []byte) *rtapi.Envelope {
	env := &rtapi.Envelope{}
	if err := proto.Unmarshal(b, env); err != nil {
		return nil
	}
	return env
}

// ---------------------------------------------------------------- registry

// SessionRegistry holds the client sockets connected to this gateway node.
type SessionRegistry struct {
	metrics *Metrics
	mu      sync.RWMutex
	byID    map[string]*Session
}

func NewSessionRegistry(metrics *Metrics) *SessionRegistry {
	return &SessionRegistry{metrics: metrics, byID: map[string]*Session{}}
}

func (r *SessionRegistry) add(s *Session) {
	r.mu.Lock()
	r.byID[s.id] = s
	n := len(r.byID)
	r.mu.Unlock()
	r.metrics.GaugeSet("sessions", nil, float64(n))
}

func (r *SessionRegistry) remove(s *Session) {
	r.mu.Lock()
	if r.byID[s.id] == s {
		delete(r.byID, s.id)
	}
	n := len(r.byID)
	r.mu.Unlock()
	r.metrics.GaugeSet("sessions", nil, float64(n))
}

func (r *SessionRegistry) Get(id string) *Session {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.byID[id]
}

func (r *SessionRegistry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.byID)
}

// presenceSnapshot maps each connected user to whether they are in a match.
func (r *SessionRegistry) presenceSnapshot() map[string]bool {
	out := map[string]bool{}
	for _, s := range r.all() {
		s.mu.Lock()
		inMatch := len(s.matches) > 0
		s.mu.Unlock()
		out[s.userID] = out[s.userID] || inMatch
	}
	return out
}

func (r *SessionRegistry) all() []*Session {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Session, 0, len(r.byID))
	for _, s := range r.byID {
		out = append(out, s)
	}
	return out
}

// SendEnvelope delivers one envelope to several local sessions, encoding it
// at most once per wire format.
func (r *SessionRegistry) SendEnvelope(sids []string, env *rtapi.Envelope) {
	var pb, js []byte
	for _, sid := range sids {
		s := r.Get(sid)
		if s == nil {
			continue
		}
		if s.protobuf {
			if pb == nil {
				pb, _ = proto.Marshal(env)
			}
			s.enqueue(pb)
		} else {
			if js == nil {
				js, _ = jsonMarshal.Marshal(env)
			}
			s.enqueue(js)
		}
	}
}

// DropMembership forgets that sessions are in a match (the match ended or
// kicked them) without sending a leave back to the match.
func (r *SessionRegistry) DropMembership(sids []string, matchID string) {
	u, ok := splitMatchID(matchID)
	if !ok {
		return
	}
	for _, sid := range sids {
		if s := r.Get(sid); s != nil {
			s.mu.Lock()
			delete(s.matches, u)
			s.mu.Unlock()
		}
	}
}

// CloseAll closes every socket. Matches keep the seats of these sessions
// for the presence grace period so clients can reconnect elsewhere.
func (r *SessionRegistry) CloseAll(code int, reason string) {
	for _, s := range r.all() {
		s.migrating.Store(true)
		s.closeWith(code, reason)
	}
}

// ---------------------------------------------------------------- session

type Session struct {
	srv       *Server
	id        string
	userID    string
	username  string
	vars      map[string]string
	clientIP  string
	expiry    int64
	protobuf  bool
	conn      *websocket.Conn
	out       chan []byte
	done      chan struct{}
	closeOnce sync.Once
	migrating atomic.Bool

	mu      sync.Mutex
	matches map[string]string // match uuid -> match id as the client knows it
}

func (s *Session) presence() game.Presence {
	return game.Presence{UserID: s.userID, SessionID: s.id, Username: s.username, Node: s.srv.cfg.Name}
}

func (s *Session) enqueue(b []byte) {
	select {
	case <-s.done:
	case s.out <- b:
	default:
		// A client that cannot keep up is disconnected (as Nakama does).
		s.srv.metrics.CounterAdd("socket_outgoing_overflow_total", nil, 1)
		go s.closeWith(websocket.ClosePolicyViolation, "outgoing queue full")
	}
}

func (s *Session) send(env *rtapi.Envelope) {
	var b []byte
	var err error
	if s.protobuf {
		b, err = proto.Marshal(env)
	} else {
		b, err = jsonMarshal.Marshal(env)
	}
	if err == nil {
		s.enqueue(b)
	}
}

func (s *Session) sendError(cid string, code rtapi.Error_Code, msg string) {
	s.send(&rtapi.Envelope{Cid: cid, Message: &rtapi.Envelope_Error{Error: &rtapi.Error{Code: int32(code), Message: msg}}})
}

func (s *Session) closeWith(code int, reason string) {
	s.closeOnce.Do(func() {
		close(s.done)
		deadline := time.Now().Add(time.Second)
		_ = s.conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, reason), deadline)
		_ = s.conn.Close()
	})
}

// ---------------------------------------------------------------- handler

func (srv *Server) handleSocket(w http.ResponseWriter, r *http.Request) {
	if srv.cluster.Draining() {
		http.Error(w, "server is restarting", http.StatusServiceUnavailable)
		return
	}
	q := r.URL.Query()
	claims, err := srv.tokens.Verify(q.Get("token"))
	if err != nil {
		http.Error(w, "Auth token invalid", http.StatusUnauthorized)
		return
	}
	srv.analytics.Active(claims.UserID, clientTag(r))
	format := strings.ToLower(q.Get("format"))
	if format != "" && format != "json" && format != "protobuf" {
		http.Error(w, "Invalid format parameter", http.StatusBadRequest)
		return
	}
	ip := srv.clientIP(r)
	if ok, reason := srv.guard.OpenSocket(ip, claims.UserID, srv.sessions.Count()); !ok {
		srv.metrics.CounterAdd("security_rejected_total", map[string]string{"kind": reason}, 1)
		if reason == "server_full" {
			// Load balancers retry another gateway on 503.
			http.Error(w, "server is full", http.StatusServiceUnavailable)
		} else {
			w.Header().Set("Retry-After", "5")
			http.Error(w, "too many connections", http.StatusTooManyRequests)
		}
		return
	}
	up := websocket.Upgrader{ReadBufferSize: 1024, WriteBufferSize: 1024, CheckOrigin: func(*http.Request) bool { return true }}
	conn, err := up.Upgrade(w, r, nil)
	if err != nil {
		srv.guard.CloseSocket(claims.UserID)
		return
	}
	qs := srv.cfg.Socket.OutgoingQueueSize
	if qs < 16 {
		qs = 16
	}
	s := &Session{
		srv: srv, id: newUUID(), userID: claims.UserID, username: claims.Username, vars: claims.Vars, clientIP: ip,
		expiry: claims.ExpiresAt, protobuf: format == "protobuf", conn: conn, out: make(chan []byte, qs), done: make(chan struct{}),
		matches: map[string]string{},
	}
	srv.sessions.add(s)
	srv.metrics.CounterAdd("socket_connections_total", nil, 1)
	go s.writeLoop()
	s.readLoop()
}

func (s *Session) writeLoop() {
	cfg := s.srv.cfg.Socket
	ping := time.NewTicker(time.Duration(cfg.PingPeriodMs) * time.Millisecond)
	defer ping.Stop()
	wait := time.Duration(cfg.WriteWaitMs) * time.Millisecond
	mt := websocket.TextMessage
	if s.protobuf {
		mt = websocket.BinaryMessage
	}
	for {
		select {
		case <-s.done:
			return
		case b := <-s.out:
			_ = s.conn.SetWriteDeadline(time.Now().Add(wait))
			if err := s.conn.WriteMessage(mt, b); err != nil {
				s.closeWith(websocket.CloseAbnormalClosure, "write failed")
				return
			}
		case <-ping.C:
			if err := s.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(wait)); err != nil {
				s.closeWith(websocket.CloseAbnormalClosure, "ping failed")
				return
			}
		}
	}
}

func (s *Session) readLoop() {
	cfg := s.srv.cfg.Socket
	pongWait := time.Duration(cfg.PongWaitMs) * time.Millisecond
	s.conn.SetReadLimit(cfg.MaxRequestSizeBytes)
	_ = s.conn.SetReadDeadline(time.Now().Add(pongWait))
	s.conn.SetPongHandler(func(string) error { return s.conn.SetReadDeadline(time.Now().Add(pongWait)) })
	defer s.cleanup()
	limit := newSocketRate(s.srv.cfg.Security.SocketMessagesPerSecond)
	for {
		mt, data, err := s.conn.ReadMessage()
		if err != nil {
			return
		}
		if process, abusive := limit.allow(s.srv.cfg.Security.SocketAbuseDrops); !process {
			s.srv.metrics.CounterAdd("security_rejected_total", map[string]string{"kind": "socket_messages"}, 1)
			if abusive {
				s.srv.logger.Warn("closing flooding socket", "user_id", s.userID, "ip", s.clientIP)
				s.closeWith(websocket.ClosePolicyViolation, "message rate exceeded")
				return
			}
			continue
		}
		_ = s.conn.SetReadDeadline(time.Now().Add(pongWait))
		env := &rtapi.Envelope{}
		if mt == websocket.BinaryMessage {
			err = proto.Unmarshal(data, env)
		} else {
			err = jsonUnmarshal.Unmarshal(data, env)
		}
		if err != nil {
			s.sendError("", rtapi.Error_UNRECOGNIZED_PAYLOAD, "Could not unmarshal message")
			continue
		}
		s.srv.metrics.CounterAdd("socket_messages_total", nil, 1)
		s.handle(env)
	}
}

// cleanup runs once the socket is gone: it leaves every match the session
// was in. A gateway drain marks the leave as "migrating" so the match keeps
// the seat while the client reconnects through another gateway.
func (s *Session) cleanup() {
	s.closeWith(websocket.CloseNormalClosure, "")
	s.srv.sessions.remove(s)
	s.srv.guard.CloseSocket(s.userID)
	s.mu.Lock()
	ids := make([]string, 0, len(s.matches))
	for _, id := range s.matches {
		ids = append(ids, id)
	}
	s.matches = map[string]string{}
	s.mu.Unlock()
	migrating := s.migrating.Load()
	for _, id := range ids {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		s.srv.host.Leave(ctx, id, []game.Presence{s.presence()}, migrating)
		cancel()
	}
}

func (s *Session) handle(env *rtapi.Envelope) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	switch m := env.Message.(type) {
	case *rtapi.Envelope_Ping:
		s.send(&rtapi.Envelope{Cid: env.Cid, Message: &rtapi.Envelope_Pong{Pong: &rtapi.Pong{}}})
	case *rtapi.Envelope_MatchJoin:
		s.matchJoin(ctx, env.Cid, m.MatchJoin)
	case *rtapi.Envelope_MatchLeave:
		id := m.MatchLeave.GetMatchId()
		if u, ok := splitMatchID(id); ok {
			s.mu.Lock()
			_, member := s.matches[u]
			delete(s.matches, u)
			s.mu.Unlock()
			if member {
				s.srv.host.Leave(ctx, id, []game.Presence{s.presence()}, false)
			}
		}
		s.send(&rtapi.Envelope{Cid: env.Cid})
	case *rtapi.Envelope_MatchDataSend:
		d := m.MatchDataSend
		u, ok := splitMatchID(d.GetMatchId())
		if !ok {
			return
		}
		s.mu.Lock()
		id, member := s.matches[u]
		s.mu.Unlock()
		if !member {
			return
		}
		s.srv.host.SendData(ctx, id, game.MatchMessage{Sender: s.presence(), OpCode: d.OpCode, Data: d.Data, HasData: d.Data != nil,
			Reliable: d.Reliable, ReceiveTimeMs: time.Now().UnixMilli()})
	case *rtapi.Envelope_Rpc:
		in := m.Rpc
		caller := &rpcCaller{userID: s.userID, username: s.username, sessionID: s.id, vars: s.vars, clientIP: s.clientIP, viaSocket: true}
		out, err := s.srv.callRPC(ctx, strings.ToLower(in.GetId()), caller, in.GetPayload())
		if err != nil {
			code := rtapi.Error_RUNTIME_FUNCTION_EXCEPTION
			if errors.Is(err, errRPCNotFound) {
				code = rtapi.Error_RUNTIME_FUNCTION_NOT_FOUND
			}
			s.sendError(env.Cid, code, messageOfRPC(err))
			return
		}
		s.send(&rtapi.Envelope{Cid: env.Cid, Message: &rtapi.Envelope_Rpc{Rpc: &api.Rpc{Id: in.GetId(), Payload: out}}})
	case nil:
		s.sendError(env.Cid, rtapi.Error_MISSING_PAYLOAD, "Missing message.")
	default:
		s.sendError(env.Cid, rtapi.Error_UNRECOGNIZED_PAYLOAD, "Unrecognized message.")
	}
}

func (s *Session) matchJoin(ctx context.Context, cid string, j *rtapi.MatchJoin) {
	id := j.GetMatchId()
	if id == "" {
		s.sendError(cid, rtapi.Error_BAD_INPUT, "Match join requires a match ID; matchmaker tokens are not supported")
		return
	}
	u, ok := splitMatchID(id)
	if !ok {
		s.sendError(cid, rtapi.Error_BAD_INPUT, "Invalid match ID")
		return
	}
	self := s.presence()
	res, err := s.srv.host.JoinAttempt(ctx, id, self, j.Metadata)
	if err != nil {
		if errors.Is(err, errMatchNotFound) || errors.Is(err, errNotOwner) || errors.Is(err, errMoved) || errors.Is(err, errPeerUnavailable) {
			s.sendError(cid, rtapi.Error_MATCH_NOT_FOUND, "Match not found")
		} else {
			s.sendError(cid, rtapi.Error_RUNTIME_EXCEPTION, "Match join failed")
		}
		return
	}
	if res == nil || !res.Allow {
		reason := "Match join rejected"
		if res != nil && res.Reason != "" {
			reason = res.Reason
		}
		s.sendError(cid, rtapi.Error_MATCH_JOIN_REJECTED, reason)
		return
	}
	select {
	case <-s.done:
		// The socket closed while joining: release the seat right away.
		s.srv.host.Leave(context.Background(), id, []game.Presence{self}, s.migrating.Load())
		return
	default:
	}
	s.mu.Lock()
	s.matches[u] = id
	s.mu.Unlock()
	ps := make([]*rtapi.UserPresence, 0, len(res.Presences))
	for _, p := range res.Presences {
		if p.SessionID != self.SessionID {
			ps = append(ps, &rtapi.UserPresence{UserId: p.UserID, SessionId: p.SessionID, Username: p.Username})
		}
	}
	s.send(&rtapi.Envelope{Cid: cid, Message: &rtapi.Envelope_Match{Match: &rtapi.Match{
		MatchId: id, Authoritative: true, Label: wrapperspb.String(res.Label), Size: int32(len(ps) + 1), Presences: ps,
		Self: &rtapi.UserPresence{UserId: self.UserID, SessionId: self.SessionID, Username: self.Username},
	}}})
}
