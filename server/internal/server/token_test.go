package server

import "testing"

// Players signed in on Nakama keep working after the switch: their session
// and refresh tokens are signed with Nakama's keys, which the Go server
// accepts for verification only while they are listed as legacy keys.
func TestLegacyKeysCarryNakamaSessionsAcross(t *testing.T) {
	old := &Config{}
	old.Session.EncryptionKey = "defaultencryptionkey"
	old.Session.RefreshEncryptionKey = "defaultrefreshencryptionkey"
	old.Session.TokenExpirySec, old.Session.RefreshTokenExpirySec = 3600, 3600
	token, refresh, _ := NewTokenService(old).Issue("11111111-1111-1111-1111-111111111111", "p", nil)

	cfg := &Config{}
	cfg.Session.EncryptionKey = "new-session-key-new-session-key-xx"
	cfg.Session.RefreshEncryptionKey = "new-refresh-key-new-refresh-key-xx"
	cfg.Session.TokenExpirySec, cfg.Session.RefreshTokenExpirySec = 3600, 3600
	svc := NewTokenService(cfg)
	if _, err := svc.Verify(token); err == nil {
		t.Fatal("old session accepted without a legacy key")
	}
	if _, err := svc.VerifyRefresh(refresh); err == nil {
		t.Fatal("old refresh accepted without a legacy key")
	}

	cfg.Session.LegacyEncryptionKeys = []string{"defaultencryptionkey"}
	cfg.Session.LegacyRefreshEncryptionKeys = []string{"defaultrefreshencryptionkey"}
	if _, err := svc.Verify(token); err != nil {
		t.Fatalf("session: %v", err)
	}
	if _, err := svc.VerifyRefresh(refresh); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	// A legacy session key must not be accepted for refresh, or the other way.
	if _, err := svc.VerifyRefresh(token); err == nil {
		t.Fatal("session token accepted as refresh token")
	}
	// New tokens are signed with the new keys only.
	nt, _, _ := svc.Issue("11111111-1111-1111-1111-111111111111", "p", nil)
	if _, err := NewTokenService(old).Verify(nt); err == nil {
		t.Fatal("new token verifies with the old key")
	}
}
