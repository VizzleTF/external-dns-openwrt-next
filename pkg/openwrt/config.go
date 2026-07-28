package openwrt

import (
	"fmt"

	"github.com/VizzleTF/external-dns-openwrt-webhook/pkg/lucirpc"
)

// How dnsmasq is made to pick up committed changes.
const (
	// ReloadStrategyDnsmasq runs `/etc/init.d/dnsmasq reload` through the
	// rpc/sys endpoint. Default: it is the narrowest option — no other
	// service and no other UCI config is touched.
	ReloadStrategyDnsmasq = "dnsmasq"

	// ReloadStrategyUciApply calls `uci apply` with no arguments. It commits
	// and applies EVERY pending UCI config, not just dhcp, so anything an
	// admin left staged on the router is applied too. Use it when the RPC user
	// has no access to rpc/sys.
	ReloadStrategyUciApply = "uci-apply"

	// ReloadStrategyNone reproduces the upstream behaviour: commit only.
	// Records land in /etc/config/dhcp but dnsmasq keeps serving the old set
	// until something else reloads it.
	ReloadStrategyNone = "none"
)

type Config struct {
	LuciRPC *lucirpc.Config `mapstructure:"lucirpc"`
	// ReloadStrategy selects how dnsmasq is reloaded after a commit.
	ReloadStrategy string `mapstructure:"reloadStrategy"`
}

func DefaultConfig() *Config {
	return &Config{
		LuciRPC:        lucirpc.DefaultConfig(),
		ReloadStrategy: ReloadStrategyDnsmasq,
	}
}

func validateReloadStrategy(strategy string) error {
	switch strategy {
	case ReloadStrategyDnsmasq, ReloadStrategyUciApply, ReloadStrategyNone:
		return nil
	default:
		return fmt.Errorf(
			"invalid reload strategy %q, expected one of %q, %q, %q",
			strategy, ReloadStrategyDnsmasq, ReloadStrategyUciApply, ReloadStrategyNone,
		)
	}
}
