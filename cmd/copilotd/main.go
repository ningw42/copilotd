// Command copilotd is the composition root for the copilotd walking skeleton.
// It parses configuration, builds the structured logger, binds the listener,
// and runs the HTTP server under a signal-aware context. It holds no business
// logic — every capability lives in the internal packages it wires together.
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/ningw42/copilotd/internal/build"
	"github.com/ningw42/copilotd/internal/config"
	"github.com/ningw42/copilotd/internal/logging"
	"github.com/ningw42/copilotd/internal/server"
)

func main() {
	os.Exit(run(os.Args[1:], os.LookupEnv, os.Stdout, os.Stderr))
}

// run wires the process together and returns an exit code. Args, env, and the
// output streams are injected so the version and validation paths can be tested
// without touching process globals.
//
// Exit codes: --version -> 0; config error -> 1; bind error -> 1 (distinct from
// a serve error); clean shutdown -> 0; serve error -> 1.
func run(args []string, lookupEnv func(string) (string, bool), stdout, stderr io.Writer) int {
	// --version short-circuits before Load, so it works even when the rest of
	// the configuration would fail to load.
	if config.VersionRequested(args) {
		fmt.Fprintln(stdout, build.String())
		return 0
	}

	cfg, err := config.Load(args, lookupEnv)
	if err != nil {
		fmt.Fprintln(stderr, "copilotd: "+err.Error())
		return 1
	}

	logger, closer, err := logging.New(cfg)
	if err != nil {
		fmt.Fprintln(stderr, "copilotd: "+err.Error())
		return 1
	}
	defer func() { _ = closer.Close() }()

	// Route stray global slog calls and dependency logs through our handler.
	slog.SetDefault(logger)

	logger.Info("starting copilotd",
		slog.String("build", build.String()),
		slog.Any("config", cfg),
	)

	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		// Distinct from a serve error: the process never began serving.
		logger.Error("bind failed", slog.String("addr", cfg.Addr), slog.Any("error", err))
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// After the first signal, restore default signal handling so a second
	// signal hard-kills the process if graceful shutdown wedges.
	go func() {
		<-ctx.Done()
		stop()
	}()

	if err := server.New(cfg, logger).Run(ctx, ln); err != nil {
		logger.Error("server error", slog.Any("error", err))
		return 1
	}
	return 0
}
