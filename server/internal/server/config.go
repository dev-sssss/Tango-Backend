package server

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config mirrors the subset of Nakama's YAML/flag configuration that the Tango
// deployment uses, so existing config files and command lines keep working.
// Unknown Nakama keys are ignored.
type Config struct {
	Name     string         `yaml:"name"`
	Logger   LoggerConfig   `yaml:"logger"`
	Database DatabaseConfig `yaml:"database"`
	Session  SessionConfig  `yaml:"session"`
	Socket   SocketConfig   `yaml:"socket"`
	Runtime  RuntimeConfig  `yaml:"runtime"`
	Match    MatchConfig    `yaml:"match"`
	IAP      IAPConfig      `yaml:"iap"`
	Social   SocialConfig   `yaml:"social"`
	Metrics  MetricsConfig  `yaml:"metrics"`
	Cluster  ClusterConfig  `yaml:"cluster"`
	Security SecurityConfig `yaml:"security"`
}

type LoggerConfig struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

type DatabaseConfig struct {
	// Address accepts Nakama style "user:pass@host:port/db" entries or full postgres:// URLs.
	Address        []string `yaml:"address"`
	MaxOpenConns   int      `yaml:"max_open_conns"`
	ConnMaxLifeSec int      `yaml:"conn_max_lifetime_ms"`
}

type SessionConfig struct {
	EncryptionKey         string `yaml:"encryption_key"`
	TokenExpirySec        int64  `yaml:"token_expiry_sec"`
	RefreshEncryptionKey  string `yaml:"refresh_encryption_key"`
	RefreshTokenExpirySec int64  `yaml:"refresh_token_expiry_sec"`
	// LegacyEncryptionKeys are accepted for verification only. Use them for a
	// short window while rotating away from an old key (e.g. Nakama's default).
	LegacyEncryptionKeys []string `yaml:"legacy_encryption_keys"`
	// LegacyRefreshEncryptionKeys do the same for refresh tokens, so players
	// signed in on the previous backend refresh without signing in again.
	LegacyRefreshEncryptionKeys []string `yaml:"legacy_refresh_encryption_keys"`
}

type SocketConfig struct {
	ServerKey           string `yaml:"server_key"`
	Address             string `yaml:"address"`
	Port                int    `yaml:"port"`
	MaxMessageSizeBytes int64  `yaml:"max_message_size_bytes"`
	MaxRequestSizeBytes int64  `yaml:"max_request_size_bytes"`
	ReadTimeoutMs       int    `yaml:"read_timeout_ms"`
	WriteTimeoutMs      int    `yaml:"write_timeout_ms"`
	IdleTimeoutMs       int    `yaml:"idle_timeout_ms"`
	WriteWaitMs         int    `yaml:"write_wait_ms"`
	PongWaitMs          int    `yaml:"pong_wait_ms"`
	PingPeriodMs        int    `yaml:"ping_period_ms"`
	OutgoingQueueSize   int    `yaml:"outgoing_queue_size"`
	// TrustedProxyHeader is used to read the real client address behind Nginx.
	TrustedProxyHeader string `yaml:"trusted_proxy_header"`
}

type RuntimeConfig struct {
	Path    string   `yaml:"path"`
	Env     []string `yaml:"env"`
	HTTPKey string   `yaml:"http_key"`
	// MinCount/MaxCount size the pool of Lua VMs used for RPCs and hooks.
	MinCount int `yaml:"lua_min_count"`
	MaxCount int `yaml:"lua_max_count"`
	// ExcludeModules never load even if present in Path (defence in depth for
	// one-off scripts such as update_wallet.lua).
	ExcludeModules []string `yaml:"exclude_modules"`
	// PublicRPCs may be called with only the server key (Basic auth), no user
	// session and no HTTP key. Use this for pre-login flows such as password
	// reset instead of shipping the HTTP key inside the app.
	PublicRPCs []string `yaml:"public_rpcs"`
	// HTTPRequestTimeoutCapMs caps nk.http_request timeouts.
	HTTPRequestTimeoutCapMs int `yaml:"http_request_timeout_cap_ms"`
}

type MatchConfig struct {
	InputQueueSize        int `yaml:"input_queue_size"`
	CallQueueSize         int `yaml:"call_queue_size"`
	JoinAttemptQueueSize  int `yaml:"join_attempt_queue_size"`
	MaxEmptySec           int `yaml:"max_empty_sec"`
	LabelUpdateIntervalMs int `yaml:"label_update_interval_ms"`
}

type IAPConfig struct {
	Google IAPGoogleConfig `yaml:"google"`
}

type IAPGoogleConfig struct {
	ClientEmail string `yaml:"client_email"`
	PrivateKey  string `yaml:"private_key"`
}

type SocialConfig struct {
	Google SocialGoogleConfig `yaml:"google"`
}

type SocialGoogleConfig struct {
	// ClientIDs restricts accepted Google ID token audiences. Empty accepts any
	// audience (Nakama's default behaviour); set it in production.
	ClientIDs []string `yaml:"client_ids"`
}

type MetricsConfig struct {
	// Address the metrics listener binds to (default: socket.address).
	Address        string `yaml:"address"`
	PrometheusPort int    `yaml:"prometheus_port"`
	Namespace      string `yaml:"namespace"`
}

type ClusterConfig struct {
	// Address is the host:port other nodes use to reach this node's cluster
	// bus. Defaults to <hostname>:<bus port>.
	AdvertiseAddress string `yaml:"advertise_address"`
	BusPort          int    `yaml:"bus_port"`
	// Secret authenticates node-to-node bus connections. Required when more
	// than one node runs.
	Secret string `yaml:"secret"`
	// HeartbeatMs is how often a node renews its liveness row.
	HeartbeatMs int `yaml:"heartbeat_ms"`
	// NodeTimeoutMs after which a silent node is considered dead and its
	// matches are taken over.
	NodeTimeoutMs int `yaml:"node_timeout_ms"`
	// CheckpointIntervalMs controls match state checkpoints, which let another
	// node resume a match if its node crashes. Checkpoints are written only
	// when the state changed: 0 writes every change (a crash loses at most
	// the writes in flight), > 0 writes a match at most this often, < 0
	// disables checkpoints and crash failover. Live handover during deploys
	// does not depend on checkpoints.
	CheckpointIntervalMs int `yaml:"checkpoint_interval_ms"`
	// ResyncOpCode/ResyncData, when set, are injected into a match loop on
	// behalf of every surviving presence after a failover takeover so the game
	// can re-send a full snapshot. Tango uses op 9 {"action":"REQUEST_SNAPSHOT"}.
	ResyncOpCode int64  `yaml:"resync_op_code"`
	ResyncData   string `yaml:"resync_data"`
	// DrainTimeoutMs bounds how long a gracefully stopping node waits for its
	// matches to be handed over.
	DrainTimeoutMs int `yaml:"drain_timeout_ms"`
	// Roles this node plays: "gateway" (client HTTP/WebSocket) and/or "game"
	// (hosts matches). Default both, which is a single-node deployment.
	Roles []string `yaml:"roles"`
	// AdminPort serves /admin (drain, cluster state, API usage). Requests
	// need "Authorization: Bearer <cluster.secret>". 0 disables it.
	AdminPort int `yaml:"admin_port"`
	// AdminAddress is where the admin listener binds; loopback by default so
	// it is never reachable from the internet.
	AdminAddress string `yaml:"admin_address"`
	// BusAddress is where the cluster bus listens (default: socket.address).
	// Use a private interface; peers must reach it, the internet must not.
	BusAddress string `yaml:"bus_address"`
	// PresenceGraceMs keeps a seat for a session whose gateway is restarting.
	PresenceGraceMs int `yaml:"presence_grace_ms"`
}

func NewConfig() *Config {
	return &Config{
		Name:   "",
		Logger: LoggerConfig{Level: "info", Format: "json"},
		Database: DatabaseConfig{
			Address:      []string{"root@localhost:26257"},
			MaxOpenConns: 100,
		},
		Session: SessionConfig{
			EncryptionKey:         "defaultencryptionkey",
			TokenExpirySec:        60,
			RefreshEncryptionKey:  "defaultrefreshencryptionkey",
			RefreshTokenExpirySec: 3600,
		},
		Socket: SocketConfig{
			ServerKey:           "defaultkey",
			Port:                7350,
			MaxMessageSizeBytes: 4096,
			MaxRequestSizeBytes: 262144,
			ReadTimeoutMs:       10000,
			WriteTimeoutMs:      10000,
			IdleTimeoutMs:       60000,
			WriteWaitMs:         5000,
			PongWaitMs:          25000,
			PingPeriodMs:        15000,
			OutgoingQueueSize:   64,
			TrustedProxyHeader:  "X-Forwarded-For",
		},
		Runtime: RuntimeConfig{
			Path:                    "./data/modules",
			HTTPKey:                 "defaulthttpkey",
			MinCount:                16,
			MaxCount:                48,
			HTTPRequestTimeoutCapMs: 5000,
		},
		Match: MatchConfig{
			InputQueueSize:        128,
			CallQueueSize:         128,
			JoinAttemptQueueSize:  128,
			MaxEmptySec:           0,
			LabelUpdateIntervalMs: 0,
		},
		Metrics:  MetricsConfig{PrometheusPort: 9100, Namespace: ""},
		Security: defaultSecurity(),
		Cluster: ClusterConfig{
			BusPort:              7352,
			HeartbeatMs:          1000,
			NodeTimeoutMs:        6000,
			CheckpointIntervalMs: 0,
			DrainTimeoutMs:       15000,
			Roles:                []string{"gateway", "game"},
			AdminPort:            7351,
			AdminAddress:         "127.0.0.1",
			PresenceGraceMs:      10000,
			ResyncOpCode:         9,
			ResyncData:           `{"action":"REQUEST_SNAPSHOT"}`,
		},
	}
}

// LoadConfig reads optional --config files (YAML) and then applies Nakama-style
// "--section.key value" flags on top. Repeated --config flags are merged in
// order, as Nakama does.
func LoadConfig(args []string) (*Config, error) {
	merged := map[string]any{}
	var flagPairs [][2]string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "--") {
			return nil, fmt.Errorf("unexpected argument %q", a)
		}
		key := strings.TrimPrefix(a, "--")
		var val string
		if eq := strings.IndexByte(key, '='); eq >= 0 {
			key, val = key[:eq], key[eq+1:]
		} else {
			if i+1 >= len(args) {
				return nil, fmt.Errorf("flag %q needs a value", a)
			}
			i++
			val = args[i]
		}
		if key == "config" {
			b, err := os.ReadFile(val)
			if err != nil {
				return nil, fmt.Errorf("read config %s: %w", val, err)
			}
			var m map[string]any
			if err := yaml.Unmarshal(b, &m); err != nil {
				return nil, fmt.Errorf("parse config %s: %w", val, err)
			}
			deepMerge(merged, m)
			continue
		}
		flagPairs = append(flagPairs, [2]string{key, val})
	}
	for _, p := range flagPairs {
		setPath(merged, strings.Split(p[0], "."), p[1])
	}
	cfg := NewConfig()
	b, err := yaml.Marshal(merged)
	if err != nil {
		return nil, err
	}
	if err := yaml.Unmarshal(b, cfg); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func deepMerge(dst, src map[string]any) {
	for k, v := range src {
		if sm, ok := v.(map[string]any); ok {
			if dm, ok := dst[k].(map[string]any); ok {
				deepMerge(dm, sm)
				continue
			}
		}
		dst[k] = v
	}
}

// setPath stores a flag value, converting scalars so YAML decoding into typed
// fields works. List fields accept comma separated values or repeated flags.
// listFlags are the list-valued settings (comma separated or repeated).
var listFlags = map[string]bool{
	"database.address": true, "runtime.env": true, "runtime.exclude_modules": true, "runtime.public_rpcs": true,
	"social.google.client_ids": true, "session.legacy_encryption_keys": true, "session.legacy_refresh_encryption_keys": true, "cluster.roles": true,
	"security.trusted_proxies": true,
}

func setPath(m map[string]any, path []string, val string) {
	for i, p := range path {
		if i == len(path)-1 {
			if listFlags[strings.Join(path, ".")] {
				existing, _ := m[p].([]any)
				for _, part := range strings.Split(val, ",") {
					if part = strings.TrimSpace(part); part != "" {
						existing = append(existing, part)
					}
				}
				m[p] = existing
				return
			}
			if n, err := strconv.ParseInt(val, 10, 64); err == nil {
				m[p] = n
			} else if b, err := strconv.ParseBool(val); err == nil && (val == "true" || val == "false") {
				m[p] = b
			} else {
				m[p] = val
			}
			return
		}
		next, ok := m[p].(map[string]any)
		if !ok {
			next = map[string]any{}
			m[p] = next
		}
		m = next
	}
}

var insecureDefaults = map[string]bool{
	"defaultencryptionkey":        true,
	"defaultrefreshencryptionkey": true,
	"defaultkey":                  true,
	"defaulthttpkey":              true,
}

func (c *Config) validate() error {
	if c.Name == "" {
		h, _ := os.Hostname()
		if h == "" {
			h = "node1"
		}
		c.Name = h
	}
	if strings.ContainsAny(c.Name, ". ") {
		return errors.New("name must not contain '.' or spaces (it is embedded in match IDs)")
	}
	if c.Session.TokenExpirySec <= 0 || c.Session.RefreshTokenExpirySec <= 0 {
		return errors.New("session token expiries must be positive")
	}
	if c.Socket.Port <= 0 {
		return errors.New("socket.port must be set")
	}
	if c.Runtime.MinCount < 1 {
		c.Runtime.MinCount = 1
	}
	if c.Runtime.MaxCount < c.Runtime.MinCount {
		c.Runtime.MaxCount = c.Runtime.MinCount
	}
	if c.Cluster.HeartbeatMs <= 0 {
		c.Cluster.HeartbeatMs = 1000
	}
	if c.Cluster.NodeTimeoutMs < 3*c.Cluster.HeartbeatMs {
		c.Cluster.NodeTimeoutMs = 3 * c.Cluster.HeartbeatMs
	}
	roles := map[string]bool{}
	for _, r := range c.Cluster.Roles {
		r = strings.TrimSpace(r)
		if r != "gateway" && r != "game" {
			return fmt.Errorf("cluster.roles: unknown role %q (want gateway and/or game)", r)
		}
		roles[r] = true
	}
	if len(roles) == 0 {
		return errors.New("cluster.roles must include gateway and/or game")
	}
	env := c.RuntimeEnv()
	if strings.EqualFold(env["DEPLOYMENT_ENVIRONMENT"], "production") && env["TANGO_ALLOW_CHEATS"] == "true" {
		return errors.New("TANGO_ALLOW_CHEATS must not be enabled in production")
	}
	if c.Cluster.AdvertiseAddress == "" {
		h, _ := os.Hostname()
		if h == "" {
			h = "127.0.0.1"
		}
		c.Cluster.AdvertiseAddress = fmt.Sprintf("%s:%d", h, c.Cluster.BusPort)
	}
	return nil
}

// InsecureSettings lists configuration values that are publicly known
// defaults. Production refuses to start with any of them unless
// --allow_insecure_defaults is used.
func (c *Config) InsecureSettings() []string {
	var out []string
	if insecureDefaults[c.Session.EncryptionKey] || len(c.Session.EncryptionKey) < 16 {
		out = append(out, "session.encryption_key")
	}
	if insecureDefaults[c.Session.RefreshEncryptionKey] || len(c.Session.RefreshEncryptionKey) < 16 {
		out = append(out, "session.refresh_encryption_key")
	}
	if insecureDefaults[c.Socket.ServerKey] {
		out = append(out, "socket.server_key")
	}
	if insecureDefaults[c.Runtime.HTTPKey] || len(c.Runtime.HTTPKey) < 16 {
		out = append(out, "runtime.http_key")
	}
	if c.Cluster.Secret == "" || len(c.Cluster.Secret) < 16 {
		out = append(out, "cluster.secret")
	}
	return out
}

// RuntimeEnv converts runtime.env "KEY=VALUE" entries into a map.
func (c *Config) RuntimeEnv() map[string]string {
	env := make(map[string]string, len(c.Runtime.Env))
	for _, e := range c.Runtime.Env {
		k, v, _ := strings.Cut(e, "=")
		env[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return env
}
