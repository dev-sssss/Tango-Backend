package server

import (
	"net/http/httptest"
	"testing"
	"time"
)

func TestAPIVersionTableKeepsSupportWindow(t *testing.T) {
	if p := NewAPIVersions(apiVersionTable).Validate(); len(p) > 0 {
		t.Fatalf("api version table violates the support window: %v", p)
	}
	bad := NewAPIVersions([]APIVersion{{Name: "vX", Deprecated: date("2027-01-01"), Sunset: date("2027-03-01")}})
	if len(bad.Validate()) == 0 {
		t.Fatal("a sunset two months after deprecation must be rejected")
	}
	if len(NewAPIVersions([]APIVersion{{Name: "vY", Sunset: date("2027-03-01")}}).Validate()) == 0 {
		t.Fatal("a sunset without deprecation must be rejected")
	}
}

func TestV1IsNeverRemovedFromTheTable(t *testing.T) {
	// Every app build in the field speaks v1; removing it requires an explicit,
	// reviewed change to this test as well as to the table.
	v := NewAPIVersions(apiVersionTable)
	if _, ok := v.by["v1"]; !ok {
		t.Fatal("v1 must stay registered")
	}
}

func TestDeprecationHeaders(t *testing.T) {
	v := NewAPIVersions([]APIVersion{{Name: "v1", Deprecated: time.Unix(1800000000, 0), Sunset: time.Unix(1800000000, 0).Add(MinSupportWindow)}})
	w := httptest.NewRecorder()
	v.annotate(w, "v1")
	if w.Header().Get("Deprecation") != "@1800000000" || w.Header().Get("Sunset") == "" || w.Header().Get("X-Tango-Api-Version") != "v1" {
		t.Fatalf("headers: %v", w.Header())
	}
}

func TestListFlags(t *testing.T) {
	cfg, err := LoadConfig([]string{"--socket.address", "127.0.0.1", "--database.address", "a,b", "--cluster.roles", "gateway"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Socket.Address != "127.0.0.1" || len(cfg.Database.Address) != 2 || len(cfg.Cluster.Roles) != 1 || cfg.Cluster.Roles[0] != "gateway" {
		t.Fatalf("parsed: %+v %+v %+v", cfg.Socket.Address, cfg.Database.Address, cfg.Cluster.Roles)
	}
	if _, err := LoadConfig([]string{"--cluster.roles", "matchmaker"}); err == nil {
		t.Fatal("unknown role accepted")
	}
}
