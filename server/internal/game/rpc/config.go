package rpc

import (
	"context"
	_ "embed"
	"encoding/json"

	"github.com/dev-sssss/tango_backend/server/internal/game"
)

// App config and shop catalog live in system storage so they can be changed
// at runtime with the admin RPCs; the embedded defaults apply until then.

//go:embed catalog_default.json
var defaultCatalogJSON []byte

const (
	configCollection = "config"
	appConfigKey     = "app"
	catalogKey       = "shop_catalog"
)

// AppConfig is what the client reads at start-up.
type AppConfig struct {
	EntryFees          []int64  `json:"entry_fees"`
	StartingCoins      int64    `json:"starting_coins"`
	StartingDiamonds   int64    `json:"starting_diamonds"`
	AdRewardMin        int64    `json:"ad_reward_min"`
	AdRewardMax        int64    `json:"ad_reward_max"`
	AdDailyCap         int      `json:"ad_daily_cap"`
	AdCooldownSec      int      `json:"ad_cooldown_sec"`
	TurnSecondsOptions []int    `json:"turn_seconds_options"`
	LobbyWaitSeconds   int      `json:"lobby_wait_seconds"`
	MinClientVersion   string   `json:"min_client_version"`
	Formats            []string `json:"formats"`
	Modes              []string `json:"modes"`
}

func defaultAppConfig() AppConfig {
	return AppConfig{
		EntryFees: []int64{200, 400, 1000, 2000, 5000}, StartingCoins: 2000, StartingDiamonds: 20,
		AdRewardMin: 250, AdRewardMax: 300, AdDailyCap: 10, AdCooldownSec: 30,
		TurnSecondsOptions: []int{10, 15, 20, 30}, LobbyWaitSeconds: 15, MinClientVersion: "0.0.0",
		Formats: []string{"1v1", "2v2", "4v4", "4p", "8p"}, Modes: []string{"classic", "no_mercy"},
	}
}

func (s *Service) loadAppConfig(ctx context.Context) AppConfig {
	cfg := defaultAppConfig()
	row, err := s.read1(ctx, configCollection, appConfigKey, "")
	if err != nil || row == nil {
		return cfg
	}
	b, _ := json.Marshal(row.Value)
	_ = json.Unmarshal(b, &cfg) // stored fields override defaults
	return cfg
}

func (c AppConfig) feeAllowed(fee int64) bool {
	for _, f := range c.EntryFees {
		if f == fee {
			return true
		}
	}
	return false
}

// CatalogItem is a cosmetic sold in the shop.
type CatalogItem struct {
	ID       string           `json:"id"`
	Category string           `json:"category"` // avatar, frame, badge, card_back, table, emote, effect
	Name     string           `json:"name"`
	Price    map[string]int64 `json:"price"` // {"coins":5000} or {"diamonds":50}
	Rarity   string           `json:"rarity,omitempty"`
	Default  bool             `json:"default,omitempty"` // owned by everyone
}

// IAPProduct maps a Google Play product id to currency.
type IAPProduct struct {
	ProductID string `json:"product_id"`
	Coins     int64  `json:"coins"`
	Diamonds  int64  `json:"diamonds"`
	Label     string `json:"label,omitempty"`
}

// Catalog is the whole shop.
type Catalog struct {
	Version     int           `json:"version"`
	Items       []CatalogItem `json:"items"`
	IAPProducts []IAPProduct  `json:"iap_products"`
}

func (c *Catalog) item(id string) *CatalogItem {
	for i := range c.Items {
		if c.Items[i].ID == id {
			return &c.Items[i]
		}
	}
	return nil
}

func (c *Catalog) product(id string) *IAPProduct {
	for i := range c.IAPProducts {
		if c.IAPProducts[i].ProductID == id {
			return &c.IAPProducts[i]
		}
	}
	return nil
}

func (s *Service) loadCatalog(ctx context.Context) *Catalog {
	cat := &Catalog{}
	if row, err := s.read1(ctx, configCollection, catalogKey, ""); err == nil && row != nil {
		b, _ := json.Marshal(row.Value)
		if json.Unmarshal(b, cat) == nil && len(cat.Items) > 0 {
			return cat
		}
	}
	cat = &Catalog{}
	_ = json.Unmarshal(defaultCatalogJSON, cat)
	return cat
}

// DefaultCatalog parses the embedded catalog (for tests and validation).
func DefaultCatalog() (*Catalog, error) {
	cat := &Catalog{}
	err := json.Unmarshal(defaultCatalogJSON, cat)
	return cat, err
}

var _ = game.SystemUserID
