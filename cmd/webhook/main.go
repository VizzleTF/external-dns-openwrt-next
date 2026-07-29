package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/VizzleTF/external-dns-openwrt-next/internal/provider"
	"github.com/VizzleTF/external-dns-openwrt-next/pkg/config"
	"github.com/VizzleTF/external-dns-openwrt-next/pkg/logger"
	"github.com/VizzleTF/external-dns-openwrt-next/pkg/router"
	"github.com/VizzleTF/external-dns-openwrt-next/pkg/webhook"
	"log/slog"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

// run owns the whole lifecycle and returns an error instead of exiting, so
// every failure path is handled the same way and deferred cleanup still runs.
func run() error {
	cfg := defaultConfig()
	if err := config.Read(cfg); err != nil {
		return fmt.Errorf("read config: %w", err)
	}

	log, err := logger.New(cfg.Log)
	if err != nil {
		return fmt.Errorf("setup logger: %w", err)
	}
	dnsProvider, err := provider.New(cfg.Provider, log)
	if err != nil {
		return fmt.Errorf("setup provider: %w", err)
	}

	srv := router.New(cfg.Router, log, webhook.New(dnsProvider, log))

	// Buffered: a signal arriving before the receive must not be dropped.
	serverErr := make(chan error, 1)
	go func() { serverErr <- srv.Run() }()

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)

	select {
	case err := <-serverErr:
		if err != nil {
			return fmt.Errorf("http server: %w", err)
		}
		return nil
	case sig := <-signals:
		log.Info("termination signal received, shutting down", slog.String("signal", sig.String()))
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.ShutdownTimeout)*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		return fmt.Errorf("shutdown server: %w", err)
	}

	log.Info("service shutdown completed")
	return nil
}
