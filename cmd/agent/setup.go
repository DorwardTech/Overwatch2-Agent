package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"overwatch/agent/internal/platform"
	"overwatch/agent/internal/setupui"
)

// runSetup opens the configuration page in a browser. It is the answer to the
// only hard part of installing this agent at a venue: somebody has to put a
// URL, a token and a host into a file, correctly, on a machine behind a bar,
// and today that means an elevated Notepad and no way to tell whether any of
// it was right until the log says so.
func runSetup(opts options) int {
	// Settings already in the file are what the page opens on; loading them
	// also picks up a LOG_FILE the operator has set.
	_, _, _ = applyEnvFile(opts)

	configPath := opts.configPath
	if configPath == "" {
		configPath = platform.DefaultEnvFile()
	}
	if configPath == "" {
		fmt.Fprintln(os.Stderr, "overwatch-agent: there is no default configuration file on this platform — say where it should go with --config PATH (or --data-dir PATH)")
		return exitUsage
	}
	dataDir := platform.DataDir()

	if err := prepareStorage(dataDir); err != nil {
		fmt.Fprintf(os.Stderr, "overwatch-agent: %v\n", err)
		return exitFailure
	}

	logPath := os.Getenv("LOG_FILE")
	if logPath == "" {
		logPath = platform.DefaultLogFile()
	}

	srv, err := setupui.New(configPath, dataDir, logPath, serviceControl(opts))
	if err != nil {
		fmt.Fprintf(os.Stderr, "overwatch-agent: %v\n", err)
		return exitFailure
	}
	url, err := srv.Listen()
	if err != nil {
		fmt.Fprintf(os.Stderr, "overwatch-agent: %v\n", err)
		return exitFailure
	}

	fmt.Println("Overwatch Agent setup is open in your browser.")
	fmt.Println()
	fmt.Println("  " + url)
	fmt.Println()
	fmt.Println("If no browser opened, copy that address into one. It works only on this")
	fmt.Println("computer, and only until this window is closed.")
	fmt.Println()
	fmt.Println("Leave this window open while you work. Press Ctrl+C when you are done.")

	if err := openBrowser(url); err != nil {
		fmt.Printf("\n(The browser could not be opened automatically: %v)\n", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := srv.Serve(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "overwatch-agent: %v\n", err)
		return exitFailure
	}
	fmt.Println("Setup closed.")
	return exitOK
}
