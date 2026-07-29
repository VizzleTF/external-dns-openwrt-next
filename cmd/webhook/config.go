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
		ShutdownTimeout: 5,
		Log:             logger.DefaultConfig(),
		Router:          router.DefaultConfig(),
		Provider:        provider.DefaultConfig(),
	}
}
