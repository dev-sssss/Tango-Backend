package rpc

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/dev-sssss/tango_backend/server/internal/game"
	"github.com/dev-sssss/tango_backend/server/internal/game/tango"
)

func (s *Service) getLeaderboard(c *Context, payload string) (string, error) {
	ctx := c.context()
	data := parse(payload)
	id := str(data, "id", 32)
	switch id {
	case tango.LeaderboardWeekly, tango.LeaderboardWins:
	case tango.LeaderboardFriends:
		return s.friendsBoard(c)
	default:
		id = tango.LeaderboardTrophies
	}
	limit := int(num(data, "limit", 50))
	if limit < 1 || limit > 100 {
		limit = 50
	}
	records, own, next, _, err := s.NK.LeaderboardRecordsList(ctx, id, []string{c.UserID}, limit, str(data, "cursor", 512))
	if err != nil {
		return "", fail(err.Error())
	}
	reads := make([]game.ReadID, 0, len(records))
	for _, r := range records {
		reads = append(reads, game.ReadID{Collection: game.ProfileCollection, Key: game.ProfileKey, UserID: r.OwnerID})
	}
	profiles := map[string]*game.Profile{}
	if len(reads) > 0 {
		rows, err := s.NK.StorageRead(ctx, reads)
		if err != nil {
			return "", fail(err.Error())
		}
		for _, row := range rows {
			profiles[row.UserID] = game.ProfileFromMap(row.Value)
		}
	}
	out := make([]map[string]any, 0, len(records))
	for _, r := range records {
		e := map[string]any{"rank": r.Rank, "user_id": r.OwnerID, "name": r.Username, "score": r.Score}
		if p := profiles[r.OwnerID]; p != nil {
			e["name"], e["avatar"], e["frame"], e["level"] = p.Name, p.Avatar, p.Frame, tango.LevelFor(p.Trophies)
		}
		out = append(out, e)
	}
	var mine map[string]any
	if len(own) > 0 {
		mine = map[string]any{"rank": own[0].Rank, "score": own[0].Score}
	}
	return ok(map[string]any{"id": id, "records": out, "me": mine, "cursor": next})
}

// friendsBoard ranks the caller and their friends by trophies.
func (s *Service) friendsBoard(c *Context) (string, error) {
	ctx := c.context()
	me, err := s.ensureProfile(ctx, c.UserID, c.Username)
	if err != nil {
		return "", err
	}
	type row struct {
		id, name, avatar, frame string
		trophies                int64
	}
	rows := []row{{c.UserID, me.Name, me.Avatar, me.Frame, int64(me.Trophies)}}
	if soc, ok := game.SocialFrom(s.NK); ok {
		list, err := soc.FriendsList(ctx, c.UserID)
		if err != nil {
			return "", fail(err.Error())
		}
		if fs, isList := list["friends"].([]any); isList {
			for _, f := range fs {
				m, isMap := f.(map[string]any)
				if !isMap {
					continue
				}
				id, _ := m["user_id"].(string)
				name, _ := m["display_name"].(string)
				avatar, _ := m["avatar"].(string)
				frame, _ := m["frame"].(string)
				rows = append(rows, row{id, name, avatar, frame, num(m, "trophies", 0)})
			}
		}
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].trophies > rows[j].trophies })
	out := make([]map[string]any, 0, len(rows))
	var mine map[string]any
	for i, r := range rows {
		out = append(out, map[string]any{"rank": i + 1, "user_id": r.id, "name": r.name, "avatar": r.avatar, "frame": r.frame,
			"score": r.trophies, "level": tango.LevelFor(int(r.trophies))})
		if r.id == c.UserID {
			mine = map[string]any{"rank": i + 1, "score": r.trophies}
		}
	}
	return ok(map[string]any{"id": tango.LeaderboardFriends, "records": out, "me": mine, "cursor": ""})
}

func (s *Service) appConfig(c *Context, payload string) (string, error) {
	ctx := c.context()
	cfg := s.loadAppConfig(ctx)
	maint := ""
	if c.UserID != "" {
		maint = s.blocked(c, "online_matches")
	}
	return ok(map[string]any{"config": cfg, "maintenance": maint, "server_time_ms": s.NK.NowMs()})
}

func (s *Service) getAnnouncements(c *Context, payload string) (string, error) {
	doc, err := s.Announcements.GetDocument(c.context(), s.NK)
	if err != nil {
		return refuse("UNAVAILABLE")
	}
	return enc(game.Normalize(doc)), nil
}

func (s *Service) logClientError(c *Context, payload string) (string, error) {
	data := parse(payload)
	msg := str(data, "message", 2000)
	if msg == "" {
		return refuse("EMPTY")
	}
	s.NK.Logger().Warn("client error", "user_id", c.UserID, "message", msg, "context", str(data, "context", 500), "client", str(data, "client", 64))
	s.NK.Metrics().CounterAdd("client_errors_total", nil, 1)
	return ok(map[string]any{})
}

// ---------------------------------------------------------------- admin

func (s *Service) adminGrant(c *Context, payload string) (string, error) {
	ctx := c.context()
	data := parse(payload)
	uid := str(data, "user_id", 64)
	if !game.ValidUUID(uid) {
		return refuse("INVALID_USER")
	}
	cs := map[string]int64{}
	if v := num(data, "coins", 0); v != 0 {
		cs["coins"] = v
	}
	if v := num(data, "diamonds", 0); v != 0 {
		cs["diamonds"] = v
	}
	if len(cs) == 0 {
		return refuse("NOTHING_TO_GRANT")
	}
	updated, _, err := s.NK.WalletUpdate(ctx, uid, cs, map[string]any{"reason": "admin_grant", "note": str(data, "note", 200)}, true)
	if err != nil {
		return "", fail(err.Error())
	}
	return ok(map[string]any{"wallet": updated})
}

func (s *Service) adminSetAppConfig(c *Context, payload string) (string, error) {
	data := parse(payload)
	cfg, isMap := data["config"].(map[string]any)
	if !isMap {
		return refuse("INVALID_CONFIG")
	}
	var probe AppConfig
	b, _ := json.Marshal(cfg)
	if err := json.Unmarshal(b, &probe); err != nil {
		return refuse("INVALID_CONFIG")
	}
	if _, err := s.NK.StorageWrite(c.context(), []*game.Write{{Collection: configCollection, Key: appConfigKey, Value: cfg, PermissionRead: 0}}); err != nil {
		return "", fail(err.Error())
	}
	return ok(map[string]any{"config": s.loadAppConfig(c.context())})
}

func (s *Service) adminSetCatalog(c *Context, payload string) (string, error) {
	data := parse(payload)
	raw, isMap := data["catalog"].(map[string]any)
	if !isMap {
		return refuse("INVALID_CATALOG")
	}
	var cat Catalog
	b, _ := json.Marshal(raw)
	if err := json.Unmarshal(b, &cat); err != nil || len(cat.Items) == 0 {
		return refuse("INVALID_CATALOG")
	}
	seen := map[string]bool{}
	for _, it := range cat.Items {
		if it.ID == "" || seen[it.ID] || !strings.Contains("avatar frame badge card_back table emote", it.Category) {
			return refuse("INVALID_ITEM:" + it.ID)
		}
		seen[it.ID] = true
	}
	if _, err := s.NK.StorageWrite(c.context(), []*game.Write{{Collection: configCollection, Key: catalogKey, Value: raw, PermissionRead: 0}}); err != nil {
		return "", fail(err.Error())
	}
	return ok(map[string]any{"items": len(cat.Items)})
}

func (s *Service) adminForceFinish(c *Context, payload string) (string, error) {
	id := str(parse(payload), "match_id", 128)
	if id == "" {
		return refuse("INVALID_MATCH")
	}
	res, err := s.NK.MatchSignal(c.context(), id, `{"action":"FORCE_FINISH"}`)
	if err != nil {
		return refuse("MATCH_NOT_FOUND")
	}
	return ok(map[string]any{"result": res})
}

func (s *Service) adminDeleteAccount(c *Context, payload string) (string, error) {
	uid := str(parse(payload), "user_id", 64)
	if !game.ValidUUID(uid) {
		return refuse("INVALID_USER")
	}
	if err := s.NK.AccountDelete(c.context(), uid); err != nil {
		return "", fail(err.Error())
	}
	return ok(map[string]any{})
}
