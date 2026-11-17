package server

import (
	"context"
	"testing"
	"time"
)

func TestLinkGoogleKeepsGuestProgress(t *testing.T) {
	db := friendsTestDB(t, "tango_link_test")
	ctx := context.Background()
	guest, other := "44444444-4444-4444-8444-444444444444", "55555555-5555-4555-8555-555555555555"
	addUser(t, db, guest, "guest1", time.Hour)
	addUser(t, db, other, "other1", time.Hour)
	g := &GoogleProfile{Sub: "google-sub-1", Name: "Robin", Picture: "https://example.com/a.png"}
	if err := LinkGoogle(ctx, db, guest, g); err != nil {
		t.Fatal(err)
	}
	// Linking again is fine; signing in with Google now opens the guest's account.
	if err := LinkGoogle(ctx, db, guest, g); err != nil {
		t.Fatal(err)
	}
	uid, _, created, err := AuthenticateGoogle(ctx, db, g, "", true)
	if err != nil || uid != guest || created {
		t.Fatalf("google sign-in: %s created=%v err=%v", uid, created, err)
	}
	// Another player cannot take the same Google account.
	err = LinkGoogle(ctx, db, other, g)
	if se, ok := err.(*StatusError); !ok || se.Code != CodeAlreadyExists {
		t.Fatalf("want AlreadyExists, got %v", err)
	}
	a, err := GetAccount(ctx, db, guest)
	if err != nil || a.GoogleID != "google-sub-1" || a.DisplayName != "Robin" {
		t.Fatalf("account %+v %v", a, err)
	}
}
