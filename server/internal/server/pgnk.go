package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strings"
	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"

	"github.com/dev-sssss/tango_backend/server/internal/game"
)

// PgNK implements game.NK on PostgreSQL and the cluster match host.
type PgNK struct {
	cfg       *Config
	db        *pgxpool.Pool
	env       map[string]string
	logger    *slog.Logger
	metrics   *Metrics
	host      *MatchHost
	lbs       *Leaderboards
	purchases *GooglePurchases
	httpc     *http.Client
	guard     *Guard
	analytics *Analytics
	friends   *Friends
}

func NewPgNK(cfg *Config, db *pgxpool.Pool, logger *slog.Logger, metrics *Metrics, host *MatchHost) *PgNK {
	return &PgNK{cfg: cfg, db: db, env: cfg.RuntimeEnv(), logger: logger, metrics: metrics, host: host,
		lbs: NewLeaderboards(db), purchases: NewGooglePurchases(cfg.IAP.Google, db),
		httpc: &http.Client{Timeout: 30 * time.Second}}
}

var _ game.NK = (*PgNK)(nil)

func (n *PgNK) NowMs() int64                { return time.Now().UnixMilli() }
func (n *PgNK) Unix() int64                 { return time.Now().Unix() }
func (n *PgNK) RandInt(lo, hi int) int      { return lo + rand.IntN(hi-lo+1) }
func (n *PgNK) UUID() string                { return uuid.Must(uuid.NewV4()).String() }
func (n *PgNK) Env() map[string]string      { return n.env }
func (n *PgNK) Logger() game.Logger         { return slogLogger{n.logger} }
func (n *PgNK) Metrics() game.Metrics       { return n.metrics }
func (n *PgNK) Leaderboards() *Leaderboards { return n.lbs }

type slogLogger struct{ l *slog.Logger }

func (s slogLogger) Info(msg string, kv ...any)  { s.l.Info(msg, kv...) }
func (s slogLogger) Warn(msg string, kv ...any)  { s.l.Warn(msg, kv...) }
func (s slogLogger) Error(msg string, kv ...any) { s.l.Error(msg, kv...) }

// ---------------------------------------------------------------- storage

func ownerOrSystem(u string) string {
	if u == "" {
		return systemUserID
	}
	return u
}

func toGameObject(o *StorageObject) *game.Object {
	var v map[string]any
	_ = json.Unmarshal([]byte(o.Value), &v)
	if v == nil {
		v = map[string]any{}
	}
	return &game.Object{Collection: o.Collection, Key: o.Key, UserID: o.UserID, Value: game.Normalize(v).(map[string]any), Version: o.Version,
		PermissionRead: int(o.PermissionRead), PermissionWrite: int(o.PermissionWrite), CreateTime: o.CreateTime.Unix(), UpdateTime: o.UpdateTime.Unix()}
}

func encodeStorageValue(v any) (string, error) {
	switch t := v.(type) {
	case string:
		return t, nil
	case []byte:
		return string(t), nil
	}
	s := game.Encode(v)
	if s == "" {
		return "", errors.New("value is not JSON encodable")
	}
	return s, nil
}

func toStorageWrites(ws []*game.Write) ([]*StorageWrite, error) {
	out := make([]*StorageWrite, len(ws))
	for i, w := range ws {
		val, err := encodeStorageValue(w.Value)
		if err != nil {
			return nil, err
		}
		out[i] = &StorageWrite{Collection: w.Collection, Key: w.Key, UserID: ownerOrSystem(w.UserID), Value: val, Version: w.Version,
			PermissionRead: int32(w.PermissionRead), PermissionWrite: int32(w.PermissionWrite)}
	}
	return out, nil
}

func gameErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, errStorageRejectedVersion):
		return game.ErrStorageVersion
	case errors.Is(err, errUserNotFound):
		return game.ErrNotFound
	}
	return err
}

func (n *PgNK) StorageRead(ctx context.Context, ids []game.ReadID) ([]*game.Object, error) {
	sids := make([]StorageObjectID, len(ids))
	for i, id := range ids {
		sids[i] = StorageObjectID{Collection: id.Collection, Key: id.Key, UserID: ownerOrSystem(id.UserID)}
	}
	objs, err := StorageRead(ctx, n.db, sids)
	if err != nil {
		return nil, err
	}
	out := make([]*game.Object, len(objs))
	for i, o := range objs {
		out[i] = toGameObject(o)
	}
	return out, nil
}

func (n *PgNK) StorageWrite(ctx context.Context, writes []*game.Write) ([]*game.Ack, error) {
	sw, err := toStorageWrites(writes)
	if err != nil {
		return nil, err
	}
	acks, err := StorageWriteObjects(ctx, n.db, sw)
	if err != nil {
		return nil, gameErr(err)
	}
	out := make([]*game.Ack, len(acks))
	for i, a := range acks {
		out[i] = &game.Ack{Collection: a.Collection, Key: a.Key, UserID: a.UserID, Version: a.Version}
	}
	return out, nil
}

func (n *PgNK) StorageDelete(ctx context.Context, ids []game.DeleteID) error {
	sids := make([]StorageObjectID, len(ids))
	for i, id := range ids {
		sids[i] = StorageObjectID{Collection: id.Collection, Key: id.Key, UserID: ownerOrSystem(id.UserID), Version: id.Version}
	}
	return gameErr(StorageDeleteObjects(ctx, n.db, sids))
}

func (n *PgNK) StorageList(ctx context.Context, userID *string, collection string, limit int, cursor string) ([]*game.Object, string, error) {
	objs, next, err := StorageList(ctx, n.db, userID, collection, limit, cursor)
	if err != nil {
		return nil, "", err
	}
	out := make([]*game.Object, len(objs))
	for i, o := range objs {
		out[i] = toGameObject(o)
	}
	return out, next, nil
}

func walletUpdates(ws []*game.WalletUpdate) []*WalletUpdate {
	out := make([]*WalletUpdate, len(ws))
	for i, w := range ws {
		meta := "{}"
		if w.Metadata != nil {
			meta = game.Encode(w.Metadata)
		}
		out[i] = &WalletUpdate{UserID: w.UserID, Changeset: w.Changeset, Metadata: meta}
	}
	return out
}

func (n *PgNK) MultiUpdate(ctx context.Context, writes []*game.Write, wallets []*game.WalletUpdate, updateLedger bool) error {
	sw, err := toStorageWrites(writes)
	if err != nil {
		return err
	}
	_, _, err = MultiUpdate(ctx, n.db, sw, walletUpdates(wallets), updateLedger)
	return gameErr(err)
}

func (n *PgNK) WalletUpdate(ctx context.Context, userID string, changeset map[string]int64, metadata map[string]any, updateLedger bool) (map[string]int64, map[string]int64, error) {
	res, err := UpdateWallets(ctx, n.db, walletUpdates([]*game.WalletUpdate{{UserID: userID, Changeset: changeset, Metadata: metadata}}), updateLedger)
	if err != nil {
		return nil, nil, gameErr(err)
	}
	if len(res) == 0 {
		return nil, nil, game.ErrNotFound
	}
	return res[0].Updated, res[0].Previous, nil
}

// ---------------------------------------------------------------- accounts

func parseWallet(s string) map[string]int64 {
	out := map[string]int64{}
	var raw map[string]json.Number
	d := json.NewDecoder(strings.NewReader(s))
	d.UseNumber()
	if err := d.Decode(&raw); err != nil {
		return out
	}
	for k, v := range raw {
		if i, err := v.Int64(); err == nil {
			out[k] = i
		} else if f, err := v.Float64(); err == nil {
			out[k] = int64(f)
		}
	}
	return out
}

func (n *PgNK) AccountGet(ctx context.Context, userID string) (*game.Account, error) {
	a, err := GetAccount(ctx, n.db, userID)
	if err != nil {
		return nil, gameErr(err)
	}
	return &game.Account{UserID: a.UserID, Username: a.Username, DisplayName: a.DisplayName, AvatarURL: a.AvatarURL, Email: a.Email,
		Wallet: parseWallet(a.Wallet), CreateTime: a.CreateTime.Unix()}, nil
}

func (n *PgNK) AccountUpdate(ctx context.Context, userID, username string, displayName, avatarURL *string) error {
	return gameErr(UpdateAccounts(ctx, n.db, []*AccountUpdate{{UserID: userID, Username: username, DisplayName: displayName, AvatarURL: avatarURL}}))
}

func (n *PgNK) AccountDelete(ctx context.Context, userID string) error {
	return DeleteAccount(ctx, n.db, userID)
}

func (n *PgNK) LinkEmail(ctx context.Context, userID, email, password string) error {
	release, err := n.guard.HashSlot(ctx)
	if err != nil {
		return err
	}
	defer release()
	return LinkEmail(ctx, n.db, userID, email, password)
}

func (n *PgNK) UserIDByEmail(ctx context.Context, email string) (string, error) {
	var id string
	err := n.db.QueryRow(ctx, "SELECT id::text FROM users WHERE email = $1", strings.ToLower(strings.TrimSpace(email))).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", game.ErrNotFound
	}
	return id, err
}

func (n *PgNK) SetPassword(ctx context.Context, userID, password string) error {
	release, err := n.guard.HashSlot(ctx)
	if err != nil {
		return err
	}
	defer release()
	hashed, err := bcrypt.GenerateFromPassword([]byte(password), bcryptHashCost)
	if err != nil {
		return err
	}
	tag, err := n.db.Exec(ctx, "UPDATE users SET password = $2, update_time = now() WHERE id = $1", userID, hashed)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return game.ErrNotFound
	}
	return nil
}

// ---------------------------------------------------------------- leaderboards

func (n *PgNK) LeaderboardCreate(ctx context.Context, id string, authoritative, sortDesc bool, operator, resetSchedule string) error {
	return n.lbs.Create(ctx, id, authoritative, sortDesc, operator, resetSchedule)
}

func (n *PgNK) LeaderboardRecordWrite(ctx context.Context, id, ownerID, username string, score, subscore int64, metadata map[string]any) (*game.LeaderboardRecord, error) {
	return n.lbs.Write(ctx, id, ownerID, username, score, subscore, metadata)
}

func (n *PgNK) LeaderboardRecordsList(ctx context.Context, id string, ownerIDs []string, limit int, cursor string) ([]*game.LeaderboardRecord, []*game.LeaderboardRecord, string, string, error) {
	return n.lbs.List(ctx, id, ownerIDs, limit, cursor)
}

// ---------------------------------------------------------------- matches

func (n *PgNK) MatchCreate(ctx context.Context, module string, params map[string]any) (string, error) {
	return n.host.Create(ctx, module, params)
}

func (n *PgNK) MatchGet(ctx context.Context, matchID string) (*game.MatchInfo, error) {
	return n.host.Get(ctx, matchID)
}

func (n *PgNK) MatchSignal(ctx context.Context, matchID, data string) (string, error) {
	return n.host.Signal(ctx, matchID, data)
}

// ---------------------------------------------------------------- outbound

func (n *PgNK) HTTPRequest(ctx context.Context, url, method string, headers map[string]string, body string, timeoutMs int) (*game.HTTPResponse, error) {
	limit := n.cfg.Runtime.HTTPRequestTimeoutCapMs
	if timeoutMs <= 0 || (limit > 0 && timeoutMs > limit) {
		timeoutMs = limit
	}
	if timeoutMs > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(timeoutMs)*time.Millisecond)
		defer cancel()
	}
	var rd io.Reader
	if body != "" {
		rd = bytes.NewBufferString(body)
	}
	req, err := http.NewRequestWithContext(ctx, strings.ToUpper(method), url, rd)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := n.httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	h := map[string]string{}
	for k, v := range resp.Header {
		h[k] = strings.Join(v, ",")
	}
	return &game.HTTPResponse{Code: resp.StatusCode, Headers: h, Body: string(b)}, nil
}

func (n *PgNK) PurchaseValidateGoogle(ctx context.Context, userID, receipt string, persist bool) ([]*game.ValidatedPurchase, error) {
	return n.purchases.Validate(ctx, userID, receipt, persist)
}
