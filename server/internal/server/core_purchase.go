package server

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dev-sssss/tango_backend/server/internal/game"
)

// Google Play consumable validation, matching Nakama's
// ValidatePurchaseGoogle (androidpublisher v3 purchases.products.get).

type GooglePurchases struct {
	cfg   IAPGoogleConfig
	db    *pgxpool.Pool
	httpc *http.Client

	mu      sync.Mutex
	token   string
	expires time.Time
	// Endpoints are overridable for tests.
	TokenURL     string
	PublisherURL string
}

func NewGooglePurchases(cfg IAPGoogleConfig, db *pgxpool.Pool) *GooglePurchases {
	return &GooglePurchases{cfg: cfg, db: db, httpc: &http.Client{Timeout: 10 * time.Second},
		TokenURL: "https://accounts.google.com/o/oauth2/token", PublisherURL: "https://androidpublisher.googleapis.com"}
}

type googleReceipt struct {
	PackageName   string `json:"packageName"`
	ProductID     string `json:"productId"`
	PurchaseState int    `json:"purchaseState"`
	PurchaseTime  int64  `json:"purchaseTime"`
	PurchaseToken string `json:"purchaseToken"`
}

func decodeGoogleReceipt(receipt string) (*googleReceipt, error) {
	var wrapper map[string]any
	if err := json.Unmarshal([]byte(receipt), &wrapper); err != nil {
		return nil, err
	}
	inner, ok := wrapper["json"].(string)
	if !ok {
		inner = receipt
	}
	var gr googleReceipt
	if err := json.Unmarshal([]byte(inner), &gr); err != nil || gr.PackageName == "" {
		return nil, errors.New("receipt is malformed")
	}
	return &gr, nil
}

func (g *GooglePurchases) accessToken(ctx context.Context) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.token != "" && time.Now().Before(g.expires) {
		return g.token, nil
	}
	if g.cfg.ClientEmail == "" || g.cfg.PrivateKey == "" {
		return "", errors.New("google iap credentials are not configured")
	}
	block, _ := pem.Decode([]byte(g.cfg.PrivateKey))
	if block == nil {
		return "", errors.New("google iap private key invalid")
	}
	pk, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return "", err
	}
	now := time.Now()
	claims := jwt.MapClaims{"scope": "https://www.googleapis.com/auth/androidpublisher", "aud": g.TokenURL, "exp": now.Add(time.Hour).Unix(), "iat": now.Unix(), "iss": g.cfg.ClientEmail}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodRS256, claims).SignedString(pk)
	if err != nil {
		return "", err
	}
	form := url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"}, "assertion": {signed}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, g.TokenURL, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := g.httpc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("google auth status %d", resp.StatusCode)
	}
	var tok struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tok); err != nil {
		return "", err
	}
	g.token = tok.AccessToken
	g.expires = time.Now().Add(time.Duration(tok.ExpiresIn-60) * time.Second)
	return g.token, nil
}

func (g *GooglePurchases) Validate(ctx context.Context, userID, receipt string, persist bool) ([]*game.ValidatedPurchase, error) {
	gr, err := decodeGoogleReceipt(receipt)
	if err != nil {
		return nil, err
	}
	tok, err := g.accessToken(ctx)
	if err != nil {
		return nil, err
	}
	u := fmt.Sprintf("%s/androidpublisher/v3/applications/%s/purchases/products/%s/tokens/%s", g.PublisherURL,
		url.PathEscape(gr.PackageName), url.PathEscape(gr.ProductID), url.PathEscape(gr.PurchaseToken))
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/json")
	resp, err := g.httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("google publisher status %d", resp.StatusCode)
	}
	var out struct {
		PurchaseType *int `json:"purchaseType"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	if gr.PurchaseState != 0 {
		return nil, statusErr(CodeFailedPrecondition, "Invalid Receipt. State: "+strconv.Itoa(gr.PurchaseState))
	}
	env := 2 // production
	if out.PurchaseType != nil && *out.PurchaseType == 0 {
		env = 1 // sandbox
	}
	ptime := time.UnixMilli(gr.PurchaseTime).UTC()
	if !persist {
		return []*game.ValidatedPurchase{{UserID: userID, ProductID: gr.ProductID, TransactionID: gr.PurchaseToken, ProviderResponse: string(raw)}}, nil
	}
	var storedUser string
	var refund time.Time
	var seen bool
	err = g.db.QueryRow(ctx, `INSERT INTO purchase (user_id, store, transaction_id, product_id, purchase_time, raw_response, environment, create_time, update_time)
		VALUES ($1, 1, $2, $3, $4, $5, $6, now(), now())
		ON CONFLICT (transaction_id) DO UPDATE SET purchase_time = $4, update_time = now()
		RETURNING user_id::text, refund_time, (xmax <> 0) AS seen`, userID, gr.PurchaseToken, gr.ProductID, ptime, raw, env).Scan(&storedUser, &refund, &seen)
	if err != nil {
		return nil, err
	}
	if storedUser == systemUserID {
		storedUser = ""
	}
	vp := &game.ValidatedPurchase{UserID: storedUser, ProductID: gr.ProductID, TransactionID: gr.PurchaseToken, ProviderResponse: string(raw), SeenBefore: seen}
	if refund.Unix() > 0 {
		vp.RefundTime = refund.Unix()
	}
	return []*game.ValidatedPurchase{vp}, nil
}
