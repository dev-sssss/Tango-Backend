// Command tango-server runs one node of the Tango Cards game backend.
//
//	tango-server --config node.yml [--section.key value ...]
//	tango-server migrate up --database.address postgres://...
//
// Flags follow Nakama's "--section.key value" convention, so existing
// Nakama config files and command lines keep working. SIGTERM/SIGINT drain
// the node (matches move live to other game nodes) before it exits.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/dev-sssss/tango_backend/server/internal/server"
)

func main() {
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "version" {
		fmt.Println(server.BuildVersion)
		return
	}
	migrateOnly := false
	if len(args) >= 2 && args[0] == "migrate" {
		if args[1] != "up" {
			fmt.Fprintln(os.Stderr, "only 'migrate up' is supported")
			os.Exit(2)
		}
		migrateOnly, args = true, args[2:]
	}
	allowInsecure := false
	var rest []string
	for _, a := range args {
		if a == "--allow_insecure_defaults" {
			allowInsecure = true
			continue
		}
		rest = append(rest, a)
	}
	cfg, err := server.LoadConfig(rest)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		os.Exit(2)
	}
	logger := newLogger(cfg)
	ctx := context.Background()
	if migrateOnly {
		db, err := server.OpenDB(ctx, cfg)
		if err != nil {
			logger.Error("database", "error", err)
			os.Exit(1)
		}
		if err := server.Migrate(ctx, db); err != nil {
			logger.Error("migrate", "error", err)
			os.Exit(1)
		}
		logger.Info("migrations applied")
		return
	}
	if bad := cfg.InsecureSettings(); len(bad) > 0 {
		if !allowInsecure {
			logger.Error("refusing to start with publicly known default secrets; set them or pass --allow_insecure_defaults for local development", "settings", bad)
			os.Exit(2)
		}
		logger.Warn("running with insecure default secrets", "settings", bad)
	}
	srv := server.New(cfg, logger)
	startCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	err = srv.Start(startCtx)
	cancel()
	if err != nil {
		logger.Error("startup failed", "error", err)
		os.Exit(1)
	}
	sig := make(chan os.Signal, 2)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	s := <-sig
	logger.Info("shutting down: draining node", "signal", s.String())
	go func() {
		<-sig
		logger.Warn("second signal: exiting without finishing the drain")
		os.Exit(1)
	}()
	srv.Shutdown(ctx)
}

func newLogger(cfg *server.Config) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(cfg.Logger.Level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lvl}
	var h slog.Handler
	if strings.ToLower(cfg.Logger.Format) == "text" {
		h = slog.NewTextHandler(os.Stdout, opts)
	} else {
		h = slog.NewJSONHandler(os.Stdout, opts)
	}
	return slog.New(h).With("node", cfg.Name)
}
