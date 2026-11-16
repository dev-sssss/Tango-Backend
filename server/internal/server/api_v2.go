package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// API v2: plain JSON over /api/v2. Differences from v1 (Nakama paths):
//   - auth responses include user_id, username and expiry times, so clients
//     can check expiry locally instead of calling GET /v2/account first;
//   - RPC bodies and responses are the JSON payload itself (no ?unwrap);
//   - errors are {"error":{"code":"NOT_FOUND","status":404,"message":"..."}};
//   - every response says which version answered and whether it is deprecated.

var codeNames = map[Code]string{
	CodeOK: "OK", CodeCanceled: "CANCELED", CodeUnknown: "UNKNOWN", CodeInvalidArgument: "INVALID_ARGUMENT",
	CodeDeadlineExceeded: "DEADLINE_EXCEEDED", CodeNotFound: "NOT_FOUND", CodeAlreadyExists: "ALREADY_EXISTS",
	CodePermissionDenied: "PERMISSION_DENIED", CodeResourceExhausted: "RESOURCE_EXHAUSTED", CodeFailedPrecondition: "FAILED_PRECONDITION",
	CodeAborted: "ABORTED", CodeOutOfRange: "OUT_OF_RANGE", CodeUnimplemented: "UNIMPLEMENTED", CodeInternal: "INTERNAL",
	CodeUnavailable: "UNAVAILABLE", CodeDataLoss: "DATA_LOSS", CodeUnauthenticated: "UNAUTHENTICATED",
}

func writeV2Error(w http.ResponseWriter, code Code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code.HTTPStatus())
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": codeNames[code], "status": code.HTTPStatus(), "message": msg}})
}

func writeV2(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (srv *Server) registerV2(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/meta", srv.handleAPIMeta)
	mux.HandleFunc("POST /api/v2/auth/device", srv.v2(srv.v2AuthDevice))
	mux.HandleFunc("POST /api/v2/auth/email", srv.v2(srv.v2AuthEmail))
	mux.HandleFunc("POST /api/v2/auth/google", srv.v2(srv.v2AuthGoogle))
	mux.HandleFunc("POST /api/v2/auth/refresh", srv.v2(srv.v2Refresh))
	mux.HandleFunc("GET /api/v2/account", srv.v2(srv.v2Account))
	mux.HandleFunc("POST /api/v2/rpc/{id}", srv.v2(srv.v2RPC))
	mux.HandleFunc("GET /api/v2/socket", srv.v2(srv.handleSocket))
}

func (srv *Server) v2(fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		srv.versions.annotate(w, "v2")
		srv.usage.Record("v2", clientTag(r))
		fn(w, r)
	}
}

func (srv *Server) handleAPIMeta(w http.ResponseWriter, r *http.Request) {
	type ver struct {
		Name       string `json:"name"`
		Deprecated string `json:"deprecated,omitempty"`
		Sunset     string `json:"sunset,omitempty"`
	}
	var vs []ver
	for _, x := range srv.versions.list {
		v := ver{Name: x.Name}
		if !x.Deprecated.IsZero() {
			v.Deprecated = x.Deprecated.Format("2006-01-02")
		}
		if !x.Sunset.IsZero() {
			v.Sunset = x.Sunset.Format("2006-01-02")
		}
		vs = append(vs, v)
	}
	writeV2(w, map[string]any{"versions": vs, "server": BuildVersion, "min_support_days": int(MinSupportWindow.Hours() / 24)})
}

func (srv *Server) v2Body(w http.ResponseWriter, r *http.Request, into any) bool {
	b, err := srv.readBody(w, r)
	if err != nil {
		writeV2Error(w, codeOf(err), messageOf(err))
		return false
	}
	if len(b) == 0 {
		b = []byte("{}")
	}
	if err := json.Unmarshal(b, into); err != nil {
		writeV2Error(w, CodeInvalidArgument, "Malformed JSON body.")
		return false
	}
	return true
}

type v2AuthIn struct {
	DeviceID string            `json:"device_id"`
	Email    string            `json:"email"`
	Password string            `json:"password"`
	IDToken  string            `json:"id_token"`
	Username string            `json:"username"`
	Create   bool              `json:"create"`
	Vars     map[string]string `json:"vars"`
}

func (srv *Server) v2Session(w http.ResponseWriter, userID, username string, vars map[string]string, created bool) {
	tok, refresh, exp := srv.tokens.Issue(userID, username, vars)
	writeV2(w, map[string]any{
		"token": tok, "refresh_token": refresh, "created": created, "user_id": userID, "username": username,
		"expires_at": exp, "refresh_expires_at": time.Now().Add(time.Duration(srv.cfg.Session.RefreshTokenExpirySec) * time.Second).Unix(),
	})
}

func (srv *Server) v2AuthDevice(w http.ResponseWriter, r *http.Request) {
	var in v2AuthIn
	if !srv.v2Body(w, r, &in) || !srv.guard.LimitAuth(w, r, "", true) {
		return
	}
	uid, uname, created, err := AuthenticateDevice(r.Context(), srv.db, in.DeviceID, in.Username, in.Create)
	if err != nil {
		writeV2Error(w, codeOf(err), messageOf(err))
		return
	}
	srv.trackSignin(r, "device", uid, created)
	srv.v2Session(w, uid, uname, in.Vars, created)
}

func (srv *Server) v2AuthEmail(w http.ResponseWriter, r *http.Request) {
	var in v2AuthIn
	if !srv.v2Body(w, r, &in) || !srv.guard.LimitAuth(w, r, in.Email, true) {
		return
	}
	release, err := srv.guard.HashSlot(r.Context())
	if err != nil {
		writeV2Error(w, codeOf(err), messageOf(err))
		return
	}
	uid, uname, created, err := AuthenticateEmail(r.Context(), srv.db, in.Email, in.Password, in.Username, in.Create)
	release()
	if err != nil {
		writeV2Error(w, codeOf(err), messageOf(err))
		return
	}
	srv.trackSignin(r, "email", uid, created)
	srv.v2Session(w, uid, uname, in.Vars, created)
}

func (srv *Server) v2AuthGoogle(w http.ResponseWriter, r *http.Request) {
	var in v2AuthIn
	if !srv.v2Body(w, r, &in) || !srv.guard.LimitAuth(w, r, "", true) {
		return
	}
	profile, err := srv.google.Verify(r.Context(), in.IDToken)
	if err != nil {
		writeV2Error(w, CodeUnauthenticated, "Could not verify the Google ID token.")
		return
	}
	uid, uname, created, err := AuthenticateGoogle(r.Context(), srv.db, profile, in.Username, in.Create)
	if err != nil {
		writeV2Error(w, codeOf(err), messageOf(err))
		return
	}
	srv.trackSignin(r, "google", uid, created)
	srv.v2Session(w, uid, uname, in.Vars, created)
}

func (srv *Server) v2Refresh(w http.ResponseWriter, r *http.Request) {
	var in struct {
		RefreshToken string `json:"refresh_token"`
	}
	if !srv.v2Body(w, r, &in) || !srv.guard.LimitAuth(w, r, "", true) {
		return
	}
	c, err := srv.tokens.VerifyRefresh(in.RefreshToken)
	if err != nil {
		writeV2Error(w, CodeUnauthenticated, "Refresh token invalid or expired.")
		return
	}
	var username string
	var disable time.Time
	if err := srv.db.QueryRow(r.Context(), "SELECT username, disable_time FROM users WHERE id = $1", c.UserID).Scan(&username, &disable); err != nil {
		writeV2Error(w, CodeUnauthenticated, "Refresh token invalid or expired.")
		return
	}
	if disable.Unix() != 0 {
		writeV2Error(w, CodePermissionDenied, "User account banned.")
		return
	}
	srv.v2Session(w, c.UserID, username, c.Vars, false)
}

func (srv *Server) v2Account(w http.ResponseWriter, r *http.Request) {
	c, err := srv.sessionFromRequest(r)
	if err != nil {
		writeV2Error(w, codeOf(err), messageOf(err))
		return
	}
	a, err := GetAccount(r.Context(), srv.db, c.UserID)
	if err != nil {
		writeV2Error(w, codeOf(err), messageOf(err))
		return
	}
	writeV2(w, map[string]any{
		"user_id": a.UserID, "username": a.Username, "display_name": a.DisplayName, "avatar_url": a.AvatarURL,
		"email": a.Email, "wallet": json.RawMessage(a.Wallet), "metadata": json.RawMessage(a.Metadata),
		"create_time": a.CreateTime.Unix(), "google_linked": a.GoogleID != "", "devices": len(a.Devices),
	})
}

func (srv *Server) v2RPC(w http.ResponseWriter, r *http.Request) {
	id := strings.ToLower(r.PathValue("id"))
	caller, err := srv.rpcCallerFromRequest(r)
	if err != nil {
		writeV2Error(w, codeOf(err), messageOf(err))
		return
	}
	b, err := srv.readBody(w, r)
	if err != nil {
		writeV2Error(w, codeOf(err), messageOf(err))
		return
	}
	payload := string(b)
	if strings.TrimSpace(payload) == "" {
		payload = "{}"
	}
	out, err := srv.callRPC(r.Context(), id, caller, payload)
	if err != nil {
		writeV2Error(w, codeOfRPC(err), messageOfRPC(err))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if out == "" {
		out = "null"
	}
	_, _ = w.Write([]byte(out))
}
