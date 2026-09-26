// Command copilotd is the composition root for the copilotd proxy. It assembles
// a git-style subcommand tree — serve, login, usage, help, version — wiring the internal
// packages together; it holds no business logic. `serve` runs the HTTP daemon
// (config load → logger → bind → signal-aware graceful shutdown); the other verbs
// provide discovery (help), build info (version), GitHub OAuth device login,
// and HTTP-only Usage report presentation.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // Named report zones work without host tzdata, including CGO-disabled builds.

	"github.com/ningw42/copilotd/internal/build"
	"github.com/ningw42/copilotd/internal/cache"
	"github.com/ningw42/copilotd/internal/catalog"
	"github.com/ningw42/copilotd/internal/config"
	"github.com/ningw42/copilotd/internal/forward"
	"github.com/ningw42/copilotd/internal/identity"
	"github.com/ningw42/copilotd/internal/impersonation"
	"github.com/ningw42/copilotd/internal/logging"
	"github.com/ningw42/copilotd/internal/server"
	"github.com/ningw42/copilotd/internal/shim"
	"github.com/ningw42/copilotd/internal/upstream"
	"github.com/ningw42/copilotd/internal/usage"
	"github.com/ningw42/copilotd/internal/usage/pricing"
	"github.com/ningw42/copilotd/internal/usage/report"
	"github.com/ningw42/copilotd/internal/usage/reportcli"
	"github.com/ningw42/copilotd/internal/usage/reporthttp"
	"github.com/ningw42/copilotd/internal/usage/sqlitestore"
	"github.com/ningw42/copilotd/internal/wsforward"
	"github.com/peterbourgon/ff/v4"
	"github.com/peterbourgon/ff/v4/ffhelp"
)

func main() {
	os.Exit(run(os.Args[1:], os.LookupEnv, os.Stdout, os.Stderr))
}

// errServeFailed marks a serve failure that was already reported through the
// structured logger (bind or serve error), so the top-level translator carries
// the non-zero exit code without printing the error a second time.
var errServeFailed = errors.New("serve failed")

const (
	productionVSCodeDiscoveryBaseURL      = "https://update.code.visualstudio.com"
	productionMarketplaceDiscoveryBaseURL = "https://marketplace.visualstudio.com"
	productionCodexModelsBaseURL          = "https://api.github.com"
)

// run builds the command tree, dispatches, and translates the outcome into an
// exit code. Args, env, and the output streams are injected so dispatch and the
// version/validation paths can be tested without touching process globals.
//
// Exit codes: version -> 0; bare/help -> 0; clean serve shutdown, including a
// signal received before bind -> 0; signal-driven drain forced only by
// --shutdown-timeout expiring (logged at Warn) -> 0; config error -> 1;
// startup, bind, or genuine serve/shutdown error -> 1; unknown subcommand -> 1.
func run(args []string, lookupEnv func(string) (string, bool), stdout, stderr io.Writer) int {
	root := buildCommand(lookupEnv, stdout, stderr)
	err := root.Parse(args)
	selected := root.GetSelected()
	if errors.Is(err, ff.ErrHelp) {
		if helpSelected, helpErr := validateHelpRequest(args); helpErr != nil {
			selected, err = helpSelected, helpErr
		}
	}
	if selected != nil && selected.Name == "usage" && !errors.Is(err, ff.ErrHelp) {
		// Keep stdout/stderr EPIPE nonfatal through error translation, including
		// parse failures. ff retains the selected command even when Parse fails.
		// Valid help and other commands retain their existing signal policy.
		stop := notifyUsagePipeErrors()
		defer stop()
	}
	if err == nil {
		err = root.Run(context.Background())
	}
	switch {
	case err == nil:
		return 0
	case errors.Is(err, errServeFailed):
		// Already reported via the structured logger; just carry the exit code.
		return 1
	case errors.Is(err, ff.ErrHelp):
		// -h/--help on any command: render its help to stdout and exit clean.
		fmt.Fprintln(stdout, ffhelp.Command(root))
		return 0
	default:
		writeCLIError(root, stderr, err)
		return 1
	}
}

func writeCLIError(root *ff.Command, stderr io.Writer, err error) {
	message := err.Error()
	if strings.HasPrefix(message, root.Name+":") {
		fmt.Fprintln(stderr, message)
	} else {
		fmt.Fprintln(stderr, root.Name+": "+message)
	}
}

// validateHelpRequest re-parses syntax on a fresh command tree after removing
// parser-native help flags. ff stops parsing at -h/--help, so this second pass is
// necessary to reject trailing unknown flags and operands without resolving
// configuration or executing a command. Return its selected command as well:
// help can precede the subcommand, so the original parse may have stopped at root.
func validateHelpRequest(args []string) (*ff.Command, error) {
	syntaxArgs := append([]string(nil), args...)
	for {
		root := buildCommand(func(string) (string, bool) { return "", false }, io.Discard, io.Discard)
		err := root.Parse(syntaxArgs)
		selected := root.GetSelected()
		if errors.Is(err, ff.ErrHelp) {
			if selected == nil || selected.Flags == nil {
				return selected, err
			}
			remaining := selected.Flags.GetArgs()
			helpIndex := len(syntaxArgs) - len(remaining)
			if len(remaining) == 0 || helpIndex < 0 || helpIndex >= len(syntaxArgs) {
				return selected, err
			}
			syntaxArgs = append(syntaxArgs[:helpIndex:helpIndex], syntaxArgs[helpIndex+1:]...)
			continue
		}
		if err != nil {
			return selected, err
		}

		if selected == nil || selected.Flags == nil {
			return selected, nil
		}
		operands := selected.Flags.GetArgs()
		if selected == root && len(operands) > 0 {
			return selected, fmt.Errorf("unknown subcommand %q (run 'copilotd help')", operands[0])
		}
		allowed := 0
		if selected.Name == "help" {
			allowed = 1
		}
		return selected, rejectSurplusOperands(selected.Name, operands, allowed)
	}
}

// buildCommand assembles the subcommand tree. Root and informational commands
// have no operational flags; serve, login, and usage own independent flag sets.
func buildCommand(lookupEnv func(string) (string, bool), stdout, stderr io.Writer) *ff.Command {
	rootFlags := ff.NewFlagSet("copilotd")
	serveFlags := ff.NewFlagSet("serve")
	serveCfg := config.RegisterServe(serveFlags)
	loginFlags := ff.NewFlagSet("login")
	loginCfg := config.RegisterLogin(loginFlags)
	usageFlags := ff.NewFlagSet("usage")
	usageCfg := config.RegisterUsage(usageFlags)

	// root is assigned below and captured by the help/root closures so they can
	// render the tree's help; ParseAndRun invokes those Execs after assignment.
	var root *ff.Command

	serveCmd := &ff.Command{
		Name:      "serve",
		Usage:     "copilotd serve [FLAGS]",
		ShortHelp: "run the proxy daemon",
		Flags:     serveFlags,
		Exec: func(ctx context.Context, args []string) error {
			if err := rejectSurplusOperands("serve", args, 0); err != nil {
				return err
			}
			return runServe(ctx, serveCfg, lookupEnv)
		},
	}

	// login runs the GitHub OAuth device flow and writes the GitHub OAuth token
	// file (#13).
	// Its command-local flags include the shared logging/config/write-target
	// settings followed by github-client-id and github-scope.
	loginCmd := &ff.Command{
		Name:      "login",
		Usage:     "copilotd login [FLAGS]",
		ShortHelp: "obtain a GitHub OAuth token via device flow",
		Flags:     loginFlags,
		Exec: func(ctx context.Context, args []string) error {
			if err := rejectSurplusOperands("login", args, 0); err != nil {
				return err
			}
			return runLogin(ctx, loginCfg, lookupEnv, stdout)
		},
	}

	usageCmd := &ff.Command{
		Name: "usage", Usage: "copilotd usage [FLAGS]",
		ShortHelp: "report persisted Anthropic and OpenAI Turns", Flags: usageFlags,
		Exec: func(ctx context.Context, args []string) error {
			if err := rejectSurplusOperands("usage", args, 0); err != nil {
				return err
			}
			cfg, err := usageCfg.Resolve(lookupEnv)
			if err != nil {
				return err
			}
			client, err := reporthttp.NewClient(cfg.Endpoint)
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
			defer stop()
			return reportcli.Run(ctx, client, reportcli.Options{
				Endpoint: cfg.Endpoint, Timezone: cfg.Timezone, Details: cfg.Details, JSON: cfg.JSON, Timeout: cfg.Timeout,
				Query: report.Query{Period: cfg.Period, Since: cfg.Since, Until: cfg.Until, Surface: cfg.Surface, Model: cfg.Model},
			}, stdout)
		},
	}

	versionCmd := &ff.Command{
		Name:      "version",
		Usage:     "copilotd version",
		ShortHelp: "print build version and exit",
		Flags:     ff.NewFlagSet("version"),
		Exec: func(_ context.Context, args []string) error {
			if err := rejectSurplusOperands("version", args, 0); err != nil {
				return err
			}
			fmt.Fprintln(stdout, build.String())
			return nil
		},
	}

	helpCmd := &ff.Command{
		Name:      "help",
		Usage:     "copilotd help [SUBCOMMAND]",
		ShortHelp: "show help for copilotd or a subcommand",
		Flags:     ff.NewFlagSet("help"),
		Exec: func(_ context.Context, args []string) error {
			return runHelp(root, args, stdout)
		},
	}

	root = &ff.Command{
		Name:      "copilotd",
		Usage:     "copilotd <SUBCOMMAND>",
		ShortHelp: "an Anthropic/OpenAI proxy over a GitHub Copilot subscription",
		Flags:     rootFlags,
		Exec: func(_ context.Context, args []string) error {
			// With subcommands defined, an unknown verb falls through to here with
			// args=[verb]; no args is the bare `copilotd`, which prints help.
			if len(args) > 0 {
				return fmt.Errorf("unknown subcommand %q (run 'copilotd help')", args[0])
			}
			fmt.Fprintln(stdout, generalHelp(root))
			return nil
		},
	}
	root.Subcommands = []*ff.Command{versionCmd, helpCmd, serveCmd, loginCmd, usageCmd}
	return root
}

// generalHelp renders the root's own help — usage and the subcommand list —
// independent of which command the parse phase selected. It is
// the counterpart to ffhelp.Command, which follows GetSelected and would instead
// render the terminal verb (e.g. `help` itself) when called from a verb's Exec.
func generalHelp(root *ff.Command) ffhelp.Help {
	title := root.Name
	if root.ShortHelp != "" {
		title = fmt.Sprintf("%s -- %s", root.Name, root.ShortHelp)
	}
	help := ffhelp.Help{ffhelp.NewSection("COMMAND", title)}
	if root.Usage != "" {
		help = append(help, ffhelp.NewSection("USAGE", root.Usage))
	}
	if len(root.Subcommands) > 0 {
		help = append(help, ffhelp.NewSubcommandsSection(root.Subcommands))
	}
	help = append(help, ffhelp.NewFlagsSections(root.Flags)...)
	return help
}

// runHelp implements the `help [SUBCOMMAND]` verb: no argument prints the root
// help; a name renders that subcommand's help, or errors if it is unknown.
func runHelp(root *ff.Command, args []string, stdout io.Writer) error {
	if err := rejectSurplusOperands("help", args, 1); err != nil {
		return err
	}
	if len(args) == 0 {
		fmt.Fprintln(stdout, generalHelp(root))
		return nil
	}
	name := args[0]
	for _, sub := range root.Subcommands {
		if strings.EqualFold(sub.Name, name) {
			fmt.Fprintln(stdout, ffhelp.Command(sub))
			return nil
		}
	}
	return fmt.Errorf("unknown subcommand %q (run 'copilotd help')", name)
}

func rejectSurplusOperands(command string, args []string, allowed int) error {
	if len(args) > allowed {
		return fmt.Errorf("%s: unexpected operand %q", command, args[allowed])
	}
	return nil
}

// runServe is the serve command: resolve config, build the logger and set it as
// the slog default, install signal handling, then hand the production edges to
// runServeLifecycle and map its outcome to the exit code. The signal-aware
// context covers the whole lifecycle, so a first signal before bind is a
// graceful stop too, and its re-armed handler lets a second signal hard-kill a
// wedged startup or shutdown. Errors after the logger is up were already
// reported through it and return errServeFailed so the caller does not
// double-report them; a pre-logger config error is returned raw for the
// top-level translator to print.
func runServe(ctx context.Context, flags *config.ServeFlags, lookupEnv func(string) (string, bool)) error {
	cfg, err := flags.Resolve(lookupEnv)
	if err != nil {
		return err
	}

	base, closer, err := logging.New(cfg)
	if err != nil {
		return err
	}
	// The lifecycle finalizes the Usage store before returning, so logging stays
	// alive through the final flush, cleanup status, and aggregate publication.
	defer func() { _ = closer.Close() }()

	// Route stray global slog calls and dependency logs through the component-free
	// base so unadapted dependency records are not falsely attributed.
	slog.SetDefault(base)
	logger := logging.ForComponent(base, "cmd/copilotd")

	logger.Info("starting copilotd",
		slog.String(logging.BuildKey, build.String()),
		slog.Any(logging.ConfigKey, cfg),
	)

	serveCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	// After the first signal, restore default signal handling so a second signal
	// hard-kills the process if graceful startup or shutdown wedges.
	go func() {
		<-serveCtx.Done()
		stop()
	}()

	return serveExitError(runServeLifecycle(serveCtx, base, serveInput{Config: cfg, Edges: productionServeEdges()}))
}

// serveEdges holds the network edges the serve lifecycle reaches besides
// Copilot itself, whose base URL each exchange supplies: the GitHub token
// exchange (an empty base URL means api.github.com), impersonation discovery,
// the Codex models release source, and the public pricing source. Production
// uses productionServeEdges; tests point each edge at a local stub.
type serveEdges struct {
	GitHubBaseURL string
	GitHubClient  *http.Client
	Discovery     impersonation.Edge
	CodexModels   catalog.ModelsEdge
	Pricing       pricing.Remote
}

func productionServeEdges() serveEdges {
	return serveEdges{
		GitHubClient: newExchangeClient(),
		Discovery:    productionDiscoveryEdge(),
		CodexModels:  productionCodexModelsEdge(),
		Pricing:      pricing.NewRemote(pricing.ModelsDevURL, nil),
	}
}

// serveInput is what runServeLifecycle serves besides its base logger.
// Listener is optional: when nil the lifecycle binds Config.Addr itself. A
// supplied listener belongs to the lifecycle from the call, which closes it on
// every return that does not serve it.
type serveInput struct {
	Config   config.ServeConfig
	Edges    serveEdges
	Listener net.Listener
}

// serveOutcome classifies how one serve lifecycle ended. The kinds tell tests
// and logs apart; runServe collapses them to exit 0 (clean or forced drain)
// or 1 (every failure).
type serveOutcome int

const (
	servePreBindFailure serveOutcome = iota + 1
	serveBindFailure
	serveServeFailure
	serveForcedDrain
	serveClean
)

func (o serveOutcome) String() string {
	switch o {
	case servePreBindFailure:
		return "pre-bind failure"
	case serveBindFailure:
		return "bind failure"
	case serveServeFailure:
		return "serve failure"
	case serveForcedDrain:
		return "forced drain"
	case serveClean:
		return "clean"
	default:
		return fmt.Sprintf("serveOutcome(%d)", int(o))
	}
}

// serveResult is runServeLifecycle's result. Err is the failure behind a
// failure outcome, or Server.Run's forced-drain error. Report is the Usage
// store's finalization report: zero when the meter is disabled, though a zero
// Report alone does not prove the store never opened. It never changes Outcome.
type serveResult struct {
	Outcome serveOutcome
	Err     error
	Report  sqlitestore.Report
}

// servedOutcome classifies Server.Run's raw result. A clean drain and a
// timeout-only forced drain (recognized solely by server.ErrForcedDrain, never
// a bare context.DeadlineExceeded) are distinct outcomes; every other error is
// a serve failure.
func servedOutcome(err error) serveOutcome {
	switch {
	case err == nil:
		return serveClean
	case errors.Is(err, server.ErrForcedDrain):
		return serveForcedDrain
	default:
		return serveServeFailure
	}
}

// serveExitError maps a lifecycle result to runServe's error. A clean stop and
// a forced drain are operator policy success whatever the Report says; every
// failure was already logged once and becomes errServeFailed (exit 1).
func serveExitError(result serveResult) error {
	if result.Outcome == serveClean || result.Outcome == serveForcedDrain {
		return nil
	}
	return errServeFailed
}

// runServeLifecycle is the single owner of production serve assembly and
// resource lifetime. In order, it resolves the GitHub OAuth token and builds the
// minting Manager, registers the Codex models and pricing cached values when
// their features are enabled, opens the Usage store when the meter is enabled,
// builds the Shim registry, binds Config.Addr unless a listener was supplied,
// and serves through runBoundServe until ctx is cancelled or serving fails.
//
// A missing local prerequisite fails before bind with one command-level
// diagnostic. Cancellation is checked between setup steps: already cancelled
// on entry, nothing is set up; noticed later but before serving, the lifecycle
// stops cleanly without serving. A step that already failed keeps its failure
// outcome. Once serving starts, cancellation takes Server.Run's drain path.
// Every return that opened the Usage store finalizes it with a fresh
// ShutdownTimeout after serving has stopped, and every return that did not
// serve closes the listener, supplied or acquired.
func runServeLifecycle(ctx context.Context, base *slog.Logger, input serveInput) serveResult {
	cfg := input.Config
	edges := input.Edges
	ln := input.Listener
	var usageStore *sqlitestore.Store
	endUnserved := func(outcome serveOutcome, err error) serveResult {
		if ln != nil {
			_ = ln.Close()
		}
		return finalizeServe(serveResult{Outcome: outcome, Err: err}, usageStore, cfg.ShutdownTimeout)
	}
	if ctx.Err() != nil {
		return endUnserved(serveClean, nil)
	}
	logger := logging.ForComponent(base, "cmd/copilotd")
	logCodexCatalogStaging(logger, cfg)

	// Credential-presence check + real credential Provider, assembled BEFORE the
	// listener binds so a missing OAuth token fails fast (non-zero exit) without
	// ever serving.
	cacheRegistry := cache.NewRegistry()
	mgr, imp, err := buildServeProvider(cfg, base, edges.GitHubBaseURL, edges.GitHubClient, edges.Discovery, cacheRegistry)
	if err != nil {
		// Already carries the "run copilotd login" guidance when no source yields a
		// token; reported through the logger, then a silent non-zero exit.
		logger.Error("cannot start: resolving the GitHub OAuth token failed", slog.Any(logging.ErrorKey, err))
		return endUnserved(servePreBindFailure, err)
	}
	codexModels := configuredCodexModels(cfg, edges.CodexModels, cacheRegistry, base)
	usagePricing := configuredUsagePricing(cfg, edges.Pricing, cacheRegistry, base)
	if ctx.Err() != nil {
		return endUnserved(serveClean, nil)
	}

	var sink usage.Sink
	if cfg.ShimUsageMeterEnabled {
		var openErr error
		// Resolve once; writer and reporter receive this same daemon-owned path.
		cfg.UsageDBPath, openErr = filepath.Abs(cfg.UsageDBPath)
		if openErr == nil {
			usageStore, openErr = sqlitestore.Open(cfg.UsageDBPath, logging.ForComponent(base, "internal/usage/sqlitestore"))
		}
		if openErr != nil {
			logger.Error("cannot start: opening usage database failed",
				slog.String(logging.PathKey, cfg.UsageDBPath),
				slog.Any(logging.ErrorKey, openErr))
			return endUnserved(servePreBindFailure, openErr)
		}
		sink = usageStore
		if ctx.Err() != nil {
			return endUnserved(serveClean, nil)
		}
	}
	registry := configuredShimRegistry(cfg, sink)
	logShimChain(logger, registry)
	if ctx.Err() != nil {
		return endUnserved(serveClean, nil)
	}

	if ln == nil {
		ln, err = net.Listen("tcp", cfg.Addr)
		if err != nil {
			// Distinct from a serve error: the process never began serving.
			logger.Error("bind failed", slog.String(logging.AddrKey, cfg.Addr), slog.Any(logging.ErrorKey, err))
			return endUnserved(serveBindFailure, err)
		}
	}
	if ctx.Err() != nil {
		return endUnserved(serveClean, nil)
	}

	serveErr := runBoundServe(ctx, cfg, base, mgr, imp, codexModels, usagePricing, cacheRegistry, registry, ln, usageStore)
	return finalizeServe(serveResult{Outcome: servedOutcome(serveErr), Err: serveErr}, usageStore, cfg.ShutdownTimeout)
}

// finalizeServe attaches the Usage store's finalization report to result when
// the lifecycle opened a store.
func finalizeServe(result serveResult, usageStore *sqlitestore.Store, timeout time.Duration) serveResult {
	if usageStore != nil {
		result.Report = finalizeUsageStore(usageStore, timeout)
	}
	return result
}

// runBoundServe starts the background impersonation/mint lifecycle only after
// its caller has supplied an already-bound listener and the configured Shim
// registry. That ordering keeps
// /healthz and the locally-ready /readyz available while bounded startup
// discovery is in progress. Neither discovery nor startup mint outcomes gate
// readiness or request admission. Startup runs under a child of ctx. When
// Server.Run returns, usage admission (if usageStore is non-nil) is cut off
// first, then the startup context is cancelled, and only then is the outcome
// synchronously logged. A forced drain (server.ErrForcedDrain) is logged once
// at Warn with the configured timeout; any other error at Error. Either way the
// raw Server.Run result is returned unchanged, so callers and tests can still
// tell a forced drain from a clean one.
func runBoundServe(ctx context.Context, cfg config.ServeConfig, base *slog.Logger, mgr *identity.Manager, imp *impersonation.Set, codexModels *cache.Value[[]byte], usagePricing pricing.Source, cacheRegistry *cache.Registry, registry shim.Registry, ln net.Listener, usageStore *sqlitestore.Store) error {
	startupCtx, cancelStartup := context.WithCancel(ctx)
	defer cancelStartup()
	go runServeStartup(startupCtx, cacheRegistry, mgr, logging.ForComponent(base, "cmd/copilotd"))
	catalogs := catalog.RenderDescriptors{
		Anthropic: catalog.AnthropicRenderConfig{
			ModelIDNormalizationEnabled: cfg.AnthropicCatalogModelIDNormalizationEnabled,
		},
		Codex: catalog.CodexDescriptor{
			Enabled: cfg.CodexCatalogEnabled,
			Models:  codexModels,
			RenderConfig: catalog.CodexRenderConfig{
				ModelAliases:             cfg.CodexCatalogModelAliases,
				AutoReviewModel:          cfg.CodexAutoReviewModel,
				AutoReviewModelOverrides: cfg.CodexAutoReviewModelOverrides,
				OverrideLimits:           cfg.CodexOverrideLimits,
			},
		},
	}

	forwardClient := forward.NewClient(cfg.ResponseHeaderTimeout)
	caller := upstream.New(mgr, forwardClient, cfg.OutboundTimeout, cfg.MaxBufferedResponseBytes, logging.ForComponent(base, "internal/upstream"))
	fwd := forward.New(caller, cfg.OutboundTimeout, cfg.WriteTimeout, cfg.StreamIdleTimeout, cfg.StreamKeepaliveInterval, cfg.MaxRequestBytes, registry,
		logging.ForComponent(base, "internal/sse"), logging.ForComponent(base, "internal/shim"), cfg.ShimHookOverrunThreshold)
	wsDialClient := &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment}}
	wsAccepts := server.NewWsAcceptCounter()
	wsTerminals := server.NewWsSessionTerminalCounter()
	wsProxy := wsforward.New(caller, wsDialClient, cfg.WebSocketHandshakeTimeout, cfg.WriteTimeout, cfg.MaxRequestBytes, registry,
		logging.ForComponent(base, "internal/wsforward"), logging.ForComponent(base, "internal/shim"), cfg.ShimHookOverrunThreshold, wsforward.WsMetrics{
			Accept:          wsAccepts,
			SessionTerminal: wsTerminals,
		})
	streamOutcomes := server.NewStreamOutcomeCounter()

	var reportQuery reporthttp.QueryFunc
	if usageStore != nil {
		reportQuery = report.New(cfg.UsageDBPath, usagePricing).Query
	}
	reportHandler := reporthttp.Handler(reportQuery)
	serveErr := server.New(cfg, logging.ForComponent(base, "internal/server"), logging.ForComponent(base, "internal/catalog"), logging.DependencyErrorLog(base, slog.LevelWarn), mgr, server.ReadyObservers{
		Impersonation: imp,
		Caches:        cacheRegistry,
	}, fwd, caller, wsProxy, streamOutcomes, catalogs, reportHandler).Run(ctx, ln)
	if usageStore != nil {
		usageStore.StopAdmission()
	}
	cancelStartup()
	logger := logging.ForComponent(base, "cmd/copilotd")
	switch {
	case errors.Is(serveErr, server.ErrForcedDrain):
		logger.Warn("forced drain",
			slog.Any(logging.ErrorKey, serveErr),
			slog.Duration(logging.TimeoutKey, cfg.ShutdownTimeout))
	case serveErr != nil:
		logger.Error("server error", slog.Any(logging.ErrorKey, serveErr))
	}
	return serveErr
}

// runServeStartup performs the ordered background startup sequence. The cache
// registry primes every refresh-enabled cached value first, launches their
// independent refresh loops after that bounded wait, and then the startup mint
// runs with the resulting live (or fallback) impersonation headers. Values with
// refresh disabled stay registered and make both lifecycle operations no-ops.
func runServeStartup(ctx context.Context, cacheRegistry *cache.Registry, mgr *identity.Manager, logger *slog.Logger) {
	cacheRegistry.Prime(ctx)
	logCachedValueStartupOutcomes(logger, cacheRegistry.Observe())
	cacheRegistry.Start(ctx)
	mgr.StartupMint(ctx)
}

func finalizeUsageStore(store *sqlitestore.Store, timeout time.Duration) sqlitestore.Report {
	store.StopAdmission()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return store.Close(ctx)
}

func logCachedValueStartupOutcomes(logger *slog.Logger, observed []cache.Status) {
	for _, status := range observed {
		logger.Info("startup cached value refresh outcome",
			slog.String(logging.CachedValueKey, status.Name),
			slog.String(logging.CachedValueSourceKey, status.Source),
			slog.String(logging.CachedValueVersionKey, status.Version))
	}
}

func logCodexCatalogStaging(logger *slog.Logger, cfg config.ServeConfig) {
	if cfg.CodexCatalogEnabled || cfg.CodexAutoReviewModel == "" {
		return
	}
	logger.Info("Codex reviewer is staged while the Codex catalog is disabled",
		slog.String(logging.ReviewerKey, cfg.CodexAutoReviewModel))
}

func configuredShimRegistry(cfg config.ServeConfig, sink usage.Sink) shim.Registry {
	registry := shim.CanonicalRegistry(sink)
	for i := range registry {
		switch registry[i].Name {
		case "nop":
			registry[i].Enabled = cfg.ShimNopEnabled
		case "responses-item-id-stabilizer":
			registry[i].Enabled = cfg.ShimResponsesItemIDStabilizerEnabled
		case "usage-meter":
			registry[i].Enabled = cfg.ShimUsageMeterEnabled && sink != nil
		}
	}
	return registry
}

func logShimChain(logger *slog.Logger, registry shim.Registry) {
	enabled := make([]string, 0, len(registry))
	for _, registration := range registry {
		if registration.Enabled {
			enabled = append(enabled, registration.Name)
		}
	}
	logger.Info("configured shim chain", slog.Any(logging.EnabledShimsKey, enabled))
}

// buildServeProvider assembles the real credential Provider and live
// impersonation Set for `serve`: it resolves the GitHub OAuth token (§6.5),
// seeds the Set with configured fallbacks and static identifiers, binds the
// injected discovery edge, and constructs the minting identity.Manager. It
// returns the resolve error unchanged (e.g. identity.ErrNoOAuthToken) so
// runServeLifecycle can fail fast before binding a listener.
//
// githubBaseURL/httpClient and discoveryEdge are the lifecycle's serveEdges:
// production uses GitHub plus the two public Microsoft origins with separate
// plain clients, while tests point them at stubs. Every other Manager
// timing/clock knob is left to NewManager's production defaults.
func buildServeProvider(cfg config.ServeConfig, base *slog.Logger, githubBaseURL string, httpClient *http.Client, discoveryEdge impersonation.Edge, cacheRegistry *cache.Registry) (*identity.Manager, *impersonation.Set, error) {
	oauthToken, err := identity.ResolveOAuthToken(cfg.GithubOAuthToken, cfg.GithubOAuthTokenFile)
	if err != nil {
		return nil, nil, err
	}
	imp := impersonation.New(impersonation.Config{
		VSCodeVersionFallback: cfg.VSCodeVersionFallback,
		PluginVersionFallback: cfg.PluginVersionFallback,
		CopilotIntegrationID:  cfg.CopilotIntegrationID,
		GithubAPIVersion:      cfg.GithubAPIVersion,
		RefreshInterval:       cfg.ImpersonationRefreshInterval,
	}, discoveryEdge, cacheRegistry, logging.ForComponent(base, "internal/cache"))
	mgr := identity.NewManager(logging.ForComponent(base, "internal/identity"), identity.ManagerConfig{
		OAuthToken:    oauthToken,
		GitHubBaseURL: githubBaseURL,
		HTTPClient:    httpClient,
		// Direct assignment is the composition-root proof that the live Set
		// satisfies identity.Impersonation without reversing package dependencies.
		Impersonation:      imp,
		StartupMintRetries: cfg.StartupMintRetries,
	})
	return mgr, imp, nil
}

// newExchangeClient returns the dedicated HTTP client for the GitHub token
// exchange, kept separate from the outbound inference client so their transports
// and timeouts never interfere. No client-level Timeout is set: the Manager
// bounds each exchange with its own background-scoped context deadline.
func newExchangeClient() *http.Client {
	return &http.Client{}
}

func productionDiscoveryEdge() impersonation.Edge {
	return impersonation.Edge{
		VSCodeBaseURL:      productionVSCodeDiscoveryBaseURL,
		MarketplaceBaseURL: productionMarketplaceDiscoveryBaseURL,
		Client:             newDiscoveryClient(),
	}
}

func productionCodexModelsEdge() catalog.ModelsEdge {
	return catalog.ModelsEdge{
		BaseURL: productionCodexModelsBaseURL,
		Client:  newCodexModelsClient(),
	}
}

// configuredCodexModels keeps the opt-in boundary at the composition root: a
// disabled Codex catalog registers no cached value and performs no GitHub read.
func configuredCodexModels(cfg config.ServeConfig, edge catalog.ModelsEdge, registry *cache.Registry, base *slog.Logger) *cache.Value[[]byte] {
	if !cfg.CodexCatalogEnabled {
		return nil
	}
	return catalog.NewModelsCache(catalog.ModelsCacheConfig{
		RefreshInterval: cfg.CodexCatalogRefreshInterval,
	}, edge, registry, logging.ForComponent(base, "internal/cache"))
}

// configuredUsagePricing keeps pricing refresh behind the Usage meter's opt-in
// boundary. Registration completes before runBoundServe starts registry priming.
func configuredUsagePricing(cfg config.ServeConfig, remote pricing.Remote, registry *cache.Registry, base *slog.Logger) pricing.Source {
	if !cfg.ShimUsageMeterEnabled {
		return nil
	}
	return pricing.NewCachedSource(pricing.CacheConfig{
		RefreshInterval: cfg.UsagePricingRefreshInterval,
	}, remote, registry, logging.ForComponent(base, "internal/cache"))
}

// newDiscoveryClient returns a dedicated plain client for the two public
// Microsoft discovery endpoints. It carries no Copilot credentials or
// impersonation transport, and each discovery request owns its timeout.
func newDiscoveryClient() *http.Client {
	return &http.Client{}
}

// newCodexModelsClient is credential-isolated from both the GitHub OAuth token
// exchange and Copilot forwarding. Each edge call owns its five-second bound.
func newCodexModelsClient() *http.Client { return &http.Client{} }

// runLogin is the login lifecycle: resolve LoginConfig, build the logger, then
// run the GitHub OAuth device flow with production defaults (real hosts, a real
// client, real sleep). The device flow prints its prompts/confirmations to
// stdout; a terminal error (expired/denied device code, or a write failure) is
// returned for the top-level translator to print with a non-zero exit.
func runLogin(ctx context.Context, flags *config.LoginFlags, lookupEnv func(string) (string, bool), stdout io.Writer) error {
	cfg, err := flags.Resolve(lookupEnv)
	if err != nil {
		return err
	}

	// login reuses the shared logger, which reads only the logging fields.
	base, closer, err := logging.New(config.ServeConfig{
		LogLevel:  cfg.LogLevel,
		LogFormat: cfg.LogFormat,
		LogFile:   cfg.LogFile,
	})
	if err != nil {
		return err
	}
	defer func() { _ = closer.Close() }()
	slog.SetDefault(base)
	logger := logging.ForComponent(base, "cmd/copilotd")

	logger.Info("starting device-flow login",
		slog.String(logging.BuildKey, build.String()),
		slog.Any(logging.ConfigKey, cfg),
	)

	// Production defaults: the empty base URLs resolve to https://github.com and
	// https://api.github.com inside identity.Login; a dedicated client and the
	// real ctx-honoring sleep are used. The e2e/unit tests inject stubs + a fast
	// sleep through the same DeviceFlowConfig seam.
	return identity.Login(ctx, logging.ForComponent(base, "internal/identity"), identity.DeviceFlowConfig{
		HTTPClient:    newLoginClient(),
		ClientID:      cfg.GithubClientID,
		Scope:         cfg.GithubScope,
		TokenFilePath: cfg.GithubOAuthTokenFile,
		Stdout:        stdout,
	})
}

// newLoginClient returns the HTTP client for the device flow. No client-level
// Timeout is set: the flow is bounded by the device code's expiry (surfaced as
// expired_token) and the caller's context.
func newLoginClient() *http.Client {
	return &http.Client{}
}
