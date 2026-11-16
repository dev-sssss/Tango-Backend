package e2e

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// Run with a scratch database (it is wiped):
//
//	TANGO_TEST_DB=postgres://tango:tango@localhost:5432/tango_e2e go test ./e2e -v -timeout 15m

const (
	serverKey = "e2e-server-key"
	httpKey   = "e2e-http-key-0123456789"
	secret    = "e2e-cluster-secret-0123456789"
)

type node struct {
	name  string
	roles string
	base  int
	cmd   *exec.Cmd
	log   string
	exit  chan error
	extra []string
}

type cluster struct {
	t     *testing.T
	bin   string
	db    string
	dir   string
	nodes map[string]*node
}

func dbOrSkip(t *testing.T) string {
	dsn := os.Getenv("TANGO_TEST_DB")
	if dsn == "" {
		t.Skip("set TANGO_TEST_DB to run")
	}
	return dsn
}

func newCluster(t *testing.T) *cluster {
	dsn := dbOrSkip(t)
	resetDB(t, dsn)
	dir := t.TempDir()
	c := &cluster{t: t, db: dsn, dir: dir, nodes: map[string]*node{}}
	c.bin = buildBinary(t, dir, strconv.FormatInt(time.Now().UnixNano(), 10))
	return c
}

func (c *cluster) start(n *node) {
	c.t.Helper()
	args := []string{
		"--name", n.name, "--cluster.roles", n.roles,
		"--database.address", c.db,
		"--socket.address", "127.0.0.1", "--socket.port", strconv.Itoa(n.base),
		"--socket.server_key", serverKey,
		"--session.encryption_key", "e2e-session-encryption-key-0123", "--session.refresh_encryption_key", "e2e-refresh-encryption-key-0123",
		"--session.token_expiry_sec", "7200",
		"--runtime.http_key", httpKey,
		"--cluster.secret", secret, "--cluster.bus_port", strconv.Itoa(n.base + 2), "--cluster.admin_port", strconv.Itoa(n.base + 1),
		"--cluster.advertise_address", "127.0.0.1:" + strconv.Itoa(n.base+2),
		"--cluster.heartbeat_ms", "500", "--cluster.node_timeout_ms", "3000", "--cluster.checkpoint_interval_ms", "0",
		"--cluster.drain_timeout_ms", "20000", "--cluster.presence_grace_ms", "10000",
		"--metrics.prometheus_port", "0", "--logger.format", "text", "--logger.level", "info",
	}
	args = append(args, n.extra...)
	f, err := os.OpenFile(n.log, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		c.t.Fatal(err)
	}
	fmt.Fprintf(f, "\n===== start %s %s =====\n", n.name, time.Now().Format(time.RFC3339Nano))
	cmd := exec.Command(c.bin, args...)
	cmd.Stdout, cmd.Stderr = f, f
	if err := cmd.Start(); err != nil {
		c.t.Fatal(err)
	}
	n.cmd = cmd
	n.exit = make(chan error, 1)
	go func() { n.exit <- cmd.Wait(); f.Close() }()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/admin/health", n.base+1))
		if err == nil {
			resp.Body.Close()
			c.nodes[n.name] = n
			return
		}
		select {
		case err := <-n.exit:
			c.t.Fatalf("node %s exited during startup: %v\n%s", n.name, err, tail(n.log))
		case <-time.After(100 * time.Millisecond):
		}
	}
	c.t.Fatalf("node %s did not start\n%s", n.name, tail(n.log))
}

func (c *cluster) stop(n *node, sig syscall.Signal, wait time.Duration) time.Duration {
	start := time.Now()
	_ = n.cmd.Process.Signal(sig)
	select {
	case <-n.exit:
	case <-time.After(wait):
		_ = n.cmd.Process.Kill()
		c.t.Fatalf("node %s did not exit within %v\n%s", n.name, wait, tail(n.log))
	}
	return time.Since(start)
}

func (c *cluster) stopAll() {
	for _, n := range c.nodes {
		if n.cmd != nil && n.cmd.ProcessState == nil {
			_ = n.cmd.Process.Signal(syscall.SIGTERM)
			select {
			case <-n.exit:
			case <-time.After(25 * time.Second):
				_ = n.cmd.Process.Kill()
			}
		}
	}
}

func tail(path string) string {
	b, _ := os.ReadFile(path)
	lines := strings.Split(string(b), "\n")
	if len(lines) > 40 {
		lines = lines[len(lines)-40:]
	}
	return strings.Join(lines, "\n")
}

func (c *cluster) owner(matchID string) (string, int64) {
	u, _, _ := strings.Cut(matchID, ".")
	conn, err := pgx.Connect(context.Background(), c.db)
	if err != nil {
		c.t.Fatal(err)
	}
	defer conn.Close(context.Background())
	var node string
	var epoch int64
	if err := conn.QueryRow(context.Background(), "SELECT node, epoch FROM cluster_match WHERE id = $1", u).Scan(&node, &epoch); err != nil {
		return "", 0
	}
	return node, epoch
}

func resetDB(t *testing.T, dsn string) {
	conn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Skipf("database unavailable: %v", err)
	}
	defer conn.Close(context.Background())
	if _, err := conn.Exec(context.Background(), "DROP SCHEMA public CASCADE; CREATE SCHEMA public"); err != nil {
		t.Fatal(err)
	}
}

func buildBinary(t *testing.T, dir, build string) string {
	bin := filepath.Join(dir, "tango-server-"+build)
	cmd := exec.Command("go", "build", "-o", bin,
		"-ldflags", "-X github.com/dev-sssss/tango_backend/server/internal/server.BuildVersion=e2e-"+build,
		"../cmd/tango-server")
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return bin
}

func gatewayURL(n *node) string { return "http://127.0.0.1:" + strconv.Itoa(n.base) }

// waitGameOver waits until every bot has received game_over.
func waitGameOver(t *testing.T, bots []*Bot, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		done := true
		for _, b := range bots {
			if b.Result() == nil {
				done = false
			}
		}
		if done {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	for _, b := range bots {
		t.Logf("%s last state: status=%v rev=%v log=%v", b.Name, b.Last()["status"], b.Last()["rev"], b.Log())
	}
	t.Fatal("game did not finish in time")
}

// waitProgress waits until every bot's state revision moves forward by n.
func waitProgress(t *testing.T, what string, bots []*Bot, n int64, timeout time.Duration) {
	t.Helper()
	target := make([]int64, len(bots))
	for i, b := range bots {
		target[i] = b.MaxRevision.Load() + n
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ok := true
		for i, b := range bots {
			if b.MaxRevision.Load() < target[i] && b.Result() == nil {
				ok = false
			}
		}
		if ok {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("%s: game made no progress", what)
}
