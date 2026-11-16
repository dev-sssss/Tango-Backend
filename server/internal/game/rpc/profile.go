package rpc

import (
	"context"
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
	return ok(map[string]any{"user_id": c.UserID, "username": c.Username, "profile": p.Map(), "level": tango.LevelFor(p.Trophies),
		"trophies_to_next_level": tango.TrophiesPerLevel - p.Trophies%tango.TrophiesPerLevel, "wallet": w})
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
	p, err := game.UpdateProfile(ctx, s.NK, c.UserID, func(p *game.Profile) error {
		if hasName {
			p.Name = name
		}
		if data["tutorial_done"] == true {
			p.Tutorial = true
		}
		return nil
	})
	if err != nil {
		return "", fail(err.Error())
	}
	if hasName {
		_ = s.NK.AccountUpdate(ctx, c.UserID, "", &name, nil)
	}
	return ok(map[string]any{"profile": p.Map()})
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
