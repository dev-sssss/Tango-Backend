package game

import (
	"context"
	"errors"
)

// Profile is a player's Tango profile, kept in storage
// (collection "profile", key "main", owned by the player, read-only to
// clients). Equipped cosmetics are item ids from the shop catalog.
type Profile struct {
	Name      string `json:"name"`
	Avatar    string `json:"avatar"`
	Frame     string `json:"frame"`
	Badge     string `json:"badge"`
	CardBack  string `json:"card_back"`
	TableSkin string `json:"table"`
	Effect    string `json:"effect"`
	Trophies  int    `json:"trophies"`
	Wins      int    `json:"wins"`
	Losses    int    `json:"losses"`
	Games     int    `json:"games"`
	CreatedMs int64  `json:"created_ms"`
	Tutorial  bool   `json:"tutorial_done"`
	// NameChanges counts names the player chose. The first is free; later
	// changes cost coins (AppConfig.NameChangeCoins).
	NameChanges int `json:"name_changes"`
}

const (
	ProfileCollection = "profile"
	ProfileKey        = "main"
)

// Default cosmetics every player owns (the same free items as the Unity
// game's shop: Nova, the gold ring, the plain plate, the compass back, the
// sunset table and the classic card effects).
var DefaultLoadout = map[string]string{
	"avatar": "avatar_nova", "frame": "frame_gold", "badge": "badge_none", "card_back": "back_crimson", "table": "table_royal",
	"effect": "effect_classic",
}

func (p *Profile) toMap() map[string]any {
	return map[string]any{"name": p.Name, "avatar": p.Avatar, "frame": p.Frame, "badge": p.Badge, "card_back": p.CardBack,
		"table": p.TableSkin, "effect": p.Effect, "trophies": p.Trophies, "wins": p.Wins, "losses": p.Losses, "games": p.Games,
		"created_ms": p.CreatedMs, "tutorial_done": p.Tutorial, "name_changes": p.NameChanges}
}

// Map returns the profile as a JSON-ready map.
func (p *Profile) Map() map[string]any { return p.toMap() }

func intOf(v any) int {
	f, _ := ToNumber(v)
	return int(f)
}

func strOf(v any, def string) string {
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	return def
}

// ProfileFromMap parses a stored profile, filling defaults.
func ProfileFromMap(m map[string]any) *Profile {
	return &Profile{
		Name: strOf(m["name"], ""), Avatar: strOf(m["avatar"], DefaultLoadout["avatar"]), Frame: strOf(m["frame"], DefaultLoadout["frame"]),
		Badge: strOf(m["badge"], DefaultLoadout["badge"]), CardBack: strOf(m["card_back"], DefaultLoadout["card_back"]),
		TableSkin: strOf(m["table"], DefaultLoadout["table"]), Effect: strOf(m["effect"], DefaultLoadout["effect"]), Trophies: intOf(m["trophies"]), Wins: intOf(m["wins"]),
		Losses: intOf(m["losses"]), Games: intOf(m["games"]), CreatedMs: int64(intOf(m["created_ms"])), Tutorial: m["tutorial_done"] == true,
		NameChanges: intOf(m["name_changes"]),
	}
}

// LoadProfile reads a player's profile. A missing profile returns a default
// one with an empty version (so the first write creates it).
func LoadProfile(ctx context.Context, nk NK, userID string) (*Profile, string, error) {
	rows, err := nk.StorageRead(ctx, []ReadID{{Collection: ProfileCollection, Key: ProfileKey, UserID: userID}})
	if err != nil {
		return nil, "", err
	}
	if len(rows) == 0 {
		return ProfileFromMap(map[string]any{}), "", nil
	}
	return ProfileFromMap(rows[0].Value), rows[0].Version, nil
}

// SaveProfile writes a profile with optimistic concurrency. version "" means
// "must not exist yet".
func SaveProfile(ctx context.Context, nk NK, userID string, p *Profile, version string) error {
	if version == "" {
		version = "*"
	}
	_, err := nk.StorageWrite(ctx, []*Write{{Collection: ProfileCollection, Key: ProfileKey, UserID: userID, Value: p.toMap(),
		Version: version, PermissionRead: 2, PermissionWrite: 0}})
	return err
}

// UpdateProfile applies fn with retries on concurrent writes.
func UpdateProfile(ctx context.Context, nk NK, userID string, fn func(p *Profile) error) (*Profile, error) {
	for attempt := 0; attempt < 5; attempt++ {
		p, ver, err := LoadProfile(ctx, nk, userID)
		if err != nil {
			return nil, err
		}
		if err := fn(p); err != nil {
			return nil, err
		}
		err = SaveProfile(ctx, nk, userID, p, ver)
		if err == nil {
			return p, nil
		}
		if !errors.Is(err, ErrStorageVersion) {
			return nil, err
		}
	}
	return nil, ErrStorageVersion
}
