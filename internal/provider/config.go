package provider

import (
	"github.com/VizzleTF/external-dns-openwrt-next/pkg/openwrt"
	"github.com/VizzleTF/external-dns-openwrt-next/pkg/status"
)

type Config struct {
	OpenWRT *openwrt.Config `mapstructure:"openwrt"`
	Status  *status.Config  `mapstructure:"status"`
}

func DefaultConfig() *Config {
	return &Config{
		OpenWRT: openwrt.DefaultConfig(),
		Status:  status.DefaultConfig(),
	}
}
