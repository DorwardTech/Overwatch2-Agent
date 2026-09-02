// Overwatch site agent: connects to the local O-Zone WebSocket API, batches
// telemetry, and pushes it to the central server. Buffers on outage; reconnects
// with backoff; shuts down gracefully.
//
// One binary runs the same way everywhere: as a container on Linux, and on
// Windows either in a console or as a Windows service. The `run` command (the
// default, so a bare invocation is still the container entrypoint) is the
// agent itself; the remaining commands manage the Windows service.
package main

import (
	"context"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"overwatch/agent/internal/app"
	"overwatch/agent/internal/config"
	legacyapp "overwatch/agent/internal/legacy/app"
	"overwatch/agent/internal/logfile"
	"overwatch/agent/internal/platform"
	"overwatch/agent/internal/version"
)

// envTemplate is written into a new installation's data directory, so the
// operator edits a file that already lists the settings instead of starting
// from nothing. The release archive ships the same file as agent.env.example.
//
//go:embed agent.env.example
var envTemplate string

// options are the flags shared by `run` and `install`.
type options struct {
	configPath string // --config: the KEY=VALUE file applied at startup
	dataDir    string // --data-dir: where the cache, buffer, log and config live
}

const (
	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2
)

const usageText = `Usage: overwatch-agent [command] [flags]

Commands:
  run          run the agent (default)
  healthcheck  exit 0 if the running agent's health endpoint answers
  version      print the agent version
  install      install the Windows service           (Windows, elevated)
  uninstall    stop and remove the Windows service   (Windows, elevated)
  start        start the Windows service             (Windows, elevated)
  stop         stop the Windows service              (Windows, elevated)
  restart      stop, then start the Windows service  (Windows, elevated)
  status       show the Windows service state

Flags (run, install, healthcheck):
  --config PATH    KEY=VALUE configuration file (default: <data-dir>\agent.env;
                   environment variables take precedence over the file)
  --data-dir PATH  data directory for the cache, buffer, log and config
                   (default: %ProgramData%\Overwatch Agent on Windows)
`

func main() {
	cmd, opts, err := parseArgs(os.Args[1:])
	if errors.Is(err, flag.ErrHelp) {
		fmt.Fprint(os.Stdout, usageText)
		os.Exit(exitOK)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "overwatch-agent: %v\n\n%s", err, usageText)
		os.Exit(exitUsage)
	}
	if opts.dataDir != "" {
		// Everything downstream — path defaults, the config file, the service
		// installer — reads the data directory from the environment.
		_ = os.Setenv(platform.DataDirEnv, opts.dataDir)
	}

	switch cmd {
	case "run":
		os.Exit(runAgent(opts))
	case "healthcheck":
		os.Exit(healthcheck(opts))
	case "version":
		fmt.Println(version.Value)
	case "help":
		fmt.Fprint(os.Stdout, usageText)
	case "install", "uninstall", "start", "stop", "restart", "status":
		os.Exit(serviceCommand(cmd, opts))
	default:
		fmt.Fprintf(os.Stderr, "overwatch-agent: unknown command %q\n\n%s", cmd, usageText)
		os.Exit(exitUsage)
	}
}

// parseArgs splits the command line into a command and its flags. The command
// is the first argument when it is not a flag, and `run` otherwise, so a bare
// invocation — the container entrypoint — still just runs the agent.
func parseArgs(args []string) (string, options, error) {
	cmd := "run"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	var opts options
	fs := flag.NewFlagSet("overwatch-agent", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&opts.configPath, "config", "", "")
	fs.StringVar(&opts.dataDir, "data-dir", "", "")
	if err := fs.Parse(args); err != nil {
		return cmd, opts, err
	}
	if fs.NArg() > 0 {
		return cmd, opts, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	// A service starts in System32, so a relative path must be resolved now,
	// while the working directory is still the operator's.
	for _, p := range []*string{&opts.configPath, &opts.dataDir} {
		if *p != "" {
			if abs, err := filepath.Abs(*p); err == nil {
				*p = abs
			}
		}
	}
	return cmd, opts, nil
}

// runAgent is the `run` command: the agent for the life of the process.
func runAgent(opts options) int {
	if isWindowsService() {
		return runAsService(opts)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return agentMain(ctx, opts, false)
}

// agentMain is the agent proper, shared by the console and the service: it
// prepares the run, then drives the poll loop until ctx is cancelled. It
// returns the process exit code rather than exiting, because under the service
// control manager an exit has to be reported, not performed.
func agentMain(ctx context.Context, opts options, service bool) int {
	cfg, closeLog, err := prepareAgent(opts, service)
	defer closeLog()
	if err != nil {
		log.Printf("[agent] configuration error: %v", err)
		return exitFailure
	}
	return runLoaded(ctx, cfg)
}

// prepareAgent does everything that can fail outright — the configuration
// file, the log destination, the configuration itself — and returns the
// loaded configuration. It is separate from the run so a service can report a
// failed start, rather than reporting itself running and stopping a moment
// later. The returned func closes the log and must be called either way.
func prepareAgent(opts options, service bool) (config.Config, func(), error) {
	envPath, applied, envErr := applyEnvFile(opts)

	closeLog := setupLogging(service)
	log.Println("[agent] Overwatch agent starting")
	if applied > 0 {
		log.Printf("[agent] loaded %d setting(s) from %s", applied, envPath)
	}
	if envErr != nil {
		return config.Config{}, closeLog, envErr
	}
	cfg, err := config.Load()
	if err != nil {
		return cfg, closeLog, err
	}
	log.Printf("[agent] version %s · O-Zone %s:%s -> central %s", cfg.Version, cfg.OzoneHost, cfg.OzonePort, cfg.CentralURL)
	return cfg, closeLog, nil
}

// runLoaded runs the poll loop for an already-loaded configuration until ctx
// is cancelled.
func runLoaded(ctx context.Context, cfg config.Config) int {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			log.Println("[agent] shutdown signal received, draining…")
		case <-done:
		}
	}()

	if cfg.Mode == "legacy" {
		log.Printf("[agent] legacy (Nexus) mode: %s + %s", config.RedactDSN(cfg.NexusDSN), cfg.LasertagURL)
		legacy, err := legacyapp.New(cfg)
		if err != nil {
			log.Printf("[agent] legacy startup error: %v", err)
			close(done)
			return exitFailure
		}
		legacy.Run(ctx)
	} else {
		app.New(cfg).Run(ctx)
	}
	close(done)
	log.Println("[agent] stopped")
	return exitOK
}

// applyEnvFile loads the configuration file named by --config, AGENT_ENV_FILE
// or the data directory's default, in that order, into the environment. Only
// a file asked for by name has to exist; the default may simply be absent.
func applyEnvFile(opts options) (path string, applied int, err error) {
	path, explicit := opts.configPath, true
	if path == "" {
		path = os.Getenv("AGENT_ENV_FILE")
		explicit = path != ""
	}
	if path == "" {
		path = platform.DefaultEnvFile()
	}
	if path == "" {
		return "", 0, nil
	}
	applied, err = config.LoadEnvFile(path)
	switch {
	case err == nil:
		return path, applied, nil
	case errors.Is(err, fs.ErrNotExist) && !explicit:
		return path, 0, nil
	default:
		return path, 0, fmt.Errorf("%s: %w", path, err)
	}
}

// setupLogging routes the standard logger. A container logs to the console
// and the runtime keeps it. Wherever a data directory is known (Windows) the
// agent keeps its own size-rotated file, and — unless it is a service, which
// has no console — still prints to the console as well. LOG_FILE overrides
// the location. The returned func closes the file.
func setupLogging(service bool) func() {
	log.SetFlags(log.LstdFlags | log.LUTC)
	path := os.Getenv("LOG_FILE")
	if path == "" {
		path = platform.DefaultLogFile()
	}
	if path == "" {
		return func() {}
	}
	w, err := logfile.Open(path, logfile.DefaultMaxBytes, logfile.DefaultKeep)
	if err != nil {
		msg := fmt.Sprintf("log file %s: %v — logging to the console only", path, err)
		log.Printf("[agent] %s", msg)
		if service {
			reportServiceError(msg) // the console does not exist; the event log does
		}
		return func() {}
	}
	if service {
		log.SetOutput(w)
	} else {
		log.SetOutput(io.MultiWriter(os.Stderr, w))
	}
	return func() { _ = w.Close() }
}

// healthcheck is the container healthcheck (distroless has no shell) and works
// the same on Windows: it asks the running agent's health endpoint. The
// configuration file is applied first so a non-default HEALTH_ADDR is found.
func healthcheck(opts options) int {
	_, _, _ = applyEnvFile(opts)
	url, err := healthURL(os.Getenv("HEALTH_ADDR"))
	if err != nil {
		return exitFailure
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(url)
	if err != nil || resp.StatusCode != http.StatusOK {
		return exitFailure
	}
	_ = resp.Body.Close()
	return exitOK
}

// healthURL turns the HEALTH_ADDR bind address into the loopback URL to probe.
// A wildcard or empty host is reached at 127.0.0.1; an explicit one as given.
func healthURL(addr string) (string, error) {
	if addr == "" {
		addr = ":8088"
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", err
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/healthz", nil
}
