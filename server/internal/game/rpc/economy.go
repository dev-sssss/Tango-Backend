package rpc

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	"github.com/dev-sssss/tango_backend/server/internal/game"
)

// Coins and diamonds live in the account wallet; every change goes through
// the wallet ledger. Purchases, ad rewards and IAP credits are each one
// transaction together with the storage record that makes them idempotent.

const (
	inventoryCollection = "inventory"
	inventoryKey        = "items"
)

var equipField = map[string]func(p *game.Profile, id string){
	"avatar":    func(p *game.Profile, id string) { p.Avatar = id },
	"frame":     func(p *game.Profile, id string) { p.Frame = id },
	"badge":     func(p *game.Profile, id string) { p.Badge = id },
	"card_back": func(p *game.Profile, id string) { p.CardBack = id },
	"table":     func(p *game.Profile, id string) { p.TableSkin = id },
	"effect":    func(p *game.Profile, id string) { p.Effect = id },
}

func isNegative(err error) bool { return err != nil && strings.Contains(err.Error(), "negative") }

func (s *Service) getWallet(c *Context, payload string) (string, error) {
	w, err := s.wallet(c.context(), c.UserID)
	if err != nil {
		return "", err
	}
	return ok(map[string]any{"wallet": w})
}

// owned returns the caller's purchased item ids and the inventory version.
func (s *Service) owned(c *Context) (map[string]any, string, error) {
	row, err := s.read1(c.context(), inventoryCollection, inventoryKey, c.UserID)
	if err != nil {
		return nil, "", err
	}
	if row == nil {
		return map[string]any{}, "", nil
	}
	items, _ := row.Value["items"].(map[string]any)
	if items == nil {
		items = map[string]any{}
	}
	return items, row.Version, nil
}

func (s *Service) getCatalog(c *Context, payload string) (string, error) {
	cat := s.loadCatalog(c.context())
	items, _, err := s.owned(c)
	if err != nil {
		return "", err
	}
	list := make([]map[string]any, 0, len(cat.Items))
	for _, it := range cat.Items {
		_, have := items[it.ID]
		list = append(list, map[string]any{"id": it.ID, "category": it.Category, "name": it.Name, "price": it.Price,
			"rarity": it.Rarity, "owned": have || it.Default})
	}
	return ok(map[string]any{"version": cat.Version, "items": list, "iap_products": cat.IAPProducts})
}

func (s *Service) getInventory(c *Context, payload string) (string, error) {
	ctx := c.context()
	cat := s.loadCatalog(ctx)
	items, _, err := s.owned(c)
	if err != nil {
		return "", err
	}
	owned := []string{}
	for _, it := range cat.Items {
		if _, have := items[it.ID]; have || it.Default {
			owned = append(owned, it.ID)
		}
	}
	p, err := s.ensureProfile(ctx, c.UserID, c.Username)
	if err != nil {
		return "", err
	}
	return ok(map[string]any{"owned": owned, "equipped": map[string]any{"avatar": p.Avatar, "frame": p.Frame, "badge": p.Badge,
		"card_back": p.CardBack, "table": p.TableSkin, "effect": p.Effect}})
}

func (s *Service) purchaseItem(c *Context, payload string) (string, error) {
	ctx := c.context()
	id := str(parse(payload), "item_id", 64)
	cat := s.loadCatalog(ctx)
	it := cat.item(id)
	if it == nil {
		return refuse("UNKNOWN_ITEM")
	}
	if it.Default {
		return refuse("ALREADY_OWNED")
	}
	cost := map[string]int64{}
	for cur, amount := range it.Price {
		if (cur == "coins" || cur == "diamonds") && amount > 0 {
			cost[cur] = -amount
		}
	}
	if len(cost) == 0 {
		return refuse("NOT_FOR_SALE")
	}
	for attempt := 0; attempt < 4; attempt++ {
		items, ver, err := s.owned(c)
		if err != nil {
			return "", err
		}
		if _, have := items[id]; have {
			return refuse("ALREADY_OWNED")
		}
		items[id] = s.NK.NowMs()
		if ver == "" {
			ver = "*"
		}
		err = s.NK.MultiUpdate(ctx,
			[]*game.Write{{Collection: inventoryCollection, Key: inventoryKey, UserID: c.UserID, Value: map[string]any{"items": items}, Version: ver, PermissionRead: 1}},
			[]*game.WalletUpdate{{UserID: c.UserID, Changeset: cost, Metadata: map[string]any{"reason": "shop_purchase", "item_id": id}}}, true)
		if err == nil {
			w, err := s.wallet(ctx, c.UserID)
			if err != nil {
				return "", err
			}
			return ok(map[string]any{"item_id": id, "wallet": w})
		}
		if isNegative(err) {
			return refuse("INSUFFICIENT_FUNDS")
		}
		if !isVersionErr(err) {
			return "", fail(err.Error())
		}
	}
	return refuse("BUSY")
}

func (s *Service) equipItem(c *Context, payload string) (string, error) {
	ctx := c.context()
	data := parse(payload)
	category, id := str(data, "category", 16), str(data, "item_id", 64)
	set, okCat := equipField[category]
	if !okCat {
		return refuse("INVALID_CATEGORY")
	}
	it := s.loadCatalog(ctx).item(id)
	if it == nil || it.Category != category {
		return refuse("UNKNOWN_ITEM")
	}
	if !it.Default {
		items, _, err := s.owned(c)
		if err != nil {
			return "", err
		}
		if _, have := items[id]; !have {
			return refuse("NOT_OWNED")
		}
	}
	if _, err := s.ensureProfile(ctx, c.UserID, c.Username); err != nil {
		return "", err
	}
	p, err := game.UpdateProfile(ctx, s.NK, c.UserID, func(p *game.Profile) error { set(p, id); return nil })
	if err != nil {
		return "", fail(err.Error())
	}
	return ok(map[string]any{"profile": p.Map()})
}

// ---------------------------------------------------------------- ads

func (s *Service) claimAdReward(c *Context, payload string) (string, error) {
	ctx := c.context()
	if reason := s.blocked(c, "ads"); reason != "" {
		return refuse("MAINTENANCE")
	}
	cfg := s.loadAppConfig(ctx)
	now := s.NK.NowMs()
	day := time.UnixMilli(now).UTC().Format("2006-01-02")
	for attempt := 0; attempt < 3; attempt++ {
		row, err := s.read1(ctx, "ads", "daily", c.UserID)
		if err != nil {
			return "", err
		}
		count, last, ver := int64(0), int64(0), "*"
		if row != nil {
			ver = row.Version
			if row.Value["day"] == day {
				count = num(row.Value, "count", 0)
			}
			last = num(row.Value, "last_ms", 0)
		}
		if count >= int64(cfg.AdDailyCap) {
			return refuse("DAILY_LIMIT")
		}
		if now-last < int64(cfg.AdCooldownSec)*1000 {
			return refuse("TOO_SOON")
		}
		reward := int64(s.NK.RandInt(int(cfg.AdRewardMin), int(cfg.AdRewardMax)))
		err = s.NK.MultiUpdate(ctx,
			[]*game.Write{{Collection: "ads", Key: "daily", UserID: c.UserID, Value: map[string]any{"day": day, "count": count + 1, "last_ms": now}, Version: ver}},
			[]*game.WalletUpdate{{UserID: c.UserID, Changeset: map[string]int64{"coins": reward}, Metadata: map[string]any{"reason": "ad_reward"}}}, true)
		if err == nil {
			w, err := s.wallet(ctx, c.UserID)
			if err != nil {
				return "", err
			}
			return ok(map[string]any{"coins": reward, "remaining_today": int64(cfg.AdDailyCap) - count - 1, "wallet": w})
		}
		if !isVersionErr(err) {
			return "", fail(err.Error())
		}
	}
	return refuse("BUSY")
}

// ---------------------------------------------------------------- IAP

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// verifyGooglePurchase credits a Google Play purchase once. Payload:
// {"receipt": "<the purchase JSON string from Google Play Billing>"}.
func (s *Service) verifyGooglePurchase(c *Context, payload string) (string, error) {
	ctx := c.context()
	env := s.NK.Env()
	if env["GOOGLE_IAP_ENABLED"] != "true" {
		return refuse("PURCHASES_UNAVAILABLE")
	}
	receiptStr := str(parse(payload), "receipt", 16384)
	receipt, okR := game.DecodeObject(receiptStr)
	token, _ := receipt["purchaseToken"].(string)
	pid, _ := receipt["productId"].(string)
	if !okR || token == "" || (env["TANGO_ANDROID_PACKAGE"] != "" && receipt["packageName"] != env["TANGO_ANDROID_PACKAGE"]) {
		return refuse("INVALID_RECEIPT")
	}
	prod := s.loadCatalog(ctx).product(pid)
	if prod == nil {
		return refuse("UNKNOWN_PRODUCT")
	}
	key := sha256Hex(token)
	if row, err := s.read1(ctx, "iap_receipts", key, ""); err != nil {
		return "", err
	} else if row != nil {
		if row.Value["user_id"] == c.UserID {
			w, _ := s.wallet(ctx, c.UserID)
			return ok(map[string]any{"already_credited": true, "wallet": w})
		}
		return refuse("RECEIPT_USED")
	}
	ps, err := s.NK.PurchaseValidateGoogle(ctx, c.UserID, receiptStr, true)
	if err != nil || len(ps) == 0 {
		return refuse("VERIFICATION_FAILED")
	}
	p := ps[0]
	if p.ProductID != pid || p.RefundTime > 0 {
		return refuse("INVALID_PURCHASE")
	}
	if provider, okP := game.DecodeObject(p.ProviderResponse); okP {
		if st, has := provider["purchaseState"]; has && st != 0.0 {
			return refuse("PURCHASE_NOT_PAID")
		}
		if acc, has := provider["obfuscatedExternalAccountId"].(string); has && acc != "" && acc != sha256Hex(c.UserID) {
			return refuse("PURCHASE_ACCOUNT_MISMATCH")
		}
	}
	credit := map[string]int64{}
	if prod.Coins > 0 {
		credit["coins"] = prod.Coins
	}
	if prod.Diamonds > 0 {
		credit["diamonds"] = prod.Diamonds
	}
	err = s.NK.MultiUpdate(ctx,
		[]*game.Write{{Collection: "iap_receipts", Key: key, Value: map[string]any{"user_id": c.UserID, "product_id": pid, "at": s.NK.NowMs()}, Version: "*"}},
		[]*game.WalletUpdate{{UserID: c.UserID, Changeset: credit, Metadata: map[string]any{"reason": "iap", "product_id": pid}}}, true)
	if isVersionErr(err) {
		return refuse("RECEIPT_USED")
	}
	if err != nil {
		return "", fail(err.Error())
	}
	w, _ := s.wallet(ctx, c.UserID)
	return ok(map[string]any{"product_id": pid, "credited": credit, "wallet": w})
}

// ---------------------------------------------------------------- dev

// devGrant is the developer shortcut (tap coins three times). It only works
// where TANGO_ALLOW_CHEATS=true, which the server refuses in production.
func (s *Service) devGrant(c *Context, payload string) (string, error) {
	env := s.NK.Env()
	if env["TANGO_ALLOW_CHEATS"] != "true" || strings.EqualFold(env["DEPLOYMENT_ENVIRONMENT"], "production") {
		return refuse("NOT_AVAILABLE")
	}
	ctx := c.context()
	if _, _, err := s.NK.WalletUpdate(ctx, c.UserID, map[string]int64{"coins": 10000, "diamonds": 1000}, map[string]any{"reason": "dev_grant"}, true); err != nil {
		return "", fail(err.Error())
	}
	w, _ := s.wallet(ctx, c.UserID)
	return ok(map[string]any{"wallet": w})
}
