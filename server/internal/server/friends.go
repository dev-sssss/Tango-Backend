package server

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dev-sssss/tango_backend/server/internal/game"
)

// Friends keeps friendships, friend codes, invite rewards, room invites and
// presence (online / in a game / last seen). Additive: none of it changes how
// matches, wallets or older app versions behave.
type Friends struct {
	db      *pgxpool.Pool
	logger  *slog.Logger
	metrics *Metrics

	mu      sync.Mutex
	touched map[string]bool // users seen through the HTTP API since the last flush
}

const (
	friendLimit         = 200
	pendingLimit        = 50
	requestTTL          = 30 * 24 * time.Hour
	roomInviteTTL       = 15 * time.Minute
	referralWindow      = 7 * 24 * time.Hour
	referralDailyCap    = 20
	referralRewardCoins = 1000
	referralRewardGems  = 10
	welcomeCoins        = 500
	onlineWindow        = 3 * time.Minute
)

// friendship.state, from user_id's side.
const (
	stateFriends  = 0
	stateSent     = 1 // user_id asked friend_id
	stateReceived = 2 // friend_id asked user_id
	stateBlocked  = 3 // user_id blocked friend_id
)

// referral.state
const (
	referralJoined   = 0 // waiting for the invited player's first finished game
	referralRewarded = 1
	referralNoReward = 2 // same device already used an invite
)

const codeAlphabet = "23456789ABCDEFGHJKLMNPQRSTUVWXYZ"

var friendCodeRe = regexp.MustCompile(`^[23456789A-HJ-NP-Z]{8}$`)

func NewFriends(db *pgxpool.Pool, logger *slog.Logger, metrics *Metrics) *Friends {
	return &Friends{db: db, logger: logger, metrics: metrics, touched: map[string]bool{}}
}

// Touch marks the user as active now (cheap; written on the next flush).
func (f *Friends) Touch(userID string) {
	if f == nil || !isUUID(userID) {
		return
	}
	f.mu.Lock()
	f.touched[userID] = true
	f.mu.Unlock()
}

// Run flushes presence every 30 s: users seen over HTTP, and every open
// socket of this node (inMatch tells which of those are in a match).
func (f *Friends) Run(ctx context.Context, sockets func() map[string]bool) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			f.flush(ctx, sockets())
		}
	}
}

func (f *Friends) flush(ctx context.Context, sockets map[string]bool) {
	f.mu.Lock()
	seen := f.touched
	f.touched = map[string]bool{}
	f.mu.Unlock()
	users, inMatch := []string{}, []bool{}
	for u, m := range sockets {
		if isUUID(u) {
			users, inMatch = append(users, u), append(inMatch, m)
		}
	}
	for u := range seen {
		if _, ok := sockets[u]; !ok {
			users, inMatch = append(users, u), append(inMatch, false)
		}
	}
	if len(users) == 0 {
		return
	}
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err := f.db.Exec(wctx, `INSERT INTO user_presence (user_id, last_seen, in_match_until)
		SELECT u::uuid, now(), CASE WHEN m THEN now() + interval '90 seconds' END
		FROM unnest($1::text[], $2::bool[]) AS x(u, m)
		WHERE EXISTS (SELECT 1 FROM users WHERE id = u::uuid)
		ON CONFLICT (user_id) DO UPDATE SET last_seen = now(), in_match_until = EXCLUDED.in_match_until`, users, inMatch)
	if err != nil {
		f.metrics.CounterAdd("friends_presence_errors_total", nil, 1)
		f.logger.Warn("presence flush failed", "error", err)
	}
}

// ---------------------------------------------------------------- helpers

type friendRow struct {
	UserID, Username, Name, Avatar string
	Frame                          string
	Trophies                       int
	LastSeen                       time.Time
	InMatch                        bool
	State                          int
	Updated                        time.Time
}

func (r friendRow) public(now time.Time) map[string]any {
	status := "offline"
	if r.InMatch {
		status = "in_game"
	} else if now.Sub(r.LastSeen) < onlineWindow {
		status = "online"
	}
	return map[string]any{"user_id": r.UserID, "username": r.Username, "display_name": r.Name, "avatar_url": r.Avatar,
		"avatar": r.Avatar, "frame": r.Frame, "trophies": r.Trophies, "level": r.Trophies/100 + 1,
		"status": status, "last_seen": r.LastSeen.Unix()}
}

const profileCols = `u.id::text, u.username,
	COALESCE(NULLIF(p.value->>'name', ''), NULLIF(u.display_name, ''), u.username),
	COALESCE(NULLIF(p.value->>'avatar', ''), NULLIF(u.avatar_url, ''), ''),
	COALESCE(p.value->>'frame', ''),
	COALESCE(CASE WHEN jsonb_typeof(p.value->'trophies') = 'number' THEN (p.value->>'trophies')::numeric::int END, 0),
	COALESCE(pr.last_seen, u.update_time), COALESCE(pr.in_match_until > now(), false)`

const profileJoins = `LEFT JOIN storage p ON p.collection = 'profile' AND p.key = 'main' AND p.user_id = u.id
	LEFT JOIN user_presence pr ON pr.user_id = u.id`

func friendsOK(m map[string]any) (map[string]any, error) {
	m["success"] = true
	return m, nil
}

func refuse(code string) (map[string]any, error) {
	return map[string]any{"success": false, "error": code}, nil
}

func newFriendCode() (string, error) {
	b := make([]byte, 8)
	for i := range b {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(codeAlphabet))))
		if err != nil {
			return "", err
		}
		b[i] = codeAlphabet[n.Int64()]
	}
	return string(b), nil
}

func (f *Friends) codeFor(ctx context.Context, userID string) (string, error) {
	var code string
	err := f.db.QueryRow(ctx, `SELECT code FROM friend_code WHERE user_id = $1`, userID).Scan(&code)
	if err == nil {
		return code, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	for attempt := 0; attempt < 5; attempt++ {
		c, err := newFriendCode()
		if err != nil {
			return "", err
		}
		tag, err := f.db.Exec(ctx, `INSERT INTO friend_code (user_id, code) VALUES ($1, $2) ON CONFLICT DO NOTHING`, userID, c)
		if err != nil {
			return "", err
		}
		if tag.RowsAffected() == 1 {
			return c, nil
		}
		if err := f.db.QueryRow(ctx, `SELECT code FROM friend_code WHERE user_id = $1`, userID).Scan(&code); err == nil {
			return code, nil // another request created it first
		}
	}
	return "", errors.New("could not allocate a friend code")
}

func inviteURL(code string) string {
	return playStoreURL() + "&referrer=" + url.QueryEscape("ref="+code+"&utm_source=friend_invite")
}

// pairLock serialises changes to one pair of players.
func pairLock(ctx context.Context, tx pgx.Tx, a, b string) error {
	if a > b {
		a, b = b, a
	}
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "friend:"+a+":"+b)
	return err
}

// ---------------------------------------------------------------- game.Social

func (f *Friends) FriendsList(ctx context.Context, userID string) (map[string]any, error) {
	code, err := f.codeFor(ctx, userID)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	rows, err := f.db.Query(ctx, `SELECT `+profileCols+`, f.state, f.update_time
		FROM friendship f JOIN users u ON u.id = f.friend_id `+profileJoins+`
		WHERE f.user_id = $1 AND (f.state IN (0, 3) OR f.update_time > now() - $2::interval)
		ORDER BY f.update_time DESC LIMIT 600`, userID, fmt.Sprintf("%d seconds", int(requestTTL.Seconds())))
	if err != nil {
		return nil, err
	}
	friends, incoming, outgoing, blocked := []any{}, []any{}, []any{}, []any{}
	for rows.Next() {
		var r friendRow
		if err := rows.Scan(&r.UserID, &r.Username, &r.Name, &r.Avatar, &r.Frame, &r.Trophies, &r.LastSeen, &r.InMatch, &r.State, &r.Updated); err != nil {
			rows.Close()
			return nil, err
		}
		p := r.public(now)
		switch r.State {
		case stateFriends:
			friends = append(friends, p)
		case stateReceived:
			incoming = append(incoming, p)
		case stateSent:
			outgoing = append(outgoing, p)
		case stateBlocked:
			delete(p, "status")
			delete(p, "last_seen")
			blocked = append(blocked, p)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	invites := []any{}
	irows, err := f.db.Query(ctx, `SELECT `+profileCols+`, i.room_code, i.mode, i.create_time
		FROM room_invite i JOIN users u ON u.id = i.from_id `+profileJoins+`
		WHERE i.to_id = $1 AND i.create_time > now() - $2::interval
		  AND EXISTS (SELECT 1 FROM friendship x WHERE x.user_id = $1 AND x.friend_id = i.from_id AND x.state = 0)
		ORDER BY i.create_time DESC LIMIT 20`, userID, fmt.Sprintf("%d seconds", int(roomInviteTTL.Seconds())))
	if err != nil {
		return nil, err
	}
	for irows.Next() {
		var r friendRow
		var room, mode string
		var at time.Time
		if err := irows.Scan(&r.UserID, &r.Username, &r.Name, &r.Avatar, &r.Frame, &r.Trophies, &r.LastSeen, &r.InMatch, &room, &mode, &at); err != nil {
			irows.Close()
			return nil, err
		}
		invites = append(invites, map[string]any{"from": r.public(now), "room_code": room, "mode": mode,
			"expires_at": at.Add(roomInviteTTL).Unix()})
	}
	irows.Close()
	if err := irows.Err(); err != nil {
		return nil, err
	}

	var joined, rewarded, pending int
	if err := f.db.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE state = 1), count(*) FILTER (WHERE state = 0)
		FROM referral WHERE referrer_id = $1`, userID).Scan(&joined, &rewarded, &pending); err != nil {
		return nil, err
	}
	var created time.Time
	var referred bool
	if err := f.db.QueryRow(ctx, `SELECT u.create_time, EXISTS (SELECT 1 FROM referral r WHERE r.invitee_id = u.id)
		FROM users u WHERE u.id = $1`, userID).Scan(&created, &referred); err != nil {
		return nil, err
	}
	return friendsOK(map[string]any{
		"code": code, "invite_url": inviteURL(code),
		"friends": friends, "incoming": incoming, "outgoing": outgoing, "blocked": blocked, "invites": invites,
		"referral": map[string]any{
			"joined": joined, "rewarded": rewarded, "pending": pending,
			"coins_earned": rewarded * referralRewardCoins, "gems_earned": rewarded * referralRewardGems,
			"entered_code": referred, "can_enter_code": !referred && now.Sub(created) < referralWindow,
		},
		"rewards": map[string]any{"coins": referralRewardCoins, "gems": referralRewardGems, "welcome_coins": welcomeCoins},
	})
}

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func (f *Friends) resolve(ctx context.Context, query string) (string, error) {
	var id string
	q := strings.TrimSpace(query)
	if up := strings.ToUpper(q); friendCodeRe.MatchString(up) {
		err := f.db.QueryRow(ctx, `SELECT user_id::text FROM friend_code WHERE code = $1`, up).Scan(&id)
		if err == nil {
			return id, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return "", err
		}
	}
	if uuidRe.MatchString(q) {
		// A player picked from a list (leaderboard, profile): the id itself.
		err := f.db.QueryRow(ctx, `SELECT id::text FROM users WHERE id = $1::uuid
			AND id <> '00000000-0000-0000-0000-000000000000' AND disable_time <= '1970-01-01 00:00:00+00'`, strings.ToLower(q)).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil
		}
		return id, err
	}
	err := f.db.QueryRow(ctx, `SELECT id::text FROM users WHERE lower(username) = lower($1)
		AND id <> '00000000-0000-0000-0000-000000000000' AND disable_time <= '1970-01-01 00:00:00+00'
		ORDER BY username = $1 DESC LIMIT 1`, q).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return id, err
}

func relation(ctx context.Context, tx pgx.Tx, a, b string) (int, bool, error) {
	var s int
	err := tx.QueryRow(ctx, `SELECT state FROM friendship WHERE user_id = $1 AND friend_id = $2`, a, b).Scan(&s)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	return s, err == nil, err
}

func setRelation(ctx context.Context, tx pgx.Tx, a, b string, state int) error {
	_, err := tx.Exec(ctx, `INSERT INTO friendship (user_id, friend_id, state, update_time) VALUES ($1, $2, $3, now())
		ON CONFLICT (user_id, friend_id) DO UPDATE SET state = EXCLUDED.state, update_time = now()`, a, b, state)
	return err
}

func dropRelation(ctx context.Context, tx pgx.Tx, a, b string) error {
	_, err := tx.Exec(ctx, `DELETE FROM friendship WHERE user_id = $1 AND friend_id = $2`, a, b)
	return err
}

func (f *Friends) FriendAdd(ctx context.Context, userID, query string) (map[string]any, error) {
	target, err := f.resolve(ctx, query)
	if err != nil {
		return nil, err
	}
	if target == "" {
		return refuse("not_found")
	}
	if target == userID {
		return refuse("self")
	}
	var result map[string]any
	err = ExecuteInTx(ctx, f.db, func(tx pgx.Tx) error {
		result = nil
		if err := pairLock(ctx, tx, userID, target); err != nil {
			return err
		}
		mine, hasMine, err := relation(ctx, tx, userID, target)
		if err != nil {
			return err
		}
		theirs, hasTheirs, err := relation(ctx, tx, target, userID)
		if err != nil {
			return err
		}
		switch {
		case hasTheirs && theirs == stateBlocked:
			result = map[string]any{"success": false, "error": "not_found"} // never reveal a block
			return nil
		case hasMine && mine == stateBlocked:
			result = map[string]any{"success": false, "error": "blocked"}
			return nil
		case hasMine && mine == stateFriends:
			result = map[string]any{"success": true, "state": "friends"}
			return nil
		case hasMine && mine == stateReceived:
			if err := setRelation(ctx, tx, userID, target, stateFriends); err != nil {
				return err
			}
			if err := setRelation(ctx, tx, target, userID, stateFriends); err != nil {
				return err
			}
			result = map[string]any{"success": true, "state": "friends"}
			return nil
		}
		var friends, pending int
		if err := tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE state = 0), count(*) FILTER (WHERE state = 1 AND update_time > now() - $2::interval)
			FROM friendship WHERE user_id = $1`, userID, fmt.Sprintf("%d seconds", int(requestTTL.Seconds()))).Scan(&friends, &pending); err != nil {
			return err
		}
		if friends >= friendLimit {
			result = map[string]any{"success": false, "error": "friend_limit"}
			return nil
		}
		if pending >= pendingLimit && !(hasMine && mine == stateSent) {
			result = map[string]any{"success": false, "error": "request_limit"}
			return nil
		}
		if err := setRelation(ctx, tx, userID, target, stateSent); err != nil {
			return err
		}
		if err := setRelation(ctx, tx, target, userID, stateReceived); err != nil {
			return err
		}
		result = map[string]any{"success": true, "state": "sent"}
		return nil
	})
	if err != nil {
		return nil, err
	}
	result["user_id"] = target
	return result, nil
}

func (f *Friends) FriendAction(ctx context.Context, userID, otherID, action string) (map[string]any, error) {
	var result map[string]any
	err := ExecuteInTx(ctx, f.db, func(tx pgx.Tx) error {
		result = map[string]any{"success": true}
		if err := pairLock(ctx, tx, userID, otherID); err != nil {
			return err
		}
		mine, hasMine, err := relation(ctx, tx, userID, otherID)
		if err != nil {
			return err
		}
		switch action {
		case "accept":
			if !hasMine || mine != stateReceived {
				result = map[string]any{"success": false, "error": "no_request"}
				return nil
			}
			if err := setRelation(ctx, tx, userID, otherID, stateFriends); err != nil {
				return err
			}
			return setRelation(ctx, tx, otherID, userID, stateFriends)
		case "decline", "cancel", "remove":
			if hasMine && mine == stateBlocked {
				return nil
			}
			if err := dropRelation(ctx, tx, userID, otherID); err != nil {
				return err
			}
			theirs, hasTheirs, err := relation(ctx, tx, otherID, userID)
			if err != nil {
				return err
			}
			if hasTheirs && theirs != stateBlocked {
				return dropRelation(ctx, tx, otherID, userID)
			}
			return nil
		case "block":
			theirs, hasTheirs, err := relation(ctx, tx, otherID, userID)
			if err != nil {
				return err
			}
			if hasTheirs && theirs != stateBlocked {
				if err := dropRelation(ctx, tx, otherID, userID); err != nil {
					return err
				}
			}
			if _, err := tx.Exec(ctx, `DELETE FROM room_invite WHERE (from_id = $1 AND to_id = $2) OR (from_id = $2 AND to_id = $1)`, userID, otherID); err != nil {
				return err
			}
			return setRelation(ctx, tx, userID, otherID, stateBlocked)
		case "unblock":
			if hasMine && mine == stateBlocked {
				return dropRelation(ctx, tx, userID, otherID)
			}
			return nil
		}
		result = map[string]any{"success": false, "error": "invalid_request"}
		return nil
	})
	return result, err
}

func (f *Friends) RoomInvite(ctx context.Context, userID, friendID, roomCode, mode string) (map[string]any, error) {
	if len(roomCode) > 32 || len(mode) > 32 {
		return refuse("invalid_request")
	}
	var friends bool
	if err := f.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM friendship WHERE user_id = $1 AND friend_id = $2 AND state = 0)
		AND EXISTS (SELECT 1 FROM friendship WHERE user_id = $2 AND friend_id = $1 AND state = 0)`, userID, friendID).Scan(&friends); err != nil {
		return nil, err
	}
	if !friends {
		return refuse("not_friends")
	}
	if _, err := f.db.Exec(ctx, `INSERT INTO room_invite (from_id, to_id, room_code, mode, create_time) VALUES ($1, $2, $3, $4, now())
		ON CONFLICT (from_id, to_id) DO UPDATE SET room_code = EXCLUDED.room_code, mode = EXCLUDED.mode, create_time = now()`,
		userID, friendID, roomCode, mode); err != nil {
		return nil, err
	}
	return friendsOK(map[string]any{})
}

func (f *Friends) ReferralClaim(ctx context.Context, userID, code, deviceID, source string) (map[string]any, error) {
	if !friendCodeRe.MatchString(code) {
		return refuse("invalid_code")
	}
	var referrer string
	err := f.db.QueryRow(ctx, `SELECT user_id::text FROM friend_code WHERE code = $1`, code).Scan(&referrer)
	if errors.Is(err, pgx.ErrNoRows) {
		return refuse("invalid_code")
	}
	if err != nil {
		return nil, err
	}
	if referrer == userID {
		return refuse("own_code")
	}
	var result map[string]any
	err = ExecuteInTx(ctx, f.db, func(tx pgx.Tx) error {
		result = nil
		if err := pairLock(ctx, tx, userID, referrer); err != nil {
			return err
		}
		var created time.Time
		var claimed, mutual, deviceUsed bool
		if err := tx.QueryRow(ctx, `SELECT u.create_time,
				EXISTS (SELECT 1 FROM referral WHERE invitee_id = $1),
				EXISTS (SELECT 1 FROM referral WHERE invitee_id = $2 AND referrer_id = $1),
				($3 <> '' AND EXISTS (SELECT 1 FROM referral WHERE device_id = $3))
			FROM users u WHERE u.id = $1`, userID, referrer, deviceID).Scan(&created, &claimed, &mutual, &deviceUsed); err != nil {
			return err
		}
		switch {
		case claimed:
			result = map[string]any{"success": false, "error": "already_claimed"}
			return nil
		case time.Since(created) > referralWindow:
			result = map[string]any{"success": false, "error": "too_late"}
			return nil
		case mutual:
			result = map[string]any{"success": false, "error": "invalid_code"}
			return nil
		}
		state, bonus := referralJoined, int64(welcomeCoins)
		if deviceUsed {
			state, bonus = referralNoReward, 0
		}
		if _, err := tx.Exec(ctx, `INSERT INTO referral (invitee_id, referrer_id, device_id, source, state) VALUES ($1, $2, $3, $4, $5)`,
			userID, referrer, deviceID, source, state); err != nil {
			return err
		}
		if bonus > 0 {
			if _, err := updateWalletsTx(ctx, tx, []*WalletUpdate{{UserID: userID, Changeset: map[string]int64{"coins": bonus},
				Metadata: game.Encode(map[string]any{"reason": "referral_welcome", "referrer_id": referrer})}}, true); err != nil {
				return err
			}
		}
		// The invite makes them friends, unless either has blocked the other.
		var blocked bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM friendship WHERE state = 3 AND
			((user_id = $1 AND friend_id = $2) OR (user_id = $2 AND friend_id = $1)))`, userID, referrer).Scan(&blocked); err != nil {
			return err
		}
		if !blocked {
			if err := setRelation(ctx, tx, userID, referrer, stateFriends); err != nil {
				return err
			}
			if err := setRelation(ctx, tx, referrer, userID, stateFriends); err != nil {
				return err
			}
		}
		result = map[string]any{"success": true, "welcome_coins": bonus, "referrer_id": referrer}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if result["success"] == true {
		f.metrics.CounterAdd("referral_claims_total", map[string]string{"source": source}, 1)
	}
	return result, nil
}

// GamesFinished pays invite rewards for players finishing their first game
// (online or offline). Called off the request path by Analytics.
func (f *Friends) GamesFinished(ctx context.Context, userIDs []string) {
	if f == nil {
		return
	}
	for _, uid := range userIDs {
		if !isUUID(uid) {
			continue
		}
		if err := f.rewardReferrer(ctx, uid); err != nil {
			f.logger.Warn("referral reward failed", "error", err)
		}
	}
}

func (f *Friends) rewardReferrer(ctx context.Context, invitee string) error {
	var pending bool
	if err := f.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM referral WHERE invitee_id = $1 AND state = 0)`, invitee).Scan(&pending); err != nil || !pending {
		return err
	}
	paid := false
	err := ExecuteInTx(ctx, f.db, func(tx pgx.Tx) error {
		paid = false
		var referrer string
		err := tx.QueryRow(ctx, `SELECT referrer_id::text FROM referral WHERE invitee_id = $1 AND state = 0 FOR UPDATE`, invitee).Scan(&referrer)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		var today int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM referral WHERE referrer_id = $1 AND state = 1 AND complete_time > now() - interval '1 day'`,
			referrer).Scan(&today); err != nil {
			return err
		}
		if today >= referralDailyCap {
			return nil // stays pending; paid on a later game
		}
		if _, err := tx.Exec(ctx, `UPDATE referral SET state = 1, complete_time = now() WHERE invitee_id = $1`, invitee); err != nil {
			return err
		}
		if _, err := updateWalletsTx(ctx, tx, []*WalletUpdate{{UserID: referrer,
			Changeset: map[string]int64{"coins": referralRewardCoins, "diamonds": referralRewardGems},
			Metadata:  game.Encode(map[string]any{"reason": "referral_reward", "invitee_id": invitee})}}, true); err != nil {
			return err
		}
		paid = true
		return nil
	})
	if err == nil && paid {
		f.metrics.CounterAdd("referral_rewards_total", nil, 1)
	}
	return err
}

// PgNK forwards the friend RPCs to the server's Friends (game.Social).
func (n *PgNK) FriendsList(ctx context.Context, userID string) (map[string]any, error) {
	return n.friends.FriendsList(ctx, userID)
}
func (n *PgNK) FriendAdd(ctx context.Context, userID, query string) (map[string]any, error) {
	return n.friends.FriendAdd(ctx, userID, query)
}
func (n *PgNK) FriendAction(ctx context.Context, userID, otherID, action string) (map[string]any, error) {
	return n.friends.FriendAction(ctx, userID, otherID, action)
}
func (n *PgNK) ReferralClaim(ctx context.Context, userID, code, deviceID, source string) (map[string]any, error) {
	return n.friends.ReferralClaim(ctx, userID, code, deviceID, source)
}
func (n *PgNK) RoomInvite(ctx context.Context, userID, friendID, roomCode, mode string) (map[string]any, error) {
	return n.friends.RoomInvite(ctx, userID, friendID, roomCode, mode)
}

var _ game.Social = (*PgNK)(nil)

// playStoreURL is the store page used in friend-invite links. Set
// TANGO_PLAY_STORE_URL in the environment once the store listing exists.
func playStoreURL() string {
	if u := os.Getenv("TANGO_PLAY_STORE_URL"); u != "" {
		return u
	}
	return "https://play.google.com/store/apps/details?id=com.sssss.tangocards"
}
