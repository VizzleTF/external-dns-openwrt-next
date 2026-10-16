package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/VizzleTF/external-dns-openwrt-next/internal/provider"
	"github.com/VizzleTF/external-dns-openwrt-next/pkg/config"
	"github.com/VizzleTF/external-dns-openwrt-next/pkg/logger"
	"github.com/VizzleTF/external-dns-openwrt-next/pkg/metrics"
	"github.com/VizzleTF/external-dns-openwrt-next/pkg/router"
	"github.com/VizzleTF/external-dns-openwrt-next/pkg/status"
	"github.com/VizzleTF/external-dns-openwrt-next/pkg/webhook"
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
	registry := metrics.NewRegistry()
	metrics.Build(registry)

	var publish func([]status.Record)
	if cfg.Provider.Status.Enabled {
		client, err := status.InCluster(cfg.Provider.Status.Namespace)
		if err != nil {
			return fmt.Errorf("setup record status: %w", err)
		}
		syncer := status.NewSyncer(client, log, registry)
		publish = syncer.Publish

		// Deferred, so it stops after the server and no request publishes
		// into it once it is gone. A write still in flight is dropped.
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			syncer.Run(ctx)
		}()
		defer func() {
			cancel()
			<-done
		}()
	}

	dnsProvider, err := provider.New(cfg.Provider, log, publish)
	if err != nil {
		return fmt.Errorf("setup provider: %w", err)
	}

	srv := router.New(cfg.Router, log, registry, webhook.New(dnsProvider, log, registry).Register)

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
