package server

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

type GoogleProfile struct {
	Sub     string
	Email   string
	Name    string
	Picture string
}

// GoogleVerifier validates Google ID tokens against Google's published
// signing certificates (cached until shortly before they expire).
type GoogleVerifier struct {
	httpc     *http.Client
	clientIDs map[string]bool
	certsURL  string

	mu       sync.RWMutex
	keys     map[string]*rsa.PublicKey
	fetched  time.Time
	validTil time.Time
}

func NewGoogleVerifier(clientIDs []string) *GoogleVerifier {
	ids := map[string]bool{}
	for _, id := range clientIDs {
		if id != "" {
			ids[id] = true
		}
	}
	return &GoogleVerifier{httpc: &http.Client{Timeout: 5 * time.Second}, clientIDs: ids, certsURL: "https://www.googleapis.com/oauth2/v1/certs"}
}

func (g *GoogleVerifier) loadKeys(ctx context.Context) (map[string]*rsa.PublicKey, error) {
	g.mu.RLock()
	if g.keys != nil && time.Now().Before(g.validTil) {
		k := g.keys
		g.mu.RUnlock()
		return k, nil
	}
	g.mu.RUnlock()
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.keys != nil && time.Now().Before(g.validTil) {
		return g.keys, nil
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, g.certsURL, nil)
	resp, err := g.httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("google certs: status %d", resp.StatusCode)
	}
	var certs map[string]string
	if err := json.Unmarshal(body, &certs); err != nil {
		return nil, err
	}
	keys := map[string]*rsa.PublicKey{}
	soonest := time.Now().Add(6 * time.Hour)
	for kid, data := range certs {
		block, _ := pem.Decode([]byte(data))
		if block == nil {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			continue
		}
		if pk, ok := cert.PublicKey.(*rsa.PublicKey); ok {
			keys[kid] = pk
			if cert.NotAfter.Before(soonest) {
				soonest = cert.NotAfter
			}
		}
	}
	if len(keys) == 0 {
		return nil, errors.New("google certs: none usable")
	}
	g.keys = keys
	g.fetched = time.Now()
	g.validTil = soonest.Add(-time.Hour)
	if g.validTil.Before(time.Now().Add(5 * time.Minute)) {
		g.validTil = time.Now().Add(5 * time.Minute)
	}
	return keys, nil
}

func (g *GoogleVerifier) Verify(ctx context.Context, idToken string) (*GoogleProfile, error) {
	keys, err := g.loadKeys(ctx)
	if err != nil {
		return nil, err
	}
	claims := jwt.MapClaims{}
	_, err = jwt.ParseWithClaims(idToken, claims, func(t *jwt.Token) (any, error) {
		if t.Method.Alg() != jwt.SigningMethodRS256.Alg() {
			return nil, errors.New("unexpected signing method")
		}
		kid, _ := t.Header["kid"].(string)
		if k, ok := keys[kid]; ok {
			return k, nil
		}
		return nil, errors.New("unknown key id")
	}, jwt.WithExpirationRequired())
	if err != nil {
		return nil, err
	}
	iss, _ := claims["iss"].(string)
	if iss != "accounts.google.com" && iss != "https://accounts.google.com" {
		return nil, fmt.Errorf("unexpected issuer %q", iss)
	}
	if len(g.clientIDs) > 0 {
		aud, _ := claims["aud"].(string)
		if !g.clientIDs[aud] {
			return nil, fmt.Errorf("unexpected audience %q", aud)
		}
	}
	p := &GoogleProfile{}
	p.Sub, _ = claims["sub"].(string)
	p.Email, _ = claims["email"].(string)
	p.Name, _ = claims["name"].(string)
	p.Picture, _ = claims["picture"].(string)
	if p.Sub == "" {
		return nil, errors.New("missing subject")
	}
	return p, nil
}
