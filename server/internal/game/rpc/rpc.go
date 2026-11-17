// Package rpc contains every RPC the Tango Cards app calls. Handlers take a
// JSON payload and return JSON; all game and economy rules are enforced here
// or in the match handler, never trusted from the client.
package rpc

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/dev-sssss/tango_backend/server/internal/game"
)

// Context describes the caller of an RPC.
type Context struct {
	Ctx       context.Context
	UserID    string // empty for server-to-server calls
	Username  string
	SessionID string
	Vars      map[string]string
	Headers   map[string][]string
	ClientIP  string
}

func (c *Context) context() context.Context {
	if c.Ctx == nil {
		return context.Background()
	}
	return c.Ctx
}

// Error is returned by handlers. Code follows gRPC status codes; HTTP status
// is derived from it by the API layer.
type Error struct {
	Code    int
	Message string
}

func (e *Error) Error() string { return e.Message }

const (
	codeInternal         = 13
	codeNotFound         = 5
	codeInvalidArgument  = 3
	codePermissionDenied = 7
	codeUnauthenticated  = 16
)

// fail is an internal error: HTTP 500, code 13.
func fail(msg string) error { return &Error{Code: codeInternal, Message: msg} }

// Access controls who may call an RPC.
type Access int

const (
	// AccessAny: users with a session, or server credentials (HTTP key).
	AccessAny Access = iota
	// AccessUser: a user session is required.
	AccessUser
	// AccessServer: server credentials only (HTTP key or admin).
	AccessServer
	// AccessPublic: callable before login with just the server key.
	AccessPublic
)

type Handler func(c *Context, payload string) (string, error)

type RPC struct {
	ID      string
	Fn      Handler
	Access  Access
	Summary string
}

// Service holds dependencies shared by all handlers.
type Service struct {
	NK            game.NK
	Announcements *game.Announcements
}

func NewService(nk game.NK, ann *game.Announcements) *Service {
	if ann == nil {
		ann = &game.Announcements{}
	}
	return &Service{NK: nk, Announcements: ann}
}

// Registry returns every RPC keyed by lower-case id. AccessUser is the
// default for anything a player calls; admin RPCs need server credentials.
func (s *Service) Registry() map[string]*RPC {
	list := []*RPC{
		// profile
		{ID: "tango.profile.get", Fn: s.getProfile, Access: AccessUser, Summary: "Profile, wallet, level; creates the profile and starting wallet on first call"},
		{ID: "tango.profile.update", Fn: s.updateProfile, Access: AccessUser, Summary: "Change display name; mark tutorial done"},
		{ID: "tango.account.delete", Fn: s.deleteOwnAccount, Access: AccessUser, Summary: "Delete the caller's account (store requirement)"},
		// economy
		{ID: "tango.wallet.get", Fn: s.getWallet, Access: AccessUser},
		{ID: "tango.shop.catalog", Fn: s.getCatalog, Access: AccessUser},
		{ID: "tango.shop.purchase", Fn: s.purchaseItem, Access: AccessUser},
		{ID: "tango.inventory.get", Fn: s.getInventory, Access: AccessUser},
		{ID: "tango.inventory.equip", Fn: s.equipItem, Access: AccessUser},
		{ID: "tango.iap.verify_google", Fn: s.verifyGooglePurchase, Access: AccessUser},
		{ID: "tango.ads.claim_reward", Fn: s.claimAdReward, Access: AccessUser},
		{ID: "tango.dev.grant", Fn: s.devGrant, Access: AccessUser, Summary: "Non-prod only (TANGO_ALLOW_CHEATS=true): +10,000 coins, +1,000 diamonds"},
		// matches
		{ID: "tango.match.quick_join", Fn: s.quickJoin, Access: AccessUser},
		{ID: "tango.match.quick_join_cancel", Fn: s.quickJoinCancel, Access: AccessUser},
		{ID: "tango.room.create", Fn: s.createRoom, Access: AccessUser},
		{ID: "tango.room.join", Fn: s.joinRoom, Access: AccessUser},
		{ID: "tango.match.active", Fn: s.activeMatch, Access: AccessUser},
		{ID: "tango.match.forfeit", Fn: s.forfeitActive, Access: AccessUser},
		// social
		{ID: "tango.friends.list", Fn: s.friendsList, Access: AccessUser},
		{ID: "tango.friends.add", Fn: s.friendsAdd, Access: AccessUser},
		{ID: "tango.friends.find", Fn: s.friendsFind, Access: AccessUser},
		{ID: "tango.friends.action", Fn: s.friendsAction, Access: AccessUser},
		{ID: "tango.friends.invite", Fn: s.friendsInvite, Access: AccessUser},
		{ID: "tango.leaderboard.get", Fn: s.getLeaderboard, Access: AccessUser},
		// misc
		{ID: "tango.app.config", Fn: s.appConfig, Access: AccessAny},
		{ID: "tango.announcements", Fn: s.getAnnouncements, Access: AccessAny},
		{ID: "tango.client.log_error", Fn: s.logClientError, Access: AccessUser},
		// admin (server credentials only)
		{ID: "tango.admin.grant", Fn: s.adminGrant, Access: AccessServer},
		{ID: "tango.admin.set_app_config", Fn: s.adminSetAppConfig, Access: AccessServer},
		{ID: "tango.admin.set_shop_catalog", Fn: s.adminSetCatalog, Access: AccessServer},
		{ID: "tango.admin.force_finish_match", Fn: s.adminForceFinish, Access: AccessServer},
		{ID: "tango.admin.delete_account", Fn: s.adminDeleteAccount, Access: AccessServer},
	}
	out := make(map[string]*RPC, len(list))
	for _, r := range list {
		out[strings.ToLower(r.ID)] = r
	}
	return out
}

// ---------------------------------------------------------------- helpers

// parse decodes a JSON object payload; empty or invalid gives an empty map.
func parse(payload string) map[string]any {
	if payload == "" {
		return map[string]any{}
	}
	m, ok := game.DecodeObject(payload)
	if !ok {
		return map[string]any{}
	}
	return m
}

func enc(v any) string { return game.Encode(v) }

func ok(fields map[string]any) (string, error) {
	fields["success"] = true
	return enc(fields), nil
}

func refuse(code string) (string, error) {
	return enc(map[string]any{"success": false, "error": code}), nil
}

func str(m map[string]any, key string, max int) string {
	v, _ := m[key].(string)
	v = strings.TrimSpace(v)
	if len(v) > max {
		return ""
	}
	return v
}

func num(m map[string]any, key string, def int64) int64 {
	if f, ok := game.ToNumber(m[key]); ok {
		return int64(f)
	}
	return def
}

func (s *Service) blocked(c *Context, feature string) string {
	return s.Announcements.Blocked(c.context(), s.NK, feature)
}

func (s *Service) read1(ctx context.Context, coll, key, user string) (*game.Object, error) {
	rows, err := s.NK.StorageRead(ctx, []game.ReadID{{Collection: coll, Key: key, UserID: user}})
	if err != nil {
		return nil, fail("storage read failed: " + err.Error())
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return rows[0], nil
}

func isVersionErr(err error) bool { return errors.Is(err, game.ErrStorageVersion) }

func (s *Service) wallet(ctx context.Context, userID string) (map[string]int64, error) {
	a, err := s.NK.AccountGet(ctx, userID)
	if err != nil {
		return nil, fail("account read failed: " + err.Error())
	}
	w := map[string]int64{"coins": a.Wallet["coins"], "diamonds": a.Wallet["diamonds"]}
	return w, nil
}

var _ = fmt.Sprintf
