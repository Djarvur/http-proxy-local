// Package main is the entrypoint for the http-proxy-local binary.
//
// It is deliberately thin: flag parsing, signal handling, and a single call
// into the composition root (Run in wire.go). All business logic lives under
// internal/.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/Djarvur/http-proxy-local/internal/version"
)

// versionString returns the line printed by --version: "http-proxy-local <Version>".
func versionString() string {
	return "http-proxy-local " + version.Version
}

func main() {
	// Register all flags on the default FlagSet before parsing, then parse
	// once. --version and -v both bind to showVersion (either one triggers the
	// version short-circuit). The short-circuit happens before any startup
	// work — no config load, no listeners — and exits 0.
	var showVersion bool
	flag.BoolVar(&showVersion, "version", false, "print version and exit")
	flag.BoolVar(&showVersion, "v", false, "print version and exit (shorthand for --version)")
	configPath := flag.String("config", "", "path to config.yaml (defaults to ~/.config/http-proxy-local/config.yaml)")
	flag.Parse()

	if showVersion {
		log.Println(versionString())
		return
	}

	log.Printf("http-proxy-local %s starting", version.Version)
	if err := mainErr(context.Background(), *configPath); err != nil {
		log.Printf("http-proxy-local exited with error: %v", err)
		os.Exit(1)
	}
}

// mainErr is the non-os.Exit body of main so deferred handlers actually run
// (calling os.Exit directly skips defers — caught by gocritic).
func mainErr(parent context.Context, configPath string) error {
	ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer stop()

	return Run(ctx, configPath)
}
