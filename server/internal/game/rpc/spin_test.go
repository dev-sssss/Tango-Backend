package rpc

import "testing"

func TestLuckySpin(t *testing.T) {
	s, nk, c := setup(nil)
	call(t, s, c, "tango.profile.get", "")
	st := call(t, s, c, "tango.spin.status", "")
	if st["free_left"].(float64) != 1 || len(st["prizes"].([]any)) != 8 {
		t.Fatalf("status %v", st)
	}
	before := nk.Wallet(uid)
	r := call(t, s, c, "tango.spin.play", "{}")
	if r["success"] != true {
		t.Fatalf("spin %v", r)
	}
	slice := int(r["slice"].(float64))
	prize := defaultSpinPrizes()[slice]
	after := nk.Wallet(uid)
	if after["coins"]-before["coins"] != prize.Coins || after["diamonds"]-before["diamonds"] != prize.Diamonds {
		t.Fatalf("slice %d paid %v -> %v", slice, before, after)
	}
	if r := call(t, s, c, "tango.spin.play", "{}"); r["error"] != "NO_FREE_SPIN" {
		t.Fatalf("second free spin: %v", r)
	}
	// Ad spins: three a day, 30 s apart.
	for i := 0; i < 3; i++ {
		nk.Advance(31_000)
		if r := call(t, s, c, "tango.spin.play", `{"ad":true}`); r["success"] != true {
			t.Fatalf("ad spin %d: %v", i, r)
		}
	}
	nk.Advance(31_000)
	if r := call(t, s, c, "tango.spin.play", `{"ad":true}`); r["error"] != "DAILY_LIMIT" {
		t.Fatalf("fourth ad spin: %v", r)
	}
	// Next day: a new free spin.
	nk.Advance(24 * 3600 * 1000)
	if st := call(t, s, c, "tango.spin.status", ""); st["free_left"].(float64) != 1 || st["ad_left"].(float64) != 3 {
		t.Fatalf("next day %v", st)
	}
}

func TestSpinDayStartsAtIndianMidnight(t *testing.T) {
	cfg := defaultAppConfig()
	// 2026-11-17 18:29:00 UTC is 23:59 in India; 18:31 UTC is the next day there.
	d1, next := cfg.spinDay(1794940140000)
	d2, _ := cfg.spinDay(1794940260000)
	if d1 == d2 || next != 1794940200000 {
		t.Fatalf("%s %s next %d", d1, d2, next)
	}
}

func TestLeaderboardIDs(t *testing.T) {
	s, _, c := setup(nil)
	for _, id := range []string{"trophies", "weekly_trophies", "wins"} {
		if r := call(t, s, c, "tango.leaderboard.get", `{"id":"`+id+`"}`); r["id"] != id {
			t.Fatalf("%s -> %v", id, r)
		}
	}
	// The in-memory runtime keeps no friends: the board holds just the caller.
	r := call(t, s, c, "tango.leaderboard.get", `{"id":"friends"}`)
	if recs := r["records"].([]any); r["id"] != "friends" || len(recs) != 1 {
		t.Fatalf("friends board %v", r)
	}
}
