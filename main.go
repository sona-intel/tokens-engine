package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"tokens/internal/app"
	"tokens/internal/config"
	"tokens/internal/rpc"
	"tokens/internal/server"
	"tokens/internal/store"
)

const (
	healthURL     = "http://127.0.0.1:8080/healthz"
	healthTimeout = 2 * time.Second
)

var errUsage = errors.New("usage: app [serve|healthcheck|migrate]")

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	slog.SetDefault(logger())
	if err := run(ctx, os.Args[1:]); err != nil {
		slog.Error("tokens-engine stopped", "error", err)
		stop()
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	cmd := "serve"
	if len(args) > 0 {
		cmd = args[0]
	}
	switch cmd {
	case "serve":
		return serve(ctx)
	case "healthcheck":
		return healthcheck(ctx, healthURL)
	case "migrate":
		return migrate(ctx)
	}
	return fmt.Errorf("%w, got %q", errUsage, cmd)
}

// postgresDSN and postgresSchema stay empty in production, so the libpq PG variables decide
var (
	postgresDSN    string
	postgresSchema string
	apiAddr        = server.Addr
	debugAddr      = server.DebugAddr
)

const migrateTimeout = 30 * time.Second

func openStore(ctx context.Context) (*store.Store, error) {
	st, err := store.Connect(ctx, postgresDSN, postgresSchema)
	if err != nil {
		return nil, err
	}
	mctx, cancel := context.WithTimeout(ctx, migrateTimeout)
	defer cancel()
	if err := st.Migrate(mctx); err != nil {
		st.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return st, nil
}

func serve(ctx context.Context) error {
	cfg, err := config.Load(config.Dir())
	if err != nil {
		return err
	}
	slog.Info("tokens-engine started", "chains", len(cfg.Chains), "hypersync_key_set", cfg.HyperSyncKey.Set(),
		"max_indexed_tokens", cfg.Index.MaxTokens)
	st, err := openStore(ctx)
	if err != nil {
		return err
	}
	defer st.Close()

	clients := map[string]*rpc.Client{}
	for _, c := range cfg.Chains {
		client, err := newChain(c, cfg)
		if err != nil {
			return fmt.Errorf("rpc client %s: %w", c.ID, err)
		}
		clients[c.ID] = client
	}
	metrics := server.NewMetrics()
	a, err := app.New(app.Deps{Config: cfg, Store: st, Clients: clients, Logger: slog.Default()}, metrics.Registry)
	if err != nil {
		return err
	}

	runCtx, stopRunner := context.WithCancel(ctx)
	defer stopRunner()
	var loop sync.WaitGroup
	loop.Go(func() { a.Run(runCtx) })
	err = server.Run(ctx, server.NewServer(apiAddr, server.New(a, metrics)), server.Debug(debugAddr, metrics.Handler()))
	stopRunner()
	loop.Wait()
	return err
}

func newChain(c config.Chain, cfg config.Config) (*rpc.Client, error) {
	return rpc.New(c.RPCURL, rpc.WithUpstreamFilter(cfg.RPCUpstreamFilter), rpc.WithMaxCalls(cfg.MulticallMaxCalls),
		rpc.WithPacer(rpc.NewPacer(cfg.RPCBackgroundPerSecond)), rpc.WithOnCredits(func(method, header string, paid bool) {
			if paid {
				slog.Warn("paid rpc call", "chain", c.ID, "method", method, "credits", header)
			}
		}))
}

func migrate(ctx context.Context) error {
	if _, err := config.Load(config.Dir()); err != nil {
		return err
	}
	st, err := openStore(ctx)
	if err != nil {
		return err
	}
	st.Close()
	slog.Info("migrations applied")
	return nil
}

func healthcheck(ctx context.Context, url string) error {
	ctx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := (&http.Client{Transport: &http.Transport{DisableKeepAlives: true}}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz answered %s", resp.Status)
	}
	return nil
}

func logger() *slog.Logger {
	level := slog.LevelInfo
	_ = level.UnmarshalText([]byte(os.Getenv("LOG_LEVEL")))
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
}
