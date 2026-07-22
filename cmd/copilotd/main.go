// Command copilotd is the composition root for the copilotd proxy. It assembles
// a git-style subcommand tree — serve, login, help, version — wiring the internal
// packages together; it holds no business logic. `serve` runs the HTTP daemon
// (config load → logger → bind → signal-aware graceful shutdown); the other verbs
// provide discovery (help), build info (version), and GitHub OAuth device login.
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
	"strings"
	"syscall"

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
// Exit codes: version -> 0; bare/help -> 0; config error -> 1; bind or
// serve error -> 1; unknown subcommand -> 1.
func run(args []string, lookupEnv func(string) (string, bool), stdout, stderr io.Writer) int {
	root := buildCommand(lookupEnv, stdout, stderr)
	switch err := root.ParseAndRun(context.Background(), args); {
	case err == nil:
		return 0
	case errors.Is(err, errServeFailed):
		// Already reported via the structured logger; just carry the exit code.
		return 1
	case errors.Is(err, ff.ErrHelp):
		if err := validateHelpRequest(args); err != nil {
			writeCLIError(root, stderr, err)
			return 1
		}
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
// configuration or executing a command.
func validateHelpRequest(args []string) error {
	syntaxArgs := append([]string(nil), args...)
	for {
		root := buildCommand(func(string) (string, bool) { return "", false }, io.Discard, io.Discard)
		err := root.Parse(syntaxArgs)
		if errors.Is(err, ff.ErrHelp) {
			selected := root.GetSelected()
			if selected == nil || selected.Flags == nil {
				return err
			}
			remaining := selected.Flags.GetArgs()
			helpIndex := len(syntaxArgs) - len(remaining)
			if len(remaining) == 0 || helpIndex < 0 || helpIndex >= len(syntaxArgs) {
				return err
			}
			syntaxArgs = append(syntaxArgs[:helpIndex:helpIndex], syntaxArgs[helpIndex+1:]...)
			continue
		}
		if err != nil {
			return err
		}

		selected := root.GetSelected()
		if selected == nil || selected.Flags == nil {
			return nil
		}
		operands := selected.Flags.GetArgs()
		if selected == root && len(operands) > 0 {
			return fmt.Errorf("unknown subcommand %q (run 'copilotd help')", operands[0])
		}
		allowed := 0
		if selected.Name == "help" {
			allowed = 1
		}
		return rejectSurplusOperands(selected.Name, operands, allowed)
	}
}

// buildCommand assembles the subcommand tree. Root and informational commands
// have no operational flags; serve and login each own an independent flag set.
func buildCommand(lookupEnv func(string) (string, bool), stdout, stderr io.Writer) *ff.Command {
	rootFlags := ff.NewFlagSet("copilotd")
	serveFlags := ff.NewFlagSet("serve")
	serveCfg := config.RegisterServe(serveFlags)
	loginFlags := ff.NewFlagSet("login")
	loginCfg := config.RegisterLogin(loginFlags)

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
	root.Subcommands = []*ff.Command{versionCmd, helpCmd, serveCmd, loginCmd}
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

// runServe is the serve lifecycle: resolve config, build the logger and set it as
// the slog default, resolve the GitHub OAuth token and construct the real minting
// Manager (failing fast, before any bind, when no token source is present), bind
// the listener, then run bounded discovery followed by the cache-warming startup
// mint in the background while serving. A signal-aware context whose
// re-armed handler lets a second signal hard-kill a wedged shutdown owns every
// background task. Errors after the logger is up are reported through it and
// returned as errServeFailed so the caller does not double-report them; a
// pre-logger config error is returned raw for the top-level translator to print.
func runServe(ctx context.Context, flags *config.ServeFlags, lookupEnv func(string) (string, bool)) error {
	cfg, err := flags.Resolve(lookupEnv)
	if err != nil {
		return err
	}

	logger, closer, err := logging.New(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = closer.Close() }()

	// Route stray global slog calls and dependency logs through our handler.
	slog.SetDefault(logger)

	logger.Info("starting copilotd",
		slog.String("build", build.String()),
		slog.Any("config", cfg),
	)
	logCodexCatalogStaging(logger, cfg)
	registry := configuredShimRegistry(cfg)
	logShimChain(logger, registry)

	// Credential-presence check + real credential Provider, assembled BEFORE the
	// listener binds so a missing OAuth token fails fast (non-zero exit) without
	// ever serving. Production points the exchange at the real GitHub host ("" ⇒
	// api.github.com) with a dedicated client; the e2e test injects stubs via the
	// same buildServeProvider seam.
	cacheRegistry := cache.NewRegistry()
	mgr, imp, err := buildServeProvider(cfg, logger, "", newExchangeClient(), productionDiscoveryEdge(), cacheRegistry)
	if err != nil {
		// Already carries the "run copilotd login" guidance when no source yields a
		// token; reported through the logger, then a silent non-zero exit.
		logger.Error("cannot start: resolving the GitHub OAuth token failed", slog.Any("error", err))
		return errServeFailed
	}
	codexModels := configuredCodexModels(cfg, productionCodexModelsEdge(), cacheRegistry, logger)

	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		// Distinct from a serve error: the process never began serving.
		logger.Error("bind failed", slog.String("addr", cfg.Addr), slog.Any("error", err))
		return errServeFailed
	}

	serveCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	// After the first signal, restore default signal handling so a second signal
	// hard-kills the process if graceful shutdown wedges.
	go func() {
		<-serveCtx.Done()
		stop()
	}()

	if err := runBoundServe(serveCtx, cfg, logger, mgr, imp, codexModels, cacheRegistry, ln); err != nil {
		logger.Error("server error", slog.Any("error", err))
		return errServeFailed
	}
	return nil
}

// runBoundServe starts the background impersonation/mint lifecycle only after
// its caller has supplied an already-bound listener. That ordering keeps
// /healthz and the locally-ready /readyz available while bounded startup
// discovery is in progress. Neither discovery nor startup mint outcomes gate
// readiness or request admission.
func runBoundServe(ctx context.Context, cfg config.ServeConfig, logger *slog.Logger, mgr *identity.Manager, imp *impersonation.Set, codexModels *cache.Value[[]byte], cacheRegistry *cache.Registry, ln net.Listener) error {
	go runServeStartup(ctx, cacheRegistry, mgr, logger)

	registry := configuredShimRegistry(cfg)
	fwd := forward.New(mgr, forward.NewClient(cfg.ResponseHeaderTimeout), cfg.OutboundTimeout, cfg.WriteTimeout, cfg.StreamIdleTimeout, cfg.StreamKeepaliveInterval, cfg.MaxRequestBytes, cfg.MaxBufferedResponseBytes, registry, forward.WithLogger(logger))
	wsDialClient := &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment}}
	wsAccepts := server.NewWsAcceptCounter()
	wsTerminals := server.NewWsSessionTerminalCounter()
	wsProxy := wsforward.New(mgr, wsDialClient, cfg.WebSocketHandshakeTimeout, cfg.WriteTimeout, cfg.MaxRequestBytes, logger, wsforward.WsMetrics{
		Accept:          wsAccepts,
		SessionTerminal: wsTerminals,
	})
	streamOutcomes := server.NewStreamOutcomeCounter()

	return server.New(cfg, logger, mgr, server.ReadyObservers{
		Impersonation: imp,
		Caches:        cacheRegistry,
	}, fwd, wsProxy, streamOutcomes, server.WithCodexModels(codexModels)).Run(ctx, ln)
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

func logCachedValueStartupOutcomes(logger *slog.Logger, observed []cache.Status) {
	for _, status := range observed {
		logger.Info("startup cached value refresh outcome",
			slog.String("cached_value", status.Name),
			slog.String("source", status.Source),
			slog.String("version", status.Version))
	}
}

func logCodexCatalogStaging(logger *slog.Logger, cfg config.ServeConfig) {
	if cfg.Codex.Enabled || cfg.Codex.AutoReviewModel == "" {
		return
	}
	logger.Info("Codex reviewer is staged while the Codex catalog is disabled",
		slog.String("reviewer", cfg.Codex.AutoReviewModel))
}

func configuredShimRegistry(cfg config.ServeConfig) shim.Registry {
	registry := shim.CanonicalRegistry()
	for i := range registry {
		switch registry[i].Name {
		case "nop":
			registry[i].Enabled = cfg.ShimNopEnabled
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
	logger.Info("configured shim chain", slog.Any("enabled_shims", enabled))
}

// buildServeProvider assembles the real credential Provider and live
// impersonation Set for `serve`: it resolves the GitHub OAuth token (§6.5),
// seeds the Set with configured fallbacks and static identifiers, binds the
// injected discovery edge, and constructs the minting identity.Manager. It
// returns the resolve error unchanged (e.g. identity.ErrNoOAuthToken) so
// runServe can fail fast before binding a listener.
//
// githubBaseURL/httpClient and discoveryEdge are the injected network edges:
// production uses GitHub plus the two public Microsoft origins with separate
// plain clients, while tests point them at stubs. Every other Manager
// timing/clock knob is left to NewManager's production defaults.
func buildServeProvider(cfg config.ServeConfig, logger *slog.Logger, githubBaseURL string, httpClient *http.Client, discoveryEdge impersonation.Edge, cacheRegistry *cache.Registry) (*identity.Manager, *impersonation.Set, error) {
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
	}, discoveryEdge, cacheRegistry, logger)
	mgr := identity.NewManager(identity.ManagerConfig{
		OAuthToken:    oauthToken,
		GitHubBaseURL: githubBaseURL,
		HTTPClient:    httpClient,
		// Direct assignment is the composition-root proof that the live Set
		// satisfies identity.Impersonation without reversing package dependencies.
		Impersonation:      imp,
		StartupMintRetries: cfg.StartupMintRetries,
		Logger:             logger,
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
func configuredCodexModels(cfg config.ServeConfig, edge catalog.ModelsEdge, registry *cache.Registry, logger *slog.Logger) *cache.Value[[]byte] {
	if !cfg.Codex.Enabled {
		return nil
	}
	return catalog.NewModelsCache(catalog.ModelsCacheConfig{
		RefreshInterval: cfg.CodexCatalogRefreshInterval,
	}, edge, registry, logger)
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
	logger, closer, err := logging.New(config.ServeConfig{
		LogLevel:  cfg.LogLevel,
		LogFormat: cfg.LogFormat,
		LogFile:   cfg.LogFile,
	})
	if err != nil {
		return err
	}
	defer func() { _ = closer.Close() }()
	slog.SetDefault(logger)

	logger.Info("starting device-flow login",
		slog.String("build", build.String()),
		slog.Any("config", cfg),
	)

	// Production defaults: the empty base URLs resolve to https://github.com and
	// https://api.github.com inside identity.Login; a dedicated client and the
	// real ctx-honoring sleep are used. The e2e/unit tests inject stubs + a fast
	// sleep through the same DeviceFlowConfig seam.
	return identity.Login(ctx, identity.DeviceFlowConfig{
		HTTPClient:    newLoginClient(),
		ClientID:      cfg.GithubClientID,
		Scope:         cfg.GithubScope,
		TokenFilePath: cfg.GithubOAuthTokenFile,
		Stdout:        stdout,
		Logger:        logger,
	})
}

// newLoginClient returns the HTTP client for the device flow. No client-level
// Timeout is set: the flow is bounded by the device code's expiry (surfaced as
// expired_token) and the caller's context.
func newLoginClient() *http.Client {
	return &http.Client{}
}
