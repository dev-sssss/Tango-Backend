package rpc

import (
	"time"

	"github.com/dev-sssss/tango_backend/server/internal/game"
)

// Lucky spin: one free spin a day plus a few more after a rewarded ad. The
// server picks the prize; the client only turns the wheel to the slice it is
// told. The day turns over at local midnight of SpinDayOffsetMin (India by
// default).

// SpinPrize is one slice of the wheel.
type SpinPrize struct {
	Coins    int64 `json:"coins"`
	Diamonds int64 `json:"diamonds"`
	Weight   int   `json:"weight"`
}

func defaultSpinPrizes() []SpinPrize {
	return []SpinPrize{
		{Coins: 100, Weight: 22}, {Coins: 250, Weight: 20}, {Diamonds: 5, Weight: 10}, {Coins: 500, Weight: 14},
		{Coins: 150, Weight: 18}, {Coins: 1000, Weight: 7}, {Diamonds: 10, Weight: 5}, {Coins: 2500, Weight: 4},
	}
}

func (cfg AppConfig) spinDay(nowMs int64) (day string, nextMs int64) {
	off := time.Duration(cfg.SpinDayOffsetMin) * time.Minute
	local := time.UnixMilli(nowMs).UTC().Add(off)
	start := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC)
	return local.Format("2006-01-02"), start.Add(24 * time.Hour).Add(-off).UnixMilli()
}

type spinState struct {
	free, ads, lastAdMs int64
	version             string
}

func (s *Service) spinState(c *Context, day string) (spinState, error) {
	row, err := s.read1(c.context(), "spin", "daily", c.UserID)
	st := spinState{version: "*"}
	if err != nil || row == nil {
		return st, err
	}
	st.version = row.Version
	st.lastAdMs = num(row.Value, "last_ad_ms", 0)
	if row.Value["day"] == day {
		st.free, st.ads = num(row.Value, "free", 0), num(row.Value, "ads", 0)
	}
	return st, nil
}

func (s *Service) spinView(cfg AppConfig, st spinState, nextMs, now int64) map[string]any {
	prizes := make([]map[string]any, 0, len(cfg.SpinPrizes))
	for _, p := range cfg.SpinPrizes {
		prizes = append(prizes, map[string]any{"coins": p.Coins, "diamonds": p.Diamonds})
	}
	adWait := int64(0)
	if w := st.lastAdMs + int64(cfg.AdCooldownSec)*1000 - now; w > 0 {
		adWait = w
	}
	return map[string]any{"prizes": prizes, "free_left": max(0, int64(cfg.SpinFreePerDay)-st.free),
		"ad_left": max(0, int64(cfg.SpinAdPerDay)-st.ads), "ad_wait_ms": adWait, "next_reset_ms": nextMs, "server_time_ms": now}
}

func (s *Service) spinStatus(c *Context, payload string) (string, error) {
	cfg := s.loadAppConfig(c.context())
	now := s.NK.NowMs()
	day, next := cfg.spinDay(now)
	st, err := s.spinState(c, day)
	if err != nil {
		return "", err
	}
	return ok(s.spinView(cfg, st, next, now))
}

// spinPlay spins once. Payload {"ad": true} uses an ad spin (after the free one).
func (s *Service) spinPlay(c *Context, payload string) (string, error) {
	ctx := c.context()
	if reason := s.blocked(c, "spin"); reason != "" {
		return refuse("MAINTENANCE")
	}
	if _, err := s.ensureProfile(ctx, c.UserID, c.Username); err != nil {
		return "", err
	}
	cfg := s.loadAppConfig(ctx)
	if len(cfg.SpinPrizes) == 0 {
		return refuse("UNAVAILABLE")
	}
	useAd := parse(payload)["ad"] == true
	for attempt := 0; attempt < 3; attempt++ {
		now := s.NK.NowMs()
		day, next := cfg.spinDay(now)
		st, err := s.spinState(c, day)
		if err != nil {
			return "", err
		}
		if useAd {
			if st.free < int64(cfg.SpinFreePerDay) {
				useAd = false // the free spin comes first
			} else if st.ads >= int64(cfg.SpinAdPerDay) {
				return refuse("DAILY_LIMIT")
			} else if now-st.lastAdMs < int64(cfg.AdCooldownSec)*1000 {
				return refuse("TOO_SOON")
			}
		} else if st.free >= int64(cfg.SpinFreePerDay) {
			return refuse("NO_FREE_SPIN")
		}
		total := 0
		for _, p := range cfg.SpinPrizes {
			total += max(0, p.Weight)
		}
		if total <= 0 {
			return refuse("UNAVAILABLE")
		}
		roll, slice := s.NK.RandInt(1, total), 0
		for i, p := range cfg.SpinPrizes {
			roll -= max(0, p.Weight)
			if roll <= 0 {
				slice = i
				break
			}
		}
		prize := cfg.SpinPrizes[slice]
		if useAd {
			st.ads++
			st.lastAdMs = now
		} else {
			st.free++
		}
		change := map[string]int64{}
		if prize.Coins > 0 {
			change["coins"] = prize.Coins
		}
		if prize.Diamonds > 0 {
			change["diamonds"] = prize.Diamonds
		}
		err = s.NK.MultiUpdate(ctx,
			[]*game.Write{{Collection: "spin", Key: "daily", UserID: c.UserID, Version: st.version,
				Value: map[string]any{"day": day, "free": st.free, "ads": st.ads, "last_ad_ms": st.lastAdMs}}},
			[]*game.WalletUpdate{{UserID: c.UserID, Changeset: change, Metadata: map[string]any{"reason": "lucky_spin", "slice": slice, "ad": useAd}}}, true)
		if err == nil {
			w, err := s.wallet(ctx, c.UserID)
			if err != nil {
				return "", err
			}
			out := s.spinView(cfg, st, next, now)
			out["slice"], out["coins"], out["diamonds"], out["wallet"] = slice, prize.Coins, prize.Diamonds, w
			return ok(out)
		}
		if !isVersionErr(err) {
			return "", fail(err.Error())
		}
	}
	return refuse("BUSY")
}
