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
)

func main() {
	configPath := flag.String("config", "", "path to config.yaml (defaults to ~/.config/http-proxy-local/config.yaml)")
	flag.Parse()

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
