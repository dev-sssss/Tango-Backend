package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/heroiclabs/nakama-common/api"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/dev-sssss/tango_backend/server/internal/game/rpc"
)

// The client API has two path families:
//
//   - API v1: Nakama's own paths (/v2/account/..., /v2/rpc/..., /ws). Every
//     app build in the field uses these; they stay byte-compatible with
//     Nakama 3.x so old builds never notice the server changed.
//   - API v2 and later: /api/vN/... with plain JSON contracts.
//
// See apiversions.go for the deprecation and removal rules.

func newUUID() string { return uuid.Must(uuid.NewV4()).String() }

func (srv *Server) clientIP(r *http.Request) string { return srv.guard.ClientIP(r) }

// writeNakamaError writes the error body grpc-gateway produced for Nakama.
func writeNakamaError(w http.ResponseWriter, code Code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code.HTTPStatus())
	b, _ := json.Marshal(map[string]any{"code": int(code), "error": msg, "message": msg})
	_, _ = w.Write(b)
}

func writeErr(w http.ResponseWriter, err error) {
	writeNakamaError(w, codeOf(err), messageOf(err))
}

func writeProto(w http.ResponseWriter, m proto.Message) {
	b, err := jsonMarshal.Marshal(m)
	if err != nil {
		writeNakamaError(w, CodeInternal, "Internal server error")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(b)
}

// maxServerRPCBodyBytes bounds RPC bodies from callers that proved the
// server HTTP key (the asset publisher posts a release manifest of several
// hundred KB). Client sessions keep the small socket limit.
const maxServerRPCBodyBytes = 8 << 20

func (srv *Server) readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	return srv.readBodyLimit(w, r, srv.cfg.Socket.MaxRequestSizeBytes)
}

func (srv *Server) readBodyLimit(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, error) {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	b, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, statusErr(CodeInvalidArgument, "Request body too large or unreadable.")
	}
	return b, nil
}

func (srv *Server) checkServerKey(r *http.Request) bool {
	key, ok := parseBasic(r.Header.Get("Authorization"))
	return ok && key == srv.cfg.Socket.ServerKey
}

func (srv *Server) sessionFromRequest(r *http.Request) (*SessionClaims, error) {
	tok, ok := parseBearer(r.Header.Get("Authorization"))
	if !ok {
		return nil, statusErr(CodeUnauthenticated, "Auth token required")
	}
	c, err := srv.tokens.Verify(tok)
	if err != nil {
		return nil, statusErr(CodeUnauthenticated, "Auth token invalid")
	}
	srv.analytics.Active(c.UserID, clientTag(r))
	srv.friends.Touch(c.UserID)
	return c, nil
}

// ---------------------------------------------------------------- routes

func (srv *Server) clientMux() http.Handler {
	mux := http.NewServeMux()
	// API v1 (Nakama-compatible).
	mux.HandleFunc("GET /healthcheck", srv.handleHealth)
	mux.HandleFunc("POST /v2/account/authenticate/device", srv.v1(srv.handleAuthDevice))
	mux.HandleFunc("POST /v2/account/authenticate/email", srv.v1(srv.handleAuthEmail))
	mux.HandleFunc("POST /v2/account/authenticate/google", srv.v1(srv.handleAuthGoogle))
	mux.HandleFunc("POST /v2/account/session/refresh", srv.v1(srv.handleSessionRefresh))
	mux.HandleFunc("POST /v2/account/link/google", srv.v1(srv.handleLinkGoogle))
	mux.HandleFunc("POST /v2/session/logout", srv.v1(srv.handleLogout))
	mux.HandleFunc("GET /v2/account", srv.v1(srv.handleGetAccount))
	mux.HandleFunc("PUT /v2/account", srv.v1(srv.handleUpdateAccount))
	mux.HandleFunc("DELETE /v2/account", srv.v1(srv.handleDeleteAccount))
	mux.HandleFunc("GET /v2/rpc/{id}", srv.v1(srv.handleRPCv1))
	mux.HandleFunc("POST /v2/rpc/{id}", srv.v1(srv.handleRPCv1))
	mux.HandleFunc("GET /ws", srv.v1(srv.handleSocket))
	// API v2.
	srv.registerV2(mux)
	// Unknown paths answer like Nakama does.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeNakamaError(w, CodeNotFound, "Not Found")
	})
	return srv.recoverer(mux)
}

func (srv *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				srv.logger.Error("http handler panic", "path", r.URL.Path, "error", fmt.Sprint(rec), "stack", string(debug.Stack()))
				writeNakamaError(w, CodeInternal, "Internal server error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// v1 wraps Nakama-compatible handlers: usage tracking and version headers.
func (srv *Server) v1(fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		srv.versions.annotate(w, "v1")
		srv.usage.Record("v1", clientTag(r))
		fn(w, r)
	}
}

func (srv *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if srv.cluster.Draining() {
		// Load balancers stop sending new traffic to a draining node.
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"draining":true}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte("{}"))
}

// ---------------------------------------------------------------- v1 auth

func (srv *Server) issueSession(w http.ResponseWriter, userID, username string, vars map[string]string, created bool) {
	tok, refresh, _ := srv.tokens.Issue(userID, username, vars)
	writeProto(w, &api.Session{Created: created, Token: tok, RefreshToken: refresh})
}

func boolQuery(r *http.Request, key string) bool {
	v := strings.ToLower(r.URL.Query().Get(key))
	return v == "true" || v == "1"
}

func (srv *Server) authPrelude(w http.ResponseWriter, r *http.Request, into proto.Message) bool {
	if !srv.checkServerKey(r) {
		writeNakamaError(w, CodeUnauthenticated, "Server key invalid")
		return false
	}
	b, err := srv.readBody(w, r)
	if err != nil {
		writeErr(w, err)
		return false
	}
	if err := jsonUnmarshal.Unmarshal(b, into); err != nil {
		writeNakamaError(w, CodeInvalidArgument, "Malformed request body.")
		return false
	}
	return true
}

func (srv *Server) handleAuthDevice(w http.ResponseWriter, r *http.Request) {
	var in api.AccountDevice
	if !srv.authPrelude(w, r, &in) || !srv.guard.LimitAuth(w, r, "", false) {
		return
	}
	uid, uname, created, err := AuthenticateDevice(r.Context(), srv.db, in.Id, r.URL.Query().Get("username"), boolQuery(r, "create"))
	if err != nil {
		writeErr(w, err)
		return
	}
	srv.trackSignin(r, "device", uid, created)
	srv.issueSession(w, uid, uname, in.Vars, created)
}

func (srv *Server) handleAuthEmail(w http.ResponseWriter, r *http.Request) {
	var in api.AccountEmail
	if !srv.authPrelude(w, r, &in) || !srv.guard.LimitAuth(w, r, in.Email, false) {
		return
	}
	release, err := srv.guard.HashSlot(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	uid, uname, created, err := AuthenticateEmail(r.Context(), srv.db, in.Email, in.Password, r.URL.Query().Get("username"), boolQuery(r, "create"))
	release()
	if err != nil {
		writeErr(w, err)
		return
	}
	srv.trackSignin(r, "email", uid, created)
	srv.issueSession(w, uid, uname, in.Vars, created)
}

func (srv *Server) handleAuthGoogle(w http.ResponseWriter, r *http.Request) {
	var in api.AccountGoogle
	if !srv.authPrelude(w, r, &in) || !srv.guard.LimitAuth(w, r, "", false) {
		return
	}
	if in.Token == "" {
		writeNakamaError(w, CodeInvalidArgument, "Google access token is required.")
		return
	}
	profile, err := srv.google.Verify(r.Context(), in.Token)
	if err != nil {
		writeNakamaError(w, CodeUnauthenticated, "Could not authenticate Google profile.")
		return
	}
	uid, uname, created, err := AuthenticateGoogle(r.Context(), srv.db, profile, r.URL.Query().Get("username"), boolQuery(r, "create"))
	if err != nil {
		writeErr(w, err)
		return
	}
	srv.trackSignin(r, "google", uid, created)
	srv.issueSession(w, uid, uname, in.Vars, created)
}

// handleLinkGoogle adds a Google sign-in to the signed-in account (Nakama's
// LinkGoogle). 409 AlreadyExists: that Google account is another player's,
// so the app signs in to it instead.
func (srv *Server) handleLinkGoogle(w http.ResponseWriter, r *http.Request) {
	c, err := srv.sessionFromRequest(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	b, err := srv.readBodyLimit(w, r, srv.cfg.Socket.MaxRequestSizeBytes)
	if err != nil {
		writeErr(w, err)
		return
	}
	var in api.AccountGoogle
	if err := jsonUnmarshal.Unmarshal(b, &in); err != nil || in.Token == "" {
		writeNakamaError(w, CodeInvalidArgument, "Google token is required.")
		return
	}
	if !srv.guard.LimitAuth(w, r, "", false) {
		return
	}
	profile, err := srv.google.Verify(r.Context(), in.Token)
	if err != nil {
		writeNakamaError(w, CodeUnauthenticated, "Could not authenticate Google profile.")
		return
	}
	if err := LinkGoogle(r.Context(), srv.db, c.UserID, profile); err != nil {
		writeErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte("{}"))
}

func (srv *Server) handleSessionRefresh(w http.ResponseWriter, r *http.Request) {
	var in api.SessionRefreshRequest
	if !srv.authPrelude(w, r, &in) || !srv.guard.LimitAuth(w, r, "", false) {
		return
	}
	c, err := srv.tokens.VerifyRefresh(in.Token)
	if err != nil {
		writeNakamaError(w, CodeUnauthenticated, "Refresh token invalid or expired.")
		return
	}
	var disable time.Time
	var username string
	if err := srv.db.QueryRow(r.Context(), "SELECT username, disable_time FROM users WHERE id = $1", c.UserID).Scan(&username, &disable); err != nil {
		writeNakamaError(w, CodeUnauthenticated, "Refresh token invalid or expired.")
		return
	}
	if disable.Unix() != 0 {
		writeErr(w, errAccountDisabled)
		return
	}
	vars := c.Vars
	if len(in.Vars) > 0 {
		vars = in.Vars
	}
	srv.issueSession(w, c.UserID, username, vars, false)
}

func (srv *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if _, err := srv.sessionFromRequest(r); err != nil {
		writeErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte("{}"))
}

// ---------------------------------------------------------------- v1 account

func ts(t time.Time) *timestamppb.Timestamp { return timestamppb.New(t) }

func (srv *Server) handleGetAccount(w http.ResponseWriter, r *http.Request) {
	c, err := srv.sessionFromRequest(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	a, err := GetAccount(r.Context(), srv.db, c.UserID)
	if err != nil {
		writeErr(w, err)
		return
	}
	out := &api.Account{
		User: &api.User{Id: a.UserID, Username: a.Username, DisplayName: a.DisplayName, AvatarUrl: a.AvatarURL, LangTag: a.LangTag,
			Location: a.Location, Timezone: a.Timezone, Metadata: a.Metadata, GoogleId: a.GoogleID, FacebookId: a.FacebookID,
			AppleId: a.AppleID, GamecenterId: a.GamecenterID, SteamId: a.SteamID, EdgeCount: int32(a.EdgeCount),
			CreateTime: ts(a.CreateTime), UpdateTime: ts(a.UpdateTime)},
		Wallet: a.Wallet, Email: a.Email, CustomId: a.CustomID, VerifyTime: ts(a.VerifyTime),
	}
	if a.DisableTime.Unix() != 0 {
		out.DisableTime = ts(a.DisableTime)
	}
	for _, d := range a.Devices {
		out.Devices = append(out.Devices, &api.AccountDevice{Id: d})
	}
	writeProto(w, out)
}

func (srv *Server) handleUpdateAccount(w http.ResponseWriter, r *http.Request) {
	c, err := srv.sessionFromRequest(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	b, err := srv.readBody(w, r)
	if err != nil {
		writeErr(w, err)
		return
	}
	var in api.UpdateAccountRequest
	if err := jsonUnmarshal.Unmarshal(b, &in); err != nil {
		writeNakamaError(w, CodeInvalidArgument, "Malformed request body.")
		return
	}
	str := func(v interface{ GetValue() string }, set bool) *string {
		if !set {
			return nil
		}
		s := v.GetValue()
		return &s
	}
	u := &AccountUpdate{UserID: c.UserID,
		DisplayName: str(in.DisplayName, in.DisplayName != nil), AvatarURL: str(in.AvatarUrl, in.AvatarUrl != nil),
		LangTag: str(in.LangTag, in.LangTag != nil), Location: str(in.Location, in.Location != nil), Timezone: str(in.Timezone, in.Timezone != nil)}
	if in.Username != nil {
		if in.Username.Value == "" {
			writeNakamaError(w, CodeInvalidArgument, "Username must be set.")
			return
		}
		u.Username = in.Username.Value
	}
	if err := UpdateAccounts(r.Context(), srv.db, []*AccountUpdate{u}); err != nil {
		writeErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte("{}"))
}

func (srv *Server) handleDeleteAccount(w http.ResponseWriter, r *http.Request) {
	c, err := srv.sessionFromRequest(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := DeleteAccount(r.Context(), srv.db, c.UserID); err != nil {
		writeErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte("{}"))
}

// ---------------------------------------------------------------- RPC

type rpcCaller struct {
	userID, username, sessionID string
	vars                        map[string]string
	headers                     map[string][]string
	clientIP                    string
	httpKey                     bool // called with the runtime HTTP key
	serverKey                   bool // called with the public server key (Basic)
	viaSocket                   bool
}

var errRPCNotFound = statusErr(CodeNotFound, "RPC function not found")

func messageOfRPC(err error) string {
	var re *rpc.Error
	if errors.As(err, &re) {
		return re.Message
	}
	return messageOf(err)
}

func codeOfRPC(err error) Code {
	var re *rpc.Error
	if errors.As(err, &re) {
		return Code(re.Code)
	}
	return codeOf(err)
}

func (srv *Server) isPublicRPC(id string, r *rpc.RPC) bool {
	if r.Access == rpc.AccessPublic {
		return true
	}
	for _, p := range srv.cfg.Runtime.PublicRPCs {
		if strings.EqualFold(p, id) {
			return true
		}
	}
	return false
}

// callRPC checks access and runs an RPC. id must be lower case.
func (srv *Server) callRPC(ctx context.Context, id string, c *rpcCaller, payload string) (out string, err error) {
	if ok, _ := srv.guard.AllowRPC(c.userID, c.clientIP); !ok {
		return "", statusErr(CodeResourceExhausted, "Too many requests, please slow down")
	}
	r := srv.rpcs[id]
	if r == nil {
		return "", errRPCNotFound
	}
	switch {
	case srv.isPublicRPC(id, r):
	case r.Access == rpc.AccessServer:
		if !c.httpKey {
			return "", statusErr(CodePermissionDenied, "This RPC requires server credentials")
		}
	case r.Access == rpc.AccessUser:
		if c.userID == "" {
			return "", statusErr(CodeUnauthenticated, "Auth token required")
		}
	default:
		if c.userID == "" && !c.httpKey {
			return "", statusErr(CodeUnauthenticated, "Auth token or HTTP key required")
		}
	}
	start := time.Now()
	defer func() {
		if rec := recover(); rec != nil {
			srv.logger.Error("rpc panic", "id", id, "error", fmt.Sprint(rec), "stack", string(debug.Stack()))
			out, err = "", statusErr(CodeInternal, fmt.Sprint(rec))
		}
		status := "ok"
		if err != nil {
			status = "error"
		}
		srv.metrics.TimerRecord("rpc_duration", map[string]string{"id": id, "status": status}, time.Since(start).Nanoseconds())
	}()
	rc := &rpc.Context{Ctx: ctx, UserID: c.userID, Username: c.username, SessionID: c.sessionID, Vars: c.vars, Headers: c.headers, ClientIP: c.clientIP}
	if rc.Vars == nil {
		rc.Vars = map[string]string{}
	}
	return r.Fn(rc, payload)
}

// rpcCallerFromRequest authenticates an HTTP RPC call. A nil caller with a
// nil error means "no credentials" (allowed only for public RPCs).
func (srv *Server) rpcCallerFromRequest(r *http.Request) (*rpcCaller, error) {
	c := &rpcCaller{clientIP: srv.clientIP(r), headers: map[string][]string(r.Header.Clone())}
	q := r.URL.Query()
	if q.Has("http_key") {
		if q.Get("http_key") != srv.cfg.Runtime.HTTPKey || srv.cfg.Runtime.HTTPKey == "" {
			return nil, statusErr(CodeUnauthenticated, "HTTP key invalid")
		}
		c.httpKey = true
		return c, nil
	}
	auth := r.Header.Get("Authorization")
	if tok, ok := parseBearer(auth); ok {
		claims, err := srv.tokens.Verify(tok)
		if err != nil {
			return nil, statusErr(CodeUnauthenticated, "Auth token invalid")
		}
		c.userID, c.username, c.vars, c.sessionID = claims.UserID, claims.Username, claims.Vars, claims.TokenID
		srv.analytics.Active(claims.UserID, clientTag(r))
		return c, nil
	}
	if _, ok := parseBasic(auth); ok {
		if !srv.checkServerKey(r) {
			return nil, statusErr(CodeUnauthenticated, "Server key invalid")
		}
		c.serverKey = true
		return c, nil
	}
	return c, nil
}

func (srv *Server) handleRPCv1(w http.ResponseWriter, r *http.Request) {
	id := strings.ToLower(r.PathValue("id"))
	caller, err := srv.rpcCallerFromRequest(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	if reg := srv.rpcs[id]; reg != nil && !srv.isPublicRPC(id, reg) && caller.userID == "" && !caller.httpKey {
		writeNakamaError(w, CodeUnauthenticated, "Auth token or HTTP key required")
		return
	}
	unwrap := r.URL.Query().Has("unwrap")
	var payload string
	if r.Method == http.MethodGet {
		payload = r.URL.Query().Get("payload")
	} else {
		limit := srv.cfg.Socket.MaxRequestSizeBytes
		if caller.httpKey && caller.userID == "" && maxServerRPCBodyBytes > limit {
			limit = maxServerRPCBodyBytes
		}
		b, err := srv.readBodyLimit(w, r, limit)
		if err != nil {
			writeErr(w, err)
			return
		}
		if unwrap {
			payload = string(b)
		} else if len(b) > 0 {
			if err := json.Unmarshal(b, &payload); err != nil {
				writeNakamaError(w, CodeInvalidArgument, "Unable to parse request body as a JSON string; use ?unwrap for raw payloads.")
				return
			}
		}
	}
	out, err := srv.callRPC(r.Context(), id, caller, payload)
	if err != nil {
		writeNakamaError(w, codeOfRPC(err), messageOfRPC(err))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if unwrap {
		_, _ = w.Write([]byte(out))
		return
	}
	b, _ := json.Marshal(map[string]string{"id": id, "payload": out})
	_, _ = w.Write(b)
}
