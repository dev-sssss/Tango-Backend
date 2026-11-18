package rpc

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/dev-sssss/tango_backend/server/internal/game"
	"github.com/dev-sssss/tango_backend/server/internal/game/tango"
)

var nameRe = regexp.MustCompile(`^[\p{L}\p{N} _.\-]{3,16}$`)

// ensureProfile returns the caller's profile, creating it (and granting the
// starting wallet exactly once, in the same transaction) on first use.
func (s *Service) ensureProfile(ctx context.Context, userID, username string) (*game.Profile, error) {
	for attempt := 0; attempt < 3; attempt++ {
		p, ver, err := game.LoadProfile(ctx, s.NK, userID)
		if err != nil {
			return nil, fail(err.Error())
		}
		if ver != "" {
			return p, nil
		}
		cfg := s.loadAppConfig(ctx)
		p.Name = username
		if acct, err := s.NK.AccountGet(ctx, userID); err == nil && acct.DisplayName != "" {
			p.Name = acct.DisplayName
		}
		p.CreatedMs = s.NK.NowMs()
		err = s.NK.MultiUpdate(ctx,
			[]*game.Write{{Collection: game.ProfileCollection, Key: game.ProfileKey, UserID: userID, Value: p.Map(), Version: "*", PermissionRead: 2}},
			[]*game.WalletUpdate{{UserID: userID, Changeset: map[string]int64{"coins": cfg.StartingCoins, "diamonds": cfg.StartingDiamonds},
				Metadata: map[string]any{"reason": "starting_wallet"}}}, true)
		if err == nil {
			// Every new player gets a permanent public player ID straight away.
			if soc, ok := game.SocialFrom(s.NK); ok {
				_, _ = soc.PlayerID(ctx, userID)
			}
			return p, nil
		}
		if !isVersionErr(err) {
			return nil, fail(err.Error())
		}
	}
	return nil, fail("profile busy, retry")
}

func (s *Service) getProfile(c *Context, payload string) (string, error) {
	ctx := c.context()
	p, err := s.ensureProfile(ctx, c.UserID, c.Username)
	if err != nil {
		return "", err
	}
	w, err := s.wallet(ctx, c.UserID)
	if err != nil {
		return "", err
	}
	out := map[string]any{"user_id": c.UserID, "username": c.Username, "profile": p.Map(), "level": tango.LevelFor(p.Trophies),
		"name_change_cost":       s.loadAppConfig(ctx).NameChangeCoins,
		"trophies_to_next_level": tango.TrophiesPerLevel - p.Trophies%tango.TrophiesPerLevel, "wallet": w}
	if soc, ok := game.SocialFrom(s.NK); ok {
		if id, err := soc.PlayerID(ctx, c.UserID); err == nil {
			out["player_id"] = id
		}
	}
	return ok(out)
}

func (s *Service) updateProfile(c *Context, payload string) (string, error) {
	ctx := c.context()
	data := parse(payload)
	if _, err := s.ensureProfile(ctx, c.UserID, c.Username); err != nil {
		return "", err
	}
	name, hasName := data["name"].(string)
	name = strings.TrimSpace(name)
	if hasName && !nameRe.MatchString(name) {
		return refuse("INVALID_NAME")
	}
	cfg := s.loadAppConfig(ctx)
	for attempt := 0; attempt < 5; attempt++ {
		p, ver, err := game.LoadProfile(ctx, s.NK, c.UserID)
		if err != nil {
			return "", fail(err.Error())
		}
		var cost int64
		renamed := hasName && name != p.Name
		if renamed {
			// The first name a player picks is free; every later change costs coins.
			if p.NameChanges > 0 {
				cost = cfg.NameChangeCoins
			}
			p.Name = name
			p.NameChanges++
		}
		if data["tutorial_done"] == true {
			p.Tutorial = true
		}
		if ver == "" {
			ver = "*"
		}
		writes := []*game.Write{{Collection: game.ProfileCollection, Key: game.ProfileKey, UserID: c.UserID, Value: p.Map(), Version: ver, PermissionRead: 2}}
		var wallets []*game.WalletUpdate
		if cost > 0 {
			wallets = []*game.WalletUpdate{{UserID: c.UserID, Changeset: map[string]int64{"coins": -cost}, Metadata: map[string]any{"reason": "name_change"}}}
		}
		err = s.NK.MultiUpdate(ctx, writes, wallets, true)
		if err == nil {
			if renamed {
				_ = s.NK.AccountUpdate(ctx, c.UserID, "", &name, nil)
			}
			out := map[string]any{"profile": p.Map(), "name_change_cost": cfg.NameChangeCoins, "charged": cost}
			if w, werr := s.wallet(ctx, c.UserID); werr == nil {
				out["wallet"] = w
			}
			return ok(out)
		}
		if isNegative(err) {
			return enc(map[string]any{"success": false, "error": "INSUFFICIENT_COINS",
				"message": fmt.Sprintf("Changing your name costs %d coins.", cost)}), nil
		}
		if !isVersionErr(err) {
			return "", fail(err.Error())
		}
	}
	return refuse("BUSY")
}

func (s *Service) deleteOwnAccount(c *Context, payload string) (string, error) {
	if parse(payload)["confirm"] != true {
		return refuse("CONFIRM_REQUIRED")
	}
	if err := s.NK.AccountDelete(c.context(), c.UserID); err != nil {
		return "", fail(err.Error())
	}
	return ok(map[string]any{})
}
