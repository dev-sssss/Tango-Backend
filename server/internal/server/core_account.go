package server

import (
	"context"
	"errors"
	"math/rand/v2"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

const bcryptHashCost = 13 // matches Nakama so stored hashes are interchangeable

var (
	invalidUsernameRegex = regexp.MustCompilePOSIX("([[:cntrl:]]|[[\t\n\r\f\v]])+")
	invalidCharsRegex    = regexp.MustCompilePOSIX("([[:cntrl:]]|[[:space:]])+")
	emailRegex           = regexp.MustCompile(`^.+@.+\..+$`)
)

type Account struct {
	UserID       string
	Username     string
	DisplayName  string
	AvatarURL    string
	LangTag      string
	Location     string
	Timezone     string
	Metadata     string
	Wallet       string
	Email        string
	GoogleID     string
	FacebookID   string
	AppleID      string
	GamecenterID string
	SteamID      string
	CustomID     string
	EdgeCount    int
	CreateTime   time.Time
	UpdateTime   time.Time
	VerifyTime   time.Time
	DisableTime  time.Time
	Devices      []string
}

func generateUsername() string {
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
	b := make([]byte, 10)
	for i := range b {
		b[i] = alphabet[rand.IntN(len(alphabet))]
	}
	return string(b)
}

func validateUsername(username string) error {
	if invalidUsernameRegex.MatchString(username) {
		return statusErr(CodeInvalidArgument, "Username invalid, no spaces or control characters allowed.")
	}
	if len(username) > 128 {
		return statusErr(CodeInvalidArgument, "Username invalid, must be 1-128 bytes.")
	}
	return nil
}

// AuthenticateDevice finds or creates the account linked to a device ID.
func AuthenticateDevice(ctx context.Context, db *pgxpool.Pool, deviceID, username string, create bool) (userID, outUsername string, created bool, err error) {
	if deviceID == "" {
		return "", "", false, statusErr(CodeInvalidArgument, "Device ID is required.")
	}
	if invalidCharsRegex.MatchString(deviceID) {
		return "", "", false, statusErr(CodeInvalidArgument, "Device ID invalid, no spaces or control characters allowed.")
	}
	if len(deviceID) < 10 || len(deviceID) > 128 {
		return "", "", false, statusErr(CodeInvalidArgument, "Device ID invalid, must be 10-128 bytes.")
	}
	if username == "" {
		username = generateUsername()
	} else if err := validateUsername(username); err != nil {
		return "", "", false, err
	}

	var dbUserID, dbUsername string
	var disable time.Time
	err = db.QueryRow(ctx, `SELECT u.id::text, u.username, u.disable_time FROM users u JOIN user_device d ON d.user_id = u.id WHERE d.id = $1`, deviceID).Scan(&dbUserID, &dbUsername, &disable)
	if err == nil {
		if disable.Unix() != 0 {
			return "", "", false, errAccountDisabled
		}
		return dbUserID, dbUsername, false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", "", false, &StatusError{Code: CodeInternal, Message: "Error finding user account.", Cause: err}
	}
	if !create {
		return "", "", false, errUserNotFound
	}
	newID := uuid.Must(uuid.NewV4()).String()
	err = ExecuteInTx(ctx, db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO users (id, username, create_time, update_time) VALUES ($1, $2, now(), now())`, newID, username); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO user_device (id, user_id) VALUES ($1, $2)`, deviceID, newID)
		return err
	})
	if err != nil {
		if isPgCode(err, pgUniqueViolation) {
			if strings.Contains(err.Error(), "users_username_key") {
				return "", "", false, errUsernameInUse
			}
			// Concurrent create of the same device: fall back to lookup.
			return AuthenticateDevice(ctx, db, deviceID, "", false)
		}
		return "", "", false, &StatusError{Code: CodeInternal, Message: "Error finding or creating user account.", Cause: err}
	}
	return newID, username, true, nil
}

func normalizeEmail(email string) (string, error) {
	email = strings.ToLower(email)
	if invalidCharsRegex.MatchString(email) {
		return "", statusErr(CodeInvalidArgument, "Invalid email address, no spaces or control characters allowed.")
	}
	if !emailRegex.MatchString(email) {
		return "", statusErr(CodeInvalidArgument, "Invalid email address format.")
	}
	if len(email) < 10 || len(email) > 255 {
		return "", statusErr(CodeInvalidArgument, "Invalid email address, must be 10-255 bytes.")
	}
	return email, nil
}

func AuthenticateEmail(ctx context.Context, db *pgxpool.Pool, email, password, username string, create bool) (userID, outUsername string, created bool, err error) {
	if email == "" || password == "" {
		return "", "", false, statusErr(CodeInvalidArgument, "Email address and password is required.")
	}
	if email, err = normalizeEmail(email); err != nil {
		return "", "", false, err
	}
	if len(password) < 8 {
		return "", "", false, statusErr(CodeInvalidArgument, "Password must be at least 8 characters long.")
	}
	if username == "" {
		username = generateUsername()
	} else if err := validateUsername(username); err != nil {
		return "", "", false, err
	}
	var dbUserID, dbUsername string
	var dbPassword []byte
	var disable time.Time
	err = db.QueryRow(ctx, `SELECT id::text, username, password, disable_time FROM users WHERE email = $1`, email).Scan(&dbUserID, &dbUsername, &dbPassword, &disable)
	if err == nil {
		if disable.Unix() != 0 {
			return "", "", false, errAccountDisabled
		}
		if bcrypt.CompareHashAndPassword(dbPassword, []byte(password)) != nil {
			return "", "", false, errInvalidCredentials
		}
		return dbUserID, dbUsername, false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", "", false, &StatusError{Code: CodeInternal, Message: "Error finding user account.", Cause: err}
	}
	if !create {
		return "", "", false, errUserNotFound
	}
	hashed, err := bcrypt.GenerateFromPassword([]byte(password), bcryptHashCost)
	if err != nil {
		return "", "", false, &StatusError{Code: CodeInternal, Message: "Error finding or creating user account.", Cause: err}
	}
	newID := uuid.Must(uuid.NewV4()).String()
	_, err = db.Exec(ctx, `INSERT INTO users (id, username, email, password, create_time, update_time) VALUES ($1, $2, $3, $4, now(), now())`, newID, username, email, hashed)
	if err != nil {
		if isPgCode(err, pgUniqueViolation) {
			if strings.Contains(err.Error(), "users_username_key") {
				return "", "", false, errUsernameInUse
			}
			return "", "", false, statusErr(CodeAlreadyExists, "Email is already in use.")
		}
		return "", "", false, &StatusError{Code: CodeInternal, Message: "Error finding or creating user account.", Cause: err}
	}
	return newID, username, true, nil
}

// AuthenticateGoogle finds or creates the account linked to a verified Google
// profile, filling display name / avatar if they were empty (Nakama parity).
func AuthenticateGoogle(ctx context.Context, db *pgxpool.Pool, profile *GoogleProfile, username string, create bool) (userID, outUsername string, created bool, err error) {
	if username == "" {
		username = generateUsername()
	} else if err := validateUsername(username); err != nil {
		return "", "", false, err
	}
	displayName := profile.Name
	if len(displayName) > 255 {
		displayName = ""
	}
	avatar := profile.Picture
	if len(avatar) > 512 {
		avatar = ""
	}
	var dbUserID, dbUsername string
	var disable time.Time
	var dbDisplay, dbAvatar *string
	err = db.QueryRow(ctx, `SELECT id::text, username, disable_time, display_name, avatar_url FROM users WHERE google_id = $1`, profile.Sub).Scan(&dbUserID, &dbUsername, &disable, &dbDisplay, &dbAvatar)
	if err == nil {
		if disable.Unix() != 0 {
			return "", "", false, errAccountDisabled
		}
		var sets []string
		args := []any{dbUserID}
		if (dbDisplay == nil || *dbDisplay == "") && displayName != "" {
			args = append(args, displayName)
			sets = append(sets, "display_name = $2")
		}
		if (dbAvatar == nil || *dbAvatar == "") && avatar != "" {
			args = append(args, avatar)
			sets = append(sets, "avatar_url = $"+itoa(len(args)))
		}
		if len(sets) > 0 {
			_, _ = db.Exec(ctx, "UPDATE users SET "+strings.Join(sets, ", ")+", update_time = now() WHERE id = $1", args...)
		}
		return dbUserID, dbUsername, false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", "", false, &StatusError{Code: CodeInternal, Message: "Error finding user account.", Cause: err}
	}
	if !create {
		return "", "", false, errUserNotFound
	}
	newID := uuid.Must(uuid.NewV4()).String()
	_, err = db.Exec(ctx, `INSERT INTO users (id, username, google_id, display_name, avatar_url, create_time, update_time) VALUES ($1, $2, $3, NULLIF($4, ''), NULLIF($5, ''), now(), now())`, newID, username, profile.Sub, displayName, avatar)
	if err != nil {
		if isPgCode(err, pgUniqueViolation) {
			if strings.Contains(err.Error(), "users_username_key") {
				return "", "", false, errUsernameInUse
			}
			return AuthenticateGoogle(ctx, db, profile, "", false)
		}
		return "", "", false, &StatusError{Code: CodeInternal, Message: "Error finding or creating user account.", Cause: err}
	}
	return newID, username, true, nil
}

func GetAccount(ctx context.Context, db dbtx, userID string) (*Account, error) {
	if _, err := uuid.FromString(userID); err != nil {
		return nil, statusErr(CodeInvalidArgument, "Invalid user ID.")
	}
	a := &Account{UserID: userID}
	var display, avatar, location, timezone, email, google, facebook, apple, gamecenter, steam, custom *string
	err := db.QueryRow(ctx, `SELECT username, display_name, avatar_url, lang_tag, location, timezone, metadata::text, wallet::text,
		email, google_id, facebook_id, apple_id, gamecenter_id, steam_id, custom_id, edge_count, create_time, update_time, verify_time, disable_time
		FROM users WHERE id = $1`, userID).Scan(&a.Username, &display, &avatar, &a.LangTag, &location, &timezone, &a.Metadata, &a.Wallet,
		&email, &google, &facebook, &apple, &gamecenter, &steam, &custom, &a.EdgeCount, &a.CreateTime, &a.UpdateTime, &a.VerifyTime, &a.DisableTime)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errUserNotFound
	}
	if err != nil {
		return nil, err
	}
	a.DisplayName, a.AvatarURL, a.Location, a.Timezone = deref(display), deref(avatar), deref(location), deref(timezone)
	a.Email, a.GoogleID, a.FacebookID, a.AppleID, a.GamecenterID, a.SteamID, a.CustomID = deref(email), deref(google), deref(facebook), deref(apple), deref(gamecenter), deref(steam), deref(custom)
	rows, err := db.Query(ctx, `SELECT id FROM user_device WHERE user_id = $1`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		a.Devices = append(a.Devices, d)
	}
	return a, rows.Err()
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

type AccountUpdate struct {
	UserID      string
	Username    string
	DisplayName *string
	Timezone    *string
	Location    *string
	LangTag     *string
	AvatarURL   *string
	Metadata    *string
}

func UpdateAccounts(ctx context.Context, tx dbtx, updates []*AccountUpdate) error {
	for _, u := range updates {
		var sets []string
		args := []any{u.UserID}
		add := func(col string, v any) {
			args = append(args, v)
			sets = append(sets, col+" = $"+itoa(len(args)))
		}
		if u.Username != "" {
			if err := validateUsername(u.Username); err != nil {
				return err
			}
			add("username", u.Username)
		}
		if u.DisplayName != nil {
			if *u.DisplayName == "" {
				sets = append(sets, "display_name = NULL")
			} else {
				add("display_name", *u.DisplayName)
			}
		}
		if u.Timezone != nil {
			add("timezone", *u.Timezone)
		}
		if u.Location != nil {
			add("location", *u.Location)
		}
		if u.LangTag != nil {
			add("lang_tag", *u.LangTag)
		}
		if u.AvatarURL != nil {
			add("avatar_url", *u.AvatarURL)
		}
		if u.Metadata != nil {
			add("metadata", *u.Metadata)
		}
		if len(sets) == 0 {
			continue
		}
		tag, err := tx.Exec(ctx, "UPDATE users SET "+strings.Join(sets, ", ")+", update_time = now() WHERE id = $1", args...)
		if err != nil {
			if isPgCode(err, pgUniqueViolation) {
				return errUsernameInUse
			}
			return err
		}
		if tag.RowsAffected() == 0 {
			return errUserNotFound
		}
	}
	return nil
}

func LinkEmail(ctx context.Context, db *pgxpool.Pool, userID, email, password string) error {
	if email == "" || password == "" {
		return statusErr(CodeInvalidArgument, "Email address and password is required.")
	}
	email, err := normalizeEmail(email)
	if err != nil {
		return err
	}
	if len(password) < 8 {
		return statusErr(CodeInvalidArgument, "Password must be at least 8 characters long.")
	}
	hashed, err := bcrypt.GenerateFromPassword([]byte(password), bcryptHashCost)
	if err != nil {
		return err
	}
	tag, err := db.Exec(ctx, `UPDATE users AS u SET email = $2, password = $3, update_time = now()
		WHERE (id = $1) AND (NOT EXISTS (SELECT id FROM users WHERE email = $2 AND NOT id = $1))`, userID, email, hashed)
	if err != nil {
		if isPgCode(err, pgUniqueViolation) {
			return statusErr(CodeAlreadyExists, "Email is already in use.")
		}
		return err
	}
	if tag.RowsAffected() == 0 {
		return statusErr(CodeAlreadyExists, "Email is already in use.")
	}
	return nil
}

func DeleteAccount(ctx context.Context, db *pgxpool.Pool, userID string) error {
	if userID == uuid.Nil.String() {
		return statusErr(CodeInvalidArgument, "Cannot delete the system user.")
	}
	return ExecuteInTx(ctx, db, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM users WHERE id = $1`, userID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return nil
		}
		_, err = tx.Exec(ctx, `INSERT INTO user_tombstone (user_id) VALUES ($1) ON CONFLICT(user_id) DO NOTHING`, userID)
		return err
	})
}

type UserSummary struct {
	ID, Username, DisplayName, AvatarURL, LangTag, Location, Timezone, Metadata string
	GoogleID, FacebookID, AppleID, GamecenterID, SteamID                        string
	EdgeCount                                                                   int
	CreateTime, UpdateTime                                                      time.Time
}

func GetUsers(ctx context.Context, db dbtx, ids, usernames []string) ([]*UserSummary, error) {
	rows, err := db.Query(ctx, `SELECT id::text, username, display_name, avatar_url, lang_tag, location, timezone, metadata::text,
		google_id, facebook_id, apple_id, gamecenter_id, steam_id, edge_count, create_time, update_time
		FROM users WHERE (id = ANY($1::uuid[]) OR username = ANY($2::text[])) AND id <> '00000000-0000-0000-0000-000000000000'`, ids, usernames)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*UserSummary
	for rows.Next() {
		u := &UserSummary{}
		var display, avatar, location, timezone, google, facebook, apple, gamecenter, steam *string
		if err := rows.Scan(&u.ID, &u.Username, &display, &avatar, &u.LangTag, &location, &timezone, &u.Metadata,
			&google, &facebook, &apple, &gamecenter, &steam, &u.EdgeCount, &u.CreateTime, &u.UpdateTime); err != nil {
			return nil, err
		}
		u.DisplayName, u.AvatarURL, u.Location, u.Timezone = deref(display), deref(avatar), deref(location), deref(timezone)
		u.GoogleID, u.FacebookID, u.AppleID, u.GamecenterID, u.SteamID = deref(google), deref(facebook), deref(apple), deref(gamecenter), deref(steam)
		out = append(out, u)
	}
	return out, rows.Err()
}

func itoa(i int) string { return strconv.Itoa(i) }
