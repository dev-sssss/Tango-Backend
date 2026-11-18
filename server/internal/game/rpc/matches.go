package rpc

import (
	"fmt"
	"strconv"

	"github.com/dev-sssss/tango_backend/server/internal/game"
	"github.com/dev-sssss/tango_backend/server/internal/game/tango"
)

// Matchmaking: one advertised waiting match per (mode, format, fee). The
// RPC returns a match id; the app then joins it over the realtime socket.
// After 15 s the match fills empty seats with bots by itself.

func (s *Service) matchLabel(c *Context, matchID any) (map[string]any, bool) {
	id, isStr := matchID.(string)
	if !isStr || id == "" {
		return nil, false
	}
	info, err := s.NK.MatchGet(c.context(), id)
	if err != nil || info == nil {
		return nil, false
	}
	l, okL := game.DecodeObject(info.Label)
	return l, okL
}

func (s *Service) quickJoin(c *Context, payload string) (string, error) {
	ctx := c.context()
	if reason := s.blocked(c, "online_matches"); reason != "" {
		return enc(map[string]any{"success": false, "error": "MAINTENANCE", "message": reason}), nil
	}
	data := parse(payload)
	mode, format, fee := str(data, "mode", 16), str(data, "format", 8), num(data, "fee", 0)
	cfg := s.loadAppConfig(ctx)
	if !tango.ValidMode(mode) || !tango.ValidFormat(format) || !cfg.feeAllowed(fee) {
		return refuse("INVALID_REQUEST")
	}
	if _, err := s.ensureProfile(ctx, c.UserID, c.Username); err != nil {
		return "", err
	}
	w, err := s.wallet(ctx, c.UserID)
	if err != nil {
		return "", err
	}
	if w["coins"] < fee {
		return refuse("INSUFFICIENT_COINS")
	}
	key := fmt.Sprintf("waiting_%s_%s_%d", mode, format, fee)
	for attempt := 0; attempt < 5; attempt++ {
		row, err := s.read1(ctx, "matchmaking", key, "")
		if err != nil {
			return "", err
		}
		if row != nil {
			if l, found := s.matchLabel(c, row.Value["match_id"]); found && l["status"] == tango.StatusWaiting && num(l, "players", 0) < num(l, "max", 0) {
				return ok(map[string]any{"status": "matched", "match_id": row.Value["match_id"], "mode": mode, "format": format, "fee": fee})
			}
			_ = s.NK.StorageDelete(ctx, []game.DeleteID{{Collection: "matchmaking", Key: key, Version: row.Version}})
		}
		matchID, err := s.NK.MatchCreate(ctx, tango.Module, map[string]any{"mode": mode, "format": format, "fee": fee})
		if err != nil {
			return "", fail("match create failed: " + err.Error())
		}
		_, err = s.NK.StorageWrite(ctx, []*game.Write{{Collection: "matchmaking", Key: key, Version: "*",
			Value: map[string]any{"match_id": matchID, "created_by": c.UserID, "created_ms": s.NK.NowMs()}}})
		if err == nil {
			return ok(map[string]any{"status": "created", "match_id": matchID, "mode": mode, "format": format, "fee": fee})
		}
		// Someone else advertised a match first: retire ours and retry.
		_, _ = s.NK.MatchSignal(ctx, matchID, `{"action":"CANCEL_EMPTY_MATCH"}`)
	}
	return refuse("MATCHMAKING_BUSY")
}

// quickJoinCancel exists for symmetry: leaving the match over the socket is
// what frees the seat; this only confirms.
func (s *Service) quickJoinCancel(c *Context, payload string) (string, error) {
	return ok(map[string]any{"status": "cancelled"})
}

func (s *Service) createRoom(c *Context, payload string) (string, error) {
	ctx := c.context()
	if reason := s.blocked(c, "online_matches"); reason != "" {
		return enc(map[string]any{"success": false, "error": "MAINTENANCE", "message": reason}), nil
	}
	data := parse(payload)
	mode, format, fee := str(data, "mode", 16), str(data, "format", 8), num(data, "fee", 0)
	if mode == "" {
		mode = tango.ModeClassic
	}
	if format == "" {
		format = tango.Format1v1
	}
	cfg := s.loadAppConfig(ctx)
	if !tango.ValidMode(mode) || !tango.ValidFormat(format) || (fee != 0 && !cfg.feeAllowed(fee)) {
		return refuse("INVALID_REQUEST")
	}
	if _, err := s.ensureProfile(ctx, c.UserID, c.Username); err != nil {
		return "", err
	}
	if fee > 0 {
		w, err := s.wallet(ctx, c.UserID)
		if err != nil {
			return "", err
		}
		if w["coins"] < fee {
			return refuse("INSUFFICIENT_COINS")
		}
	}
	settings := map[string]any{"turn_seconds": int64(10), "emotes": true, "spectators": false, "mic": false}
	if sm, isMap := data["settings"].(map[string]any); isMap {
		ts := num(sm, "turn_seconds", 10)
		if mm := num(sm, "match_minutes", 0); mm == 3 || mm == 5 || mm == 10 {
			settings["match_minutes"] = mm
		}
		valid := false
		for _, o := range cfg.TurnSecondsOptions {
			if int64(o) == ts {
				valid = true
			}
		}
		if valid {
			settings["turn_seconds"] = ts
		}
		for _, k := range []string{"emotes", "spectators", "mic", "jump_in", "seven_o", "bots"} {
			if b, isBool := sm[k].(bool); isBool {
				settings[k] = b
			}
		}
	}
	for attempt := 0; attempt < 8; attempt++ {
		code := strconv.Itoa(s.NK.RandInt(100000, 999999))
		row, err := s.read1(ctx, "private_rooms", "room_"+code, "")
		if err != nil {
			return "", err
		}
		if row != nil {
			if _, live := s.matchLabel(c, row.Value["match_id"]); live {
				continue
			}
		}
		matchID, err := s.NK.MatchCreate(ctx, tango.Module, map[string]any{"mode": mode, "format": format, "fee": fee, "private": true,
			"host_id": c.UserID, "room_code": code, "settings": settings})
		if err != nil {
			return "", fail("match create failed: " + err.Error())
		}
		_, err = s.NK.StorageWrite(ctx, []*game.Write{{Collection: "private_rooms", Key: "room_" + code,
			Value: map[string]any{"match_id": matchID, "host_id": c.UserID, "mode": mode, "format": format, "fee": fee, "created_ms": s.NK.NowMs()}}})
		if err != nil {
			return "", fail(err.Error())
		}
		return ok(map[string]any{"room_code": code, "match_id": matchID, "mode": mode, "format": format, "fee": fee, "settings": settings,
			"max_players": tango.FormatPlayers(format)})
	}
	return refuse("CODE_GENERATION_FAILED")
}

func (s *Service) joinRoom(c *Context, payload string) (string, error) {
	ctx := c.context()
	code := str(parse(payload), "room_code", 16)
	if len(code) != 6 {
		return refuse("ROOM_NOT_FOUND")
	}
	row, err := s.read1(ctx, "private_rooms", "room_"+code, "")
	if err != nil {
		return "", err
	}
	if row == nil {
		return refuse("ROOM_NOT_FOUND")
	}
	l, live := s.matchLabel(c, row.Value["match_id"])
	if !live {
		return refuse("ROOM_CLOSED")
	}
	if l["status"] != tango.StatusWaiting {
		return refuse("ROOM_STARTED")
	}
	if num(l, "players", 0) >= num(l, "max", 0) {
		return refuse("ROOM_FULL")
	}
	fee := num(l, "fee", 0)
	if _, err := s.ensureProfile(ctx, c.UserID, c.Username); err != nil {
		return "", err
	}
	if fee > 0 {
		w, err := s.wallet(ctx, c.UserID)
		if err != nil {
			return "", err
		}
		if w["coins"] < fee {
			return refuse("INSUFFICIENT_COINS")
		}
	}
	return ok(map[string]any{"room_code": code, "match_id": row.Value["match_id"], "mode": l["mode"], "format": l["format"], "fee": fee})
}

func (s *Service) activeMatch(c *Context, payload string) (string, error) {
	ctx := c.context()
	row, err := s.read1(ctx, "active_match", "current", c.UserID)
	if err != nil {
		return "", err
	}
	if row == nil {
		return ok(map[string]any{"match_id": nil})
	}
	if l, live := s.matchLabel(c, row.Value["match_id"]); live && l["status"] == tango.StatusPlaying {
		return ok(map[string]any{"match_id": row.Value["match_id"], "mode": l["mode"], "format": l["format"], "fee": l["fee"]})
	}
	_ = s.NK.StorageDelete(ctx, []game.DeleteID{{Collection: "active_match", Key: "current", UserID: c.UserID}})
	return ok(map[string]any{"match_id": nil})
}

func (s *Service) forfeitActive(c *Context, payload string) (string, error) {
	ctx := c.context()
	row, err := s.read1(ctx, "active_match", "current", c.UserID)
	if err != nil {
		return "", err
	}
	if row == nil {
		return refuse("NO_ACTIVE_MATCH")
	}
	id, _ := row.Value["match_id"].(string)
	if _, err := s.NK.MatchSignal(ctx, id, enc(map[string]any{"action": "FORFEIT", "user_id": c.UserID})); err != nil {
		_ = s.NK.StorageDelete(ctx, []game.DeleteID{{Collection: "active_match", Key: "current", UserID: c.UserID}})
		return refuse("NO_ACTIVE_MATCH")
	}
	return ok(map[string]any{"match_id": id})
}

// findOpponents: the host of a friends party (a private room) takes it to
// public matchmaking with an entry fee. The room becomes the advertised
// waiting table for (mode, format, fee), so players pressing Play on that
// table sit down with the party; bots fill what is left after the wait.
func (s *Service) findOpponents(c *Context, payload string) (string, error) {
	ctx := c.context()
	if reason := s.blocked(c, "online_matches"); reason != "" {
		return enc(map[string]any{"success": false, "error": "MAINTENANCE", "message": reason}), nil
	}
	data := parse(payload)
	id, fee := str(data, "match_id", 128), num(data, "fee", 0)
	if !s.loadAppConfig(ctx).feeAllowed(fee) {
		return refuse("INVALID_REQUEST")
	}
	l, live := s.matchLabel(c, id)
	if !live || l["status"] != tango.StatusWaiting {
		return refuse("ROOM_CLOSED")
	}
	mode, _ := l["mode"].(string)
	format, _ := l["format"].(string)
	resp, err := s.NK.MatchSignal(ctx, id, enc(map[string]any{"action": "GO_PUBLIC", "user_id": c.UserID, "fee": fee}))
	if err != nil {
		return refuse("ROOM_CLOSED")
	}
	r := parse(resp)
	if r["ok"] != true {
		code, _ := r["error"].(string)
		if code == "" {
			code = "NOT_ALLOWED"
		}
		out := map[string]any{"success": false, "error": code}
		if name, _ := r["name"].(string); name != "" {
			out["name"] = name
			out["message"] = name + " doesn't have enough coins for this table."
		}
		return enc(out), nil
	}
	key := fmt.Sprintf("waiting_%s_%s_%d", mode, format, fee)
	if _, err := s.NK.StorageWrite(ctx, []*game.Write{{Collection: "matchmaking", Key: key,
		Value: map[string]any{"match_id": id, "created_by": c.UserID, "created_ms": s.NK.NowMs(), "party": true}}}); err != nil {
		return "", err
	}
	return ok(map[string]any{"status": "searching", "match_id": id, "mode": mode, "format": format, "fee": fee})
}

// lobbyStats: players online right now and who is waiting at each public
// table of a mode, so the table picker can show where the players are.
func (s *Service) lobbyStats(c *Context, payload string) (string, error) {
	ctx := c.context()
	data := parse(payload)
	mode := str(data, "mode", 16)
	out := map[string]any{"online": 0, "in_game": 0}
	if soc, has := game.SocialFrom(s.NK); has {
		if online, inGame, err := soc.OnlineCounts(ctx); err == nil {
			out["online"], out["in_game"] = online, inGame
		}
	}
	if tango.ValidMode(mode) {
		waiting := map[string]any{}
		for _, format := range []string{tango.Format1v1, tango.Format2v2, tango.Format4P, tango.Format4v4, tango.Format8P} {
			for _, fee := range s.loadAppConfig(ctx).EntryFees {
				key := fmt.Sprintf("waiting_%s_%s_%d", mode, format, fee)
				row, err := s.read1(ctx, "matchmaking", key, "")
				if err != nil || row == nil {
					continue
				}
				if l, found := s.matchLabel(c, row.Value["match_id"]); found && l["status"] == tango.StatusWaiting && num(l, "players", 0) > 0 && num(l, "players", 0) < num(l, "max", 0) {
					waiting[fmt.Sprintf("%s_%d", format, fee)] = num(l, "players", 0)
				}
			}
		}
		out["waiting"] = waiting
	}
	return ok(out)
}
