package game

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Port of announcements.lua with one change: the policy document is cached
// for a few seconds instead of being fetched synchronously on every match
// join and RPC. Failure still fails closed.

const announcementsOrigin = "http://announcements:8789/announcements.json"

const MaintenanceUnavailable = "Service temporarily unavailable. Please try again shortly."

type Announcements struct {
	TTL time.Duration

	mu      sync.Mutex
	doc     map[string]any
	err     error
	fetched time.Time
	key     string
}

var errAnnouncements = errors.New("announcements unavailable")

func (a *Announcements) Get(ctx context.Context, nk NK) (map[string]any, error) {
	env := nk.Env()
	url := env["ANNOUNCEMENTS_URL"]
	if url == "" {
		return map[string]any{"schema_version": 1, "announcements": map[string]any{}, "maintenance": map[string]any{}, "blocked_features": map[string]any{}}, nil
	}
	ttl := a.TTL
	if ttl == 0 {
		ttl = 5 * time.Second
	}
	key := url + "|" + env["DEPLOYMENT_ENVIRONMENT"]
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.key == key && time.Since(a.fetched) < ttl {
		return a.doc, a.err
	}
	a.doc, a.err = fetchAnnouncements(ctx, nk, url, env["DEPLOYMENT_ENVIRONMENT"])
	a.fetched, a.key = time.Now(), key
	return a.doc, a.err
}

func fetchAnnouncements(ctx context.Context, nk NK, url, env string) (map[string]any, error) {
	if url != announcementsOrigin {
		return nil, errors.New("Invalid announcement origin")
	}
	res, err := nk.HTTPRequest(ctx, url, "GET", map[string]string{}, "", 1500)
	if err != nil || res.Code != 200 || len(res.Body) > 262144 {
		return nil, errors.New("Announcements unavailable")
	}
	v, err := Decode(res.Body)
	if err != nil {
		return nil, err
	}
	d, ok := v.(map[string]any)
	if !ok || d["schema_version"] != float64(1) {
		return nil, errors.New("Invalid announcements")
	}
	switch d["maintenance"].(type) {
	case map[string]any, []any:
	default:
		return nil, errors.New("Invalid announcements")
	}
	if env == "" {
		if d["environment"] != nil {
			return nil, errors.New("Wrong announcement environment")
		}
	} else if e, _ := d["environment"].(string); e != env {
		return nil, errors.New("Wrong announcement environment")
	}
	return d, nil
}

// Blocked returns the maintenance message when feature is blocked, or "".
func (a *Announcements) Blocked(ctx context.Context, nk NK, feature string) string {
	if nk.Env()["ANNOUNCEMENTS_URL"] == "" {
		return ""
	}
	d, err := a.Get(ctx, nk)
	if err != nil {
		return MaintenanceUnavailable
	}
	now := float64(nk.Unix())
	windows, _ := d["maintenance"].([]any)
	for _, w := range windows {
		wm, ok := w.(map[string]any)
		if !ok {
			continue
		}
		start, ok1 := wm["start"].(float64)
		end, ok2 := wm["end"].(float64)
		if !ok1 || !ok2 || now < start || now >= end {
			continue
		}
		features, _ := wm["features"].([]any)
		for _, f := range features {
			if f == feature {
				if msg, ok := wm["message"].(string); ok {
					return msg
				}
				// Lua returns w.message which may be nil (not blocked) or
				// a non-string; treat non-string as a generic block.
				if wm["message"] == nil {
					return ""
				}
				return MaintenanceUnavailable
			}
		}
	}
	return ""
}

// GetDocument implements rpc_get_announcements.
func (a *Announcements) GetDocument(ctx context.Context, nk NK) (map[string]any, error) {
	return a.Get(ctx, nk)
}
