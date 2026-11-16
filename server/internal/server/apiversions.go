package server

import (
	"context"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// API versioning rules
//
//  1. A published API version is never changed incompatibly. New behaviour
//     goes into a new version; old versions keep their exact contract.
//  2. A version can be marked deprecated at any time. From then on every
//     response carries Deprecation and Sunset headers (RFC 9745 / RFC 8594).
//  3. A version may only be removed when BOTH hold:
//     - its sunset date has passed, and the sunset is at least
//       MinSupportWindow after the deprecation date, and
//     - no client has called it for MinSupportWindow (from api_client_usage).
//     /admin/api-usage reports this as "removable".
//
// Clients identify themselves with "X-Tango-Client: <platform>/<version>+<build>"
// so usage can be broken down per app build. Builds that predate the header
// are tracked as "legacy".

// MinSupportWindow is how long an API version keeps working after the last
// client used it (and after its deprecation was announced).
const MinSupportWindow = 183 * 24 * time.Hour

type APIVersion struct {
	Name       string
	Introduced time.Time
	Deprecated time.Time // zero = not deprecated
	Sunset     time.Time // zero = no sunset planned
	Notes      string
}

func date(s string) time.Time {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return t
}

// apiVersionTable is the source of truth for supported API versions.
var apiVersionTable = []APIVersion{
	{Name: "v1", Introduced: date("2024-01-01"), Notes: "Nakama-compatible /v2/... and /ws (every shipped app build)"},
	{Name: "v2", Introduced: date("2026-10-01"), Notes: "/api/v2: plain JSON, refresh tokens, typed errors"},
}

type APIVersions struct {
	list []APIVersion
	by   map[string]APIVersion
}

func NewAPIVersions(list []APIVersion) *APIVersions {
	v := &APIVersions{list: list, by: map[string]APIVersion{}}
	for _, x := range list {
		v.by[x.Name] = x
	}
	return v
}

// Validate enforces the support window on the table itself.
func (v *APIVersions) Validate() []string {
	var problems []string
	for _, x := range v.list {
		if !x.Sunset.IsZero() {
			if x.Deprecated.IsZero() {
				problems = append(problems, x.Name+": sunset without deprecation date")
			} else if x.Sunset.Sub(x.Deprecated) < MinSupportWindow {
				problems = append(problems, x.Name+": sunset is less than the minimum support window after deprecation")
			}
		}
	}
	return problems
}

func (v *APIVersions) annotate(w http.ResponseWriter, name string) {
	h := w.Header()
	h.Set("X-Tango-Api-Version", name)
	x, ok := v.by[name]
	if !ok {
		return
	}
	if !x.Deprecated.IsZero() {
		h.Set("Deprecation", "@"+itoa64(x.Deprecated.Unix()))
		h.Set("Link", `</api/meta>; rel="deprecation"`)
	}
	if !x.Sunset.IsZero() {
		h.Set("Sunset", x.Sunset.UTC().Format(http.TimeFormat))
	}
}

func itoa64(n int64) string { return strconv.FormatInt(n, 10) }

// ---------------------------------------------------------------- usage

var clientTagRe = regexp.MustCompile(`^[a-z0-9_-]{1,16}/[0-9A-Za-z.+_-]{1,40}$`)

// clientTag identifies the calling app build for usage tracking.
func clientTag(r *http.Request) string {
	h := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Tango-Client")))
	if h == "" {
		return "legacy"
	}
	if !clientTagRe.MatchString(h) {
		return "invalid"
	}
	return h
}

type usageKey struct {
	version, client string
	day             string
}

// UsageTracker counts requests per API version and client build and flushes
// the counts to api_client_usage periodically (cheap: one upsert per key).
type UsageTracker struct {
	db     *pgxpool.Pool
	logger *slog.Logger
	mu     sync.Mutex
	counts map[usageKey]int64
	last   map[usageKey]time.Time
}

func NewUsageTracker(db *pgxpool.Pool, logger *slog.Logger) *UsageTracker {
	return &UsageTracker{db: db, logger: logger, counts: map[usageKey]int64{}, last: map[usageKey]time.Time{}}
}

func (u *UsageTracker) Record(version, client string) {
	now := time.Now().UTC()
	k := usageKey{version, client, now.Format("2006-01-02")}
	u.mu.Lock()
	u.counts[k]++
	u.last[k] = now
	u.mu.Unlock()
}

func (u *UsageTracker) Run(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			fctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			u.Flush(fctx)
			cancel()
			return
		case <-t.C:
			u.Flush(ctx)
		}
	}
}

func (u *UsageTracker) Flush(ctx context.Context) {
	u.mu.Lock()
	counts, last := u.counts, u.last
	u.counts, u.last = map[usageKey]int64{}, map[usageKey]time.Time{}
	u.mu.Unlock()
	for k, n := range counts {
		_, err := u.db.Exec(ctx, `INSERT INTO api_client_usage (api_version, client, day, requests, last_seen) VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (api_version, client, day) DO UPDATE SET requests = api_client_usage.requests + EXCLUDED.requests,
			last_seen = GREATEST(api_client_usage.last_seen, EXCLUDED.last_seen)`, k.version, k.client, k.day, n, last[k])
		if err != nil {
			u.logger.Warn("api usage flush failed", "error", err)
			// Put it back; it is retried on the next flush.
			u.mu.Lock()
			u.counts[k] += n
			if last[k].After(u.last[k]) {
				u.last[k] = last[k]
			}
			u.mu.Unlock()
		}
	}
}

type VersionReport struct {
	Version     string         `json:"version"`
	Notes       string         `json:"notes"`
	Deprecated  string         `json:"deprecated,omitempty"`
	Sunset      string         `json:"sunset,omitempty"`
	LastSeen    *time.Time     `json:"last_seen,omitempty"`
	Requests30d int64          `json:"requests_30d"`
	Clients     []ClientReport `json:"clients"`
	Removable   bool           `json:"removable"`
	Reason      string         `json:"reason"`
}

type ClientReport struct {
	Client      string    `json:"client"`
	LastSeen    time.Time `json:"last_seen"`
	Requests30d int64     `json:"requests_30d"`
}

// Report summarises usage and whether each version may be removed.
func (u *UsageTracker) Report(ctx context.Context, versions *APIVersions, now time.Time) ([]VersionReport, error) {
	u.Flush(ctx)
	rows, err := u.db.Query(ctx, `SELECT api_version, client, max(last_seen),
		coalesce(sum(requests) FILTER (WHERE day >= current_date - 30), 0)
		FROM api_client_usage GROUP BY api_version, client`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	clients := map[string][]ClientReport{}
	for rows.Next() {
		var v string
		var c ClientReport
		if err := rows.Scan(&v, &c.Client, &c.LastSeen, &c.Requests30d); err != nil {
			return nil, err
		}
		clients[v] = append(clients[v], c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []VersionReport
	for _, x := range versions.list {
		r := VersionReport{Version: x.Name, Notes: x.Notes, Clients: clients[x.Name]}
		sort.Slice(r.Clients, func(i, j int) bool { return r.Clients[i].LastSeen.After(r.Clients[j].LastSeen) })
		for _, c := range r.Clients {
			r.Requests30d += c.Requests30d
			if r.LastSeen == nil || c.LastSeen.After(*r.LastSeen) {
				t := c.LastSeen
				r.LastSeen = &t
			}
		}
		if !x.Deprecated.IsZero() {
			r.Deprecated = x.Deprecated.Format("2006-01-02")
		}
		if !x.Sunset.IsZero() {
			r.Sunset = x.Sunset.Format("2006-01-02")
		}
		switch {
		case x.Deprecated.IsZero():
			r.Reason = "not deprecated"
		case x.Sunset.IsZero() || now.Before(x.Sunset):
			r.Reason = "sunset date not reached"
		case r.LastSeen != nil && now.Sub(*r.LastSeen) < MinSupportWindow:
			r.Reason = "used within the support window (last seen " + r.LastSeen.Format("2006-01-02") + ")"
		default:
			r.Removable = true
			r.Reason = "sunset passed and unused for the whole support window"
		}
		out = append(out, r)
	}
	return out, nil
}
