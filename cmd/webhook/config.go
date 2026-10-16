package main

import (
	"github.com/VizzleTF/external-dns-openwrt-next/internal/provider"
	"github.com/VizzleTF/external-dns-openwrt-next/pkg/logger"
	"github.com/VizzleTF/external-dns-openwrt-next/pkg/router"
)

type Config struct {
	ShutdownTimeout int              `mapstructure:"shutdown_timeout_seconds"`
	Log             *logger.Config   `mapstructure:"log"`
	Router          *router.Config   `mapstructure:"router"`
	Provider        *provider.Config `mapstructure:"provider"`
}

func defaultConfig() *Config {
	return &Config{
		// Inside the kubelet's default 30 s grace period, with room to exit.
		// A change set still running gets that long to reach the router.
		ShutdownTimeout: 25,
		Log:             logger.DefaultConfig(),
		Router:          router.DefaultConfig(),
		Provider:        provider.DefaultConfig(),
	}
}
