// Package game holds the Ludo game logic (formerly the Lua runtime modules).
// It depends only on the NK interface, which the server implements on top of
// PostgreSQL and the cluster, and which tests implement in memory.
package game

import (
	"context"
	"errors"
)

const SystemUserID = "00000000-0000-0000-0000-000000000000"

// ReadID identifies a storage object. Empty UserID means the system user.
type ReadID struct {
	Collection, Key, UserID string
}

type Object struct {
	Collection      string
	Key             string
	UserID          string
	Value           map[string]any
	Version         string
	PermissionRead  int
	PermissionWrite int
	CreateTime      int64
	UpdateTime      int64
}

type Write struct {
	Collection      string
	Key             string
	UserID          string // empty = system user
	Value           any    // JSON-encodable (usually map[string]any)
	Version         string // "", "*" or a concrete version
	PermissionRead  int
	PermissionWrite int
}

type DeleteID struct {
	Collection, Key, UserID, Version string
}

type Ack struct {
	Collection, Key, UserID, Version string
}

type WalletUpdate struct {
	UserID    string
	Changeset map[string]int64
	Metadata  map[string]any
}

type Account struct {
	UserID      string
	Username    string
	DisplayName string
	AvatarURL   string
	Email       string
	Wallet      map[string]int64
	CreateTime  int64
}

type LeaderboardRecord struct {
	LeaderboardID string
	OwnerID       string
	Username      string
	Score         int64
	Subscore      int64
	NumScore      int64
	Metadata      map[string]any
	CreateTime    int64
	UpdateTime    int64
	ExpiryTime    int64
	Rank          int64
}

type MatchInfo struct {
	MatchID  string
	Label    string
	Size     int
	TickRate int
	Handler  string
}

type ValidatedPurchase struct {
	UserID           string
	ProductID        string
	TransactionID    string
	ProviderResponse string
	RefundTime       int64
	SeenBefore       bool
}

type HTTPResponse struct {
	Code    int
	Headers map[string]string
	Body    string
}

// NK is the server API available to game code. Every method is safe for
// concurrent use.
type NK interface {
	// NowMs is milliseconds since the epoch (nk.time). Unix is seconds (os.time).
	NowMs() int64
	Unix() int64
	// RandInt returns a uniformly distributed integer in [lo, hi].
	RandInt(lo, hi int) int
	UUID() string
	Env() map[string]string

	StorageRead(ctx context.Context, ids []ReadID) ([]*Object, error)
	StorageWrite(ctx context.Context, writes []*Write) ([]*Ack, error)
	StorageDelete(ctx context.Context, ids []DeleteID) error
	StorageList(ctx context.Context, userID *string, collection string, limit int, cursor string) ([]*Object, string, error)
	MultiUpdate(ctx context.Context, writes []*Write, wallets []*WalletUpdate, updateLedger bool) error

	AccountGet(ctx context.Context, userID string) (*Account, error)
	AccountUpdate(ctx context.Context, userID string, username string, displayName, avatarURL *string) error
	AccountDelete(ctx context.Context, userID string) error
	LinkEmail(ctx context.Context, userID, email, password string) error
	UserIDByEmail(ctx context.Context, email string) (string, error)
	SetPassword(ctx context.Context, userID, password string) error
	WalletUpdate(ctx context.Context, userID string, changeset map[string]int64, metadata map[string]any, updateLedger bool) (updated, previous map[string]int64, err error)

	LeaderboardCreate(ctx context.Context, id string, authoritative bool, sortDesc bool, operator string, resetSchedule string) error
	LeaderboardRecordWrite(ctx context.Context, id, ownerID, username string, score, subscore int64, metadata map[string]any) (*LeaderboardRecord, error)
	LeaderboardRecordsList(ctx context.Context, id string, ownerIDs []string, limit int, cursor string) (records, ownerRecords []*LeaderboardRecord, next, prev string, err error)

	MatchCreate(ctx context.Context, module string, params map[string]any) (string, error)
	MatchGet(ctx context.Context, matchID string) (*MatchInfo, error)
	MatchSignal(ctx context.Context, matchID, data string) (string, error)

	HTTPRequest(ctx context.Context, url, method string, headers map[string]string, body string, timeoutMs int) (*HTTPResponse, error)
	PurchaseValidateGoogle(ctx context.Context, userID, receipt string, persist bool) ([]*ValidatedPurchase, error)

	Logger() Logger
	Metrics() Metrics
}

type Logger interface {
	Info(msg string, kv ...any)
	Warn(msg string, kv ...any)
	Error(msg string, kv ...any)
}

type Metrics interface {
	CounterAdd(name string, tags map[string]string, delta int64)
	GaugeSet(name string, tags map[string]string, value float64)
	TimerRecord(name string, tags map[string]string, nanos int64)
}

// ErrStorageVersion is returned by StorageWrite/MultiUpdate on an OCC conflict.
var ErrStorageVersion = errors.New("storage write rejected - version check failed")

// ErrNotFound is returned when a match or account does not exist.
var ErrNotFound = errors.New("not found")
