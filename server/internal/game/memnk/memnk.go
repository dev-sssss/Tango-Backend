// Package memnk is an in-memory implementation of game.NK with a virtual
// clock and a deterministic random source. It is used by unit tests, the
// Lua differential tests and local single-process runs.
package memnk

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"sort"
	"strings"
	"sync"

	"github.com/dev-sssss/tango_backend/server/internal/game"
)

type storedObject struct {
	value   string
	version string
	read    int
	write   int
	created int64
	updated int64
}

type user struct {
	id, username, display, avatar, email, password string
	wallet                                         map[string]int64
	created                                        int64
}

type LedgerEntry struct {
	UserID    string
	Changeset map[string]int64
	Metadata  map[string]any
}

type LBRecord struct {
	Owner string
	Score int64
	Num   int64
}

type NK struct {
	mu      sync.Mutex
	clockMs int64
	rng     *rand.Rand
	env     map[string]string

	storage map[string]*storedObject // collection|key|user
	users   map[string]*user
	Ledger  []LedgerEntry
	LB      map[string]map[string]*LBRecord
	Logs    []string
	Counter map[string]int64

	MatchCreateFn func(ctx context.Context, module string, params map[string]any) (string, error)
	MatchGetFn    func(ctx context.Context, id string) (*game.MatchInfo, error)
	MatchSignalFn func(ctx context.Context, id, data string) (string, error)
	HTTPFn        func(ctx context.Context, url, method string, headers map[string]string, body string, timeoutMs int) (*game.HTTPResponse, error)
	PurchaseFn    func(ctx context.Context, userID, receipt string, persist bool) ([]*game.ValidatedPurchase, error)
	uuidSeq       int
}

func New(seed uint64, startMs int64, env map[string]string) *NK {
	if env == nil {
		env = map[string]string{}
	}
	return &NK{
		clockMs: startMs,
		rng:     rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15)),
		env:     env,
		storage: map[string]*storedObject{},
		users:   map[string]*user{},
		LB:      map[string]map[string]*LBRecord{},
		Counter: map[string]int64{},
	}
}

// ---- clock / random

func (n *NK) NowMs() int64 { n.mu.Lock(); defer n.mu.Unlock(); return n.clockMs }
func (n *NK) Unix() int64  { n.mu.Lock(); defer n.mu.Unlock(); return n.clockMs / 1000 }
func (n *NK) Advance(ms int64) {
	n.mu.Lock()
	n.clockMs += ms
	n.mu.Unlock()
}
func (n *NK) RandInt(lo, hi int) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	if hi < lo {
		return lo
	}
	return lo + n.rng.IntN(hi-lo+1)
}
func (n *NK) UUID() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.uuidSeq++
	return fmt.Sprintf("b0000000-0000-4000-8000-%012d", n.uuidSeq)
}
func (n *NK) Env() map[string]string { return n.env }

// ---- users

func (n *NK) AddUser(id, username, email string, wallet map[string]int64) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if wallet == nil {
		wallet = map[string]int64{}
	}
	n.users[id] = &user{id: id, username: username, email: email, wallet: wallet, created: n.clockMs / 1000}
}

func (n *NK) Wallet(id string) map[string]int64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	if u, ok := n.users[id]; ok {
		out := map[string]int64{}
		for k, v := range u.wallet {
			out[k] = v
		}
		return out
	}
	return nil
}

// ---- storage

func skey(c, k, u string) string {
	if u == "" {
		u = game.SystemUserID
	}
	return c + "|" + k + "|" + u
}

func version(v string) string {
	h := md5.Sum([]byte(v))
	return hex.EncodeToString(h[:])
}

func decodeValue(s string) map[string]any {
	var m map[string]any
	_ = json.Unmarshal([]byte(s), &m)
	if m == nil {
		m = map[string]any{}
	}
	return m
}

func (n *NK) StorageRead(ctx context.Context, ids []game.ReadID) ([]*game.Object, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	var out []*game.Object
	for _, id := range ids {
		u := id.UserID
		if u == "" {
			u = game.SystemUserID
		}
		if o, ok := n.storage[skey(id.Collection, id.Key, u)]; ok {
			out = append(out, &game.Object{Collection: id.Collection, Key: id.Key, UserID: u, Value: decodeValue(o.value), Version: o.version, PermissionRead: o.read, PermissionWrite: o.write, CreateTime: o.created, UpdateTime: o.updated})
		}
	}
	return out, nil
}

func encodeValue(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []byte:
		return string(t)
	}
	return game.Encode(v)
}

func (n *NK) writeLocked(writes []*game.Write) ([]*game.Ack, error) {
	// Validate first so the batch is atomic.
	for _, w := range writes {
		k := skey(w.Collection, w.Key, w.UserID)
		cur, exists := n.storage[k]
		switch {
		case w.Version == "*" && exists:
			return nil, game.ErrStorageVersion
		case w.Version != "" && w.Version != "*" && (!exists || cur.version != w.Version):
			return nil, game.ErrStorageVersion
		}
		if w.UserID != "" && w.UserID != game.SystemUserID {
			if _, ok := n.users[w.UserID]; !ok {
				return nil, errors.New("storage owner not found")
			}
		}
	}
	acks := make([]*game.Ack, len(writes))
	for i, w := range writes {
		k := skey(w.Collection, w.Key, w.UserID)
		val := encodeValue(w.Value)
		o, exists := n.storage[k]
		if !exists {
			o = &storedObject{created: n.clockMs / 1000}
			n.storage[k] = o
		}
		o.value, o.version, o.read, o.write, o.updated = val, version(val), w.PermissionRead, w.PermissionWrite, n.clockMs/1000
		u := w.UserID
		if u == "" {
			u = game.SystemUserID
		}
		acks[i] = &game.Ack{Collection: w.Collection, Key: w.Key, UserID: u, Version: o.version}
	}
	return acks, nil
}

func (n *NK) StorageWrite(ctx context.Context, writes []*game.Write) ([]*game.Ack, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.writeLocked(writes)
}

func (n *NK) StorageDelete(ctx context.Context, ids []game.DeleteID) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, id := range ids {
		k := skey(id.Collection, id.Key, id.UserID)
		o, ok := n.storage[k]
		if id.Version != "" && (!ok || o.version != id.Version) {
			return errors.New("Storage delete rejected - not found, version check failed, or permission denied.")
		}
	}
	for _, id := range ids {
		delete(n.storage, skey(id.Collection, id.Key, id.UserID))
	}
	return nil
}

func (n *NK) StorageList(ctx context.Context, userID *string, collection string, limit int, cursor string) ([]*game.Object, string, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	var keys []string
	for k := range n.storage {
		parts := strings.SplitN(k, "|", 3)
		if parts[0] != collection {
			continue
		}
		if userID != nil && parts[2] != *userID {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	start := 0
	if cursor != "" {
		for i, k := range keys {
			if k > cursor {
				start = i
				break
			}
			start = len(keys)
		}
	}
	if limit <= 0 {
		limit = 100
	}
	var out []*game.Object
	next := ""
	for i := start; i < len(keys); i++ {
		if len(out) == limit {
			next = keys[i-1]
			break
		}
		parts := strings.SplitN(keys[i], "|", 3)
		o := n.storage[keys[i]]
		out = append(out, &game.Object{Collection: parts[0], Key: parts[1], UserID: parts[2], Value: decodeValue(o.value), Version: o.version, PermissionRead: o.read, PermissionWrite: o.write})
	}
	return out, next, nil
}

// Dump returns all storage as key -> JSON value (for comparisons).
func (n *NK) Dump() map[string]string {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := map[string]string{}
	for k, o := range n.storage {
		out[k] = fmt.Sprintf("%s r=%d w=%d", o.value, o.read, o.write)
	}
	return out
}

// ---- wallet / accounts

func (n *NK) walletLocked(u string, cs map[string]int64, meta map[string]any, ledger bool, apply bool) (map[string]int64, map[string]int64, error) {
	usr, ok := n.users[u]
	if !ok {
		return nil, nil, game.ErrNotFound
	}
	prev := map[string]int64{}
	for k, v := range usr.wallet {
		prev[k] = v
	}
	next := map[string]int64{}
	for k, v := range usr.wallet {
		next[k] = v
	}
	for k, v := range cs {
		nv := next[k] + v
		if nv < 0 {
			return nil, nil, fmt.Errorf("wallet update rejected: negative balance for %s", k)
		}
		next[k] = nv
	}
	if apply {
		usr.wallet = next
		if ledger {
			n.Ledger = append(n.Ledger, LedgerEntry{UserID: u, Changeset: cs, Metadata: meta})
		}
	}
	return next, prev, nil
}

func (n *NK) WalletUpdate(ctx context.Context, userID string, cs map[string]int64, meta map[string]any, ledger bool) (map[string]int64, map[string]int64, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.walletLocked(userID, cs, meta, ledger, true)
}

func (n *NK) MultiUpdate(ctx context.Context, writes []*game.Write, wallets []*game.WalletUpdate, ledger bool) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	// Validate wallets, then storage, then apply both.
	for _, w := range wallets {
		if _, _, err := n.walletLocked(w.UserID, w.Changeset, w.Metadata, ledger, false); err != nil {
			return err
		}
	}
	snapshot := map[string]*storedObject{}
	for k, v := range n.storage {
		c := *v
		snapshot[k] = &c
	}
	if _, err := n.writeLocked(writes); err != nil {
		n.storage = snapshot
		return err
	}
	for _, w := range wallets {
		_, _, _ = n.walletLocked(w.UserID, w.Changeset, w.Metadata, ledger, true)
	}
	return nil
}

func (n *NK) AccountGet(ctx context.Context, userID string) (*game.Account, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	u, ok := n.users[userID]
	if !ok {
		return nil, game.ErrNotFound
	}
	w := map[string]int64{}
	for k, v := range u.wallet {
		w[k] = v
	}
	return &game.Account{UserID: u.id, Username: u.username, DisplayName: u.display, AvatarURL: u.avatar, Email: u.email, Wallet: w, CreateTime: u.created}, nil
}

func (n *NK) AccountUpdate(ctx context.Context, userID, username string, displayName, avatarURL *string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	u, ok := n.users[userID]
	if !ok {
		return game.ErrNotFound
	}
	if username != "" {
		for _, o := range n.users {
			if o.username == username && o.id != userID {
				return errors.New("Username is already in use.")
			}
		}
		u.username = username
	}
	if displayName != nil {
		u.display = *displayName
	}
	if avatarURL != nil {
		u.avatar = *avatarURL
	}
	return nil
}

func (n *NK) AccountDelete(ctx context.Context, userID string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.users, userID)
	for k := range n.storage {
		if strings.HasSuffix(k, "|"+userID) {
			delete(n.storage, k)
		}
	}
	return nil
}

func (n *NK) LinkEmail(ctx context.Context, userID, email, password string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, o := range n.users {
		if o.email == strings.ToLower(email) && o.id != userID {
			return errors.New("Email is already in use.")
		}
	}
	u, ok := n.users[userID]
	if !ok {
		return game.ErrNotFound
	}
	u.email, u.password = strings.ToLower(email), password
	return nil
}

func (n *NK) UserIDByEmail(ctx context.Context, email string) (string, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, u := range n.users {
		if u.email == strings.ToLower(email) {
			return u.id, nil
		}
	}
	return "", game.ErrNotFound
}

func (n *NK) SetPassword(ctx context.Context, userID, password string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	u, ok := n.users[userID]
	if !ok {
		return game.ErrNotFound
	}
	u.password = password
	return nil
}

func (n *NK) Password(userID string) string {
	n.mu.Lock()
	defer n.mu.Unlock()
	if u, ok := n.users[userID]; ok {
		return u.password
	}
	return ""
}

// ---- leaderboards (increment operator, no reset windows)

func (n *NK) LeaderboardCreate(ctx context.Context, id string, authoritative, sortDesc bool, operator, reset string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if _, ok := n.LB[id]; !ok {
		n.LB[id] = map[string]*LBRecord{}
	}
	return nil
}

func (n *NK) LeaderboardRecordWrite(ctx context.Context, id, owner, username string, score, subscore int64, metadata map[string]any) (*game.LeaderboardRecord, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	lb, ok := n.LB[id]
	if !ok {
		lb = map[string]*LBRecord{}
		n.LB[id] = lb
	}
	r, ok := lb[owner]
	if !ok {
		r = &LBRecord{Owner: owner}
		lb[owner] = r
	}
	r.Score += score
	r.Num++
	return &game.LeaderboardRecord{LeaderboardID: id, OwnerID: owner, Score: r.Score, NumScore: r.Num}, nil
}

func (n *NK) LeaderboardRecordsList(ctx context.Context, id string, owners []string, limit int, cursor string) ([]*game.LeaderboardRecord, []*game.LeaderboardRecord, string, string, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	lb := n.LB[id]
	var all []*LBRecord
	for _, r := range lb {
		all = append(all, r)
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].Score != all[j].Score {
			return all[i].Score > all[j].Score
		}
		return all[i].Owner > all[j].Owner
	})
	rank := map[string]int64{}
	for i, r := range all {
		rank[r.Owner] = int64(i + 1)
	}
	var recs, ownerRecs []*game.LeaderboardRecord
	if owners == nil {
		for i, r := range all {
			if limit > 0 && i >= limit {
				break
			}
			recs = append(recs, &game.LeaderboardRecord{LeaderboardID: id, OwnerID: r.Owner, Score: r.Score, NumScore: r.Num, Rank: rank[r.Owner]})
		}
	} else {
		for _, o := range owners {
			if r, ok := lb[o]; ok {
				ownerRecs = append(ownerRecs, &game.LeaderboardRecord{LeaderboardID: id, OwnerID: r.Owner, Score: r.Score, NumScore: r.Num, Rank: rank[o]})
			}
		}
	}
	return recs, ownerRecs, "", "", nil
}

// ---- matches / http / purchases (pluggable)

func (n *NK) MatchCreate(ctx context.Context, module string, params map[string]any) (string, error) {
	if n.MatchCreateFn == nil {
		return "", errors.New("match create unavailable")
	}
	return n.MatchCreateFn(ctx, module, params)
}
func (n *NK) MatchGet(ctx context.Context, id string) (*game.MatchInfo, error) {
	if n.MatchGetFn == nil {
		return nil, nil
	}
	return n.MatchGetFn(ctx, id)
}
func (n *NK) MatchSignal(ctx context.Context, id, data string) (string, error) {
	if n.MatchSignalFn == nil {
		return "", game.ErrNotFound
	}
	return n.MatchSignalFn(ctx, id, data)
}
func (n *NK) HTTPRequest(ctx context.Context, url, method string, headers map[string]string, body string, timeoutMs int) (*game.HTTPResponse, error) {
	if n.HTTPFn == nil {
		return nil, errors.New("http disabled")
	}
	return n.HTTPFn(ctx, url, method, headers, body, timeoutMs)
}
func (n *NK) PurchaseValidateGoogle(ctx context.Context, userID, receipt string, persist bool) ([]*game.ValidatedPurchase, error) {
	if n.PurchaseFn == nil {
		return nil, errors.New("purchases disabled")
	}
	return n.PurchaseFn(ctx, userID, receipt, persist)
}

// ---- logging / metrics

type logger struct{ n *NK }

func (l logger) log(level, msg string, kv ...any) {
	l.n.mu.Lock()
	l.n.Logs = append(l.n.Logs, level+": "+msg+" "+fmt.Sprint(kv...))
	l.n.mu.Unlock()
}
func (l logger) Info(msg string, kv ...any)  { l.log("info", msg, kv...) }
func (l logger) Warn(msg string, kv ...any)  { l.log("warn", msg, kv...) }
func (l logger) Error(msg string, kv ...any) { l.log("error", msg, kv...) }

func (n *NK) Logger() game.Logger { return logger{n} }

type metrics struct{ n *NK }

func tagKey(name string, tags map[string]string) string {
	keys := make([]string, 0, len(tags))
	for k := range tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(name)
	for _, k := range keys {
		b.WriteString("," + k + "=" + tags[k])
	}
	return b.String()
}

func (m metrics) CounterAdd(name string, tags map[string]string, delta int64) {
	m.n.mu.Lock()
	m.n.Counter["counter:"+tagKey(name, tags)] += delta
	m.n.mu.Unlock()
}
func (m metrics) GaugeSet(name string, tags map[string]string, v float64) {
	m.n.mu.Lock()
	m.n.Counter["gauge:"+tagKey(name, tags)] = int64(v)
	m.n.mu.Unlock()
}
func (m metrics) TimerRecord(name string, tags map[string]string, nanos int64) {
	m.n.mu.Lock()
	m.n.Counter["timer:"+tagKey(name, tags)] += nanos
	m.n.mu.Unlock()
}

func (n *NK) Metrics() game.Metrics { return metrics{n} }

// CopyStateFrom replaces this instance's storage, users and leaderboards with
// a deep copy of other's (used by differential tests to re-synchronise after
// an intentional behaviour change).
func (n *NK) CopyStateFrom(other *NK) {
	other.mu.Lock()
	defer other.mu.Unlock()
	n.mu.Lock()
	defer n.mu.Unlock()
	n.storage = map[string]*storedObject{}
	for k, v := range other.storage {
		c := *v
		n.storage[k] = &c
	}
	n.users = map[string]*user{}
	for k, v := range other.users {
		c := *v
		c.wallet = map[string]int64{}
		for wk, wv := range v.wallet {
			c.wallet[wk] = wv
		}
		n.users[k] = &c
	}
	n.LB = map[string]map[string]*LBRecord{}
	for id, lb := range other.LB {
		m := map[string]*LBRecord{}
		for o, r := range lb {
			c := *r
			m[o] = &c
		}
		n.LB[id] = m
	}
}
