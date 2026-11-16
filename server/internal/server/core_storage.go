package server

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/gob"
	"encoding/hex"
	"errors"
	"sort"
	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const systemUserID = "00000000-0000-0000-0000-000000000000"

type StorageObject struct {
	Collection      string
	Key             string
	UserID          string
	Value           string
	Version         string
	PermissionRead  int32
	PermissionWrite int32
	CreateTime      time.Time
	UpdateTime      time.Time
}

type StorageObjectID struct {
	Collection string
	Key        string
	UserID     string
	Version    string
}

type StorageWrite struct {
	Collection      string
	Key             string
	UserID          string // owner; system user when empty
	Value           string // JSON
	Version         string // "" (no OCC), "*" (create only) or an expected version
	PermissionRead  int32
	PermissionWrite int32
}

type StorageAck struct {
	Collection, Key, UserID, Version string
}

func (w *StorageWrite) expectedVersion() string {
	h := md5.Sum([]byte(w.Value))
	return hex.EncodeToString(h[:])
}

func StorageRead(ctx context.Context, db dbtx, ids []StorageObjectID) ([]*StorageObject, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	cols := make([]string, len(ids))
	keys := make([]string, len(ids))
	users := make([]string, len(ids))
	for i, id := range ids {
		cols[i], keys[i] = id.Collection, id.Key
		users[i] = id.UserID
		if users[i] == "" {
			users[i] = systemUserID
		}
	}
	rows, err := db.Query(ctx, `SELECT s.collection, s.key, s.user_id::text, s.value::text, s.version, s.read, s.write, s.create_time, s.update_time
		FROM storage s JOIN unnest($1::text[], $2::text[], $3::uuid[]) WITH ORDINALITY AS k(collection, key, user_id, ord)
		ON s.collection = k.collection AND s.key = k.key AND s.user_id = k.user_id ORDER BY k.ord`, cols, keys, users)
	if err != nil {
		return nil, err
	}
	return scanStorage(rows)
}

func scanStorage(rows pgx.Rows) ([]*StorageObject, error) {
	defer rows.Close()
	var out []*StorageObject
	for rows.Next() {
		o := &StorageObject{}
		var r, w int16
		if err := rows.Scan(&o.Collection, &o.Key, &o.UserID, &o.Value, &o.Version, &r, &w, &o.CreateTime, &o.UpdateTime); err != nil {
			return nil, err
		}
		o.PermissionRead, o.PermissionWrite = int32(r), int32(w)
		out = append(out, o)
	}
	return out, rows.Err()
}

// StorageWriteObjects performs authoritative writes atomically (all or none)
// with Nakama's OCC semantics.
func StorageWriteObjects(ctx context.Context, db *pgxpool.Pool, writes []*StorageWrite) ([]*StorageAck, error) {
	var acks []*StorageAck
	err := ExecuteInTx(ctx, db, func(tx pgx.Tx) error {
		var err error
		acks, err = storageWriteTx(ctx, tx, writes)
		return err
	})
	return acks, err
}

func storageWriteTx(ctx context.Context, tx pgx.Tx, writes []*StorageWrite) ([]*StorageAck, error) {
	// Consistent ordering prevents deadlocks between concurrent writers.
	order := make([]int, len(writes))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		wa, wb := writes[order[a]], writes[order[b]]
		if wa.Collection != wb.Collection {
			return wa.Collection < wb.Collection
		}
		if wa.Key != wb.Key {
			return wa.Key < wb.Key
		}
		return wa.UserID < wb.UserID
	})
	acks := make([]*StorageAck, len(writes))
	for _, idx := range order {
		w := writes[idx]
		owner := w.UserID
		if owner == "" {
			owner = systemUserID
		}
		version := w.expectedVersion()
		var resultVersion string
		switch {
		case w.Version == "*":
			// Create only. A savepoint keeps the transaction usable so the
			// caller sees a clean "rejected" error rather than an aborted tx.
			err := tx.QueryRow(ctx, `INSERT INTO storage (collection, key, user_id, value, version, read, write, create_time, update_time)
				VALUES ($1, $2, $3, $4, $5, $6, $7, now(), now()) ON CONFLICT DO NOTHING RETURNING version`,
				w.Collection, w.Key, owner, w.Value, version, w.PermissionRead, w.PermissionWrite).Scan(&resultVersion)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, errStorageRejectedVersion
			}
			if err != nil {
				return nil, err
			}
		case w.Version != "":
			err := tx.QueryRow(ctx, `UPDATE storage SET value = $4, version = $5, read = $6, write = $7, update_time = now()
				WHERE collection = $1 AND key = $2 AND user_id = $3 AND version = $8 RETURNING version`,
				w.Collection, w.Key, owner, w.Value, version, w.PermissionRead, w.PermissionWrite, w.Version).Scan(&resultVersion)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, errStorageRejectedVersion
			}
			if err != nil {
				return nil, err
			}
		default:
			err := tx.QueryRow(ctx, `INSERT INTO storage (collection, key, user_id, value, version, read, write, create_time, update_time)
				VALUES ($1, $2, $3, $4, $5, $6, $7, now(), now())
				ON CONFLICT (collection, key, user_id) DO UPDATE SET value = $4, version = $5, read = $6, write = $7, update_time = now()
				RETURNING version`,
				w.Collection, w.Key, owner, w.Value, version, w.PermissionRead, w.PermissionWrite).Scan(&resultVersion)
			if err != nil {
				if isPgCode(err, "23503") {
					return nil, &StatusError{Code: CodeInvalidArgument, Message: "Storage write rejected - owner not found.", Cause: err}
				}
				return nil, err
			}
		}
		acks[idx] = &StorageAck{Collection: w.Collection, Key: w.Key, UserID: owner, Version: resultVersion}
	}
	return acks, nil
}

func StorageDeleteObjects(ctx context.Context, db *pgxpool.Pool, ids []StorageObjectID) error {
	return ExecuteInTx(ctx, db, func(tx pgx.Tx) error { return storageDeleteTx(ctx, tx, ids) })
}

func storageDeleteTx(ctx context.Context, tx pgx.Tx, ids []StorageObjectID) error {
	sorted := append([]StorageObjectID(nil), ids...)
	sort.SliceStable(sorted, func(a, b int) bool {
		if sorted[a].Collection != sorted[b].Collection {
			return sorted[a].Collection < sorted[b].Collection
		}
		if sorted[a].Key != sorted[b].Key {
			return sorted[a].Key < sorted[b].Key
		}
		return sorted[a].UserID < sorted[b].UserID
	})
	for _, id := range sorted {
		owner := id.UserID
		if owner == "" {
			owner = systemUserID
		}
		q := "DELETE FROM storage WHERE collection = $1 AND key = $2 AND user_id = $3"
		args := []any{id.Collection, id.Key, owner}
		if id.Version != "" {
			q += " AND version = $4"
			args = append(args, id.Version)
		}
		tag, err := tx.Exec(ctx, q, args...)
		if err != nil {
			return err
		}
		if id.Version != "" && tag.RowsAffected() == 0 {
			return statusErr(CodeInvalidArgument, "Storage delete rejected - not found, version check failed, or permission denied.")
		}
	}
	return nil
}

type storageCursor struct {
	Key    string
	UserID string
	Read   int32
}

// StorageList lists objects in a collection, optionally for one owner, in
// Nakama's ordering (user_id, key) with an opaque cursor.
func StorageList(ctx context.Context, db dbtx, ownerID *string, collection string, limit int, cursor string) ([]*StorageObject, string, error) {
	if limit <= 0 {
		limit = 100
	}
	var c *storageCursor
	if cursor != "" {
		b, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil {
			return nil, "", statusErr(CodeInvalidArgument, "Malformed cursor was used.")
		}
		c = &storageCursor{}
		if err := gob.NewDecoder(bytes.NewReader(b)).Decode(c); err != nil {
			return nil, "", statusErr(CodeInvalidArgument, "Malformed cursor was used.")
		}
	}
	var rows pgx.Rows
	var err error
	cols := `collection, key, user_id::text, value::text, version, read, write, create_time, update_time`
	if ownerID != nil {
		if c != nil {
			rows, err = db.Query(ctx, `SELECT `+cols+` FROM storage WHERE collection = $1 AND user_id = $2 AND key > $3 ORDER BY key LIMIT $4`, collection, *ownerID, c.Key, limit+1)
		} else {
			rows, err = db.Query(ctx, `SELECT `+cols+` FROM storage WHERE collection = $1 AND user_id = $2 ORDER BY key LIMIT $3`, collection, *ownerID, limit+1)
		}
	} else {
		if c != nil {
			rows, err = db.Query(ctx, `SELECT `+cols+` FROM storage WHERE collection = $1 AND (key, user_id) > ($2, $3::uuid) ORDER BY key, user_id LIMIT $4`, collection, c.Key, c.UserID, limit+1)
		} else {
			rows, err = db.Query(ctx, `SELECT `+cols+` FROM storage WHERE collection = $1 ORDER BY key, user_id LIMIT $2`, collection, limit+1)
		}
	}
	if err != nil {
		return nil, "", err
	}
	objs, err := scanStorage(rows)
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(objs) > limit {
		last := objs[limit-1]
		objs = objs[:limit]
		var buf bytes.Buffer
		_ = gob.NewEncoder(&buf).Encode(&storageCursor{Key: last.Key, UserID: last.UserID, Read: last.PermissionRead})
		next = base64.RawURLEncoding.EncodeToString(buf.Bytes())
	}
	return objs, next, nil
}

func validUUID(s string) bool {
	_, err := uuid.FromString(s)
	return err == nil
}
