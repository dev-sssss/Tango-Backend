package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/gofrs/uuid/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type WalletUpdate struct {
	UserID    string
	Changeset map[string]int64
	Metadata  string // JSON
}

type WalletResult struct {
	UserID   string
	Updated  map[string]int64
	Previous map[string]int64
}

var errWalletNegative = errors.New("wallet update rejected: negative balance")

// updateWalletsTx applies wallet changes (Nakama semantics: a resulting
// negative balance rejects the whole operation) and optionally records
// ledger entries.
func updateWalletsTx(ctx context.Context, tx pgx.Tx, updates []*WalletUpdate, ledger bool) ([]*WalletResult, error) {
	if len(updates) == 0 {
		return nil, nil
	}
	ids := make([]string, 0, len(updates))
	for _, u := range updates {
		ids = append(ids, u.UserID)
	}
	sort.Strings(ids)
	rows, err := tx.Query(ctx, "SELECT id::text, wallet::text FROM users WHERE id = ANY($1::uuid[]) ORDER BY id FOR UPDATE", ids)
	if err != nil {
		return nil, err
	}
	wallets := map[string]map[string]int64{}
	for rows.Next() {
		var id, w string
		if err := rows.Scan(&id, &w); err != nil {
			rows.Close()
			return nil, err
		}
		m := map[string]int64{}
		if err := json.Unmarshal([]byte(w), &m); err != nil {
			rows.Close()
			return nil, fmt.Errorf("wallet of %s is not a flat integer map: %w", id, err)
		}
		wallets[id] = m
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var results []*WalletResult
	changed := map[string]bool{}
	for _, u := range updates {
		w, ok := wallets[u.UserID]
		if !ok {
			continue
		}
		prev := make(map[string]int64, len(w))
		for k, v := range w {
			prev[k] = v
		}
		for k, v := range u.Changeset {
			nv := w[k] + v
			if nv < 0 {
				return nil, fmt.Errorf("%w (user %s, %s: %d%+d)", errWalletNegative, u.UserID, k, w[k], v)
			}
			w[k] = nv
		}
		upd := make(map[string]int64, len(w))
		for k, v := range w {
			upd[k] = v
		}
		results = append(results, &WalletResult{UserID: u.UserID, Updated: upd, Previous: prev})
		changed[u.UserID] = true
		if ledger {
			cs, _ := json.Marshal(u.Changeset)
			meta := u.Metadata
			if meta == "" {
				meta = "{}"
			}
			if _, err := tx.Exec(ctx, "INSERT INTO wallet_ledger (id, user_id, changeset, metadata) VALUES ($1, $2, $3, $4)", uuid.Must(uuid.NewV4()).String(), u.UserID, cs, meta); err != nil {
				return nil, err
			}
		}
	}
	for id := range changed {
		b, _ := json.Marshal(wallets[id])
		if _, err := tx.Exec(ctx, "UPDATE users SET wallet = $2, update_time = now() WHERE id = $1", id, b); err != nil {
			return nil, err
		}
	}
	return results, nil
}

func UpdateWallets(ctx context.Context, db *pgxpool.Pool, updates []*WalletUpdate, ledger bool) ([]*WalletResult, error) {
	var res []*WalletResult
	err := ExecuteInTx(ctx, db, func(tx pgx.Tx) error {
		var err error
		res, err = updateWalletsTx(ctx, tx, updates, ledger)
		return err
	})
	return res, err
}

// MultiUpdate commits storage writes and wallet updates atomically.
func MultiUpdate(ctx context.Context, db *pgxpool.Pool, writes []*StorageWrite, wallets []*WalletUpdate, ledger bool) ([]*StorageAck, []*WalletResult, error) {
	var acks []*StorageAck
	var res []*WalletResult
	err := ExecuteInTx(ctx, db, func(tx pgx.Tx) error {
		var err error
		if len(writes) > 0 {
			if acks, err = storageWriteTx(ctx, tx, writes); err != nil {
				return err
			}
		}
		res, err = updateWalletsTx(ctx, tx, wallets, ledger)
		return err
	})
	return acks, res, err
}
