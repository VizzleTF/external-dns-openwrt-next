package openwrt

import (
	"fmt"

	"github.com/VizzleTF/external-dns-openwrt-next/pkg/lucirpc"
)

// How dnsmasq is made to pick up committed changes.
const (
	// ReloadStrategyRestart runs `/etc/init.d/dnsmasq restart` through the
	// rpc/sys endpoint. The default, and the only strategy that reliably
	// applies both record types:
	//
	//   - A records land in the hostfile /tmp/hosts/dhcp.*, which dnsmasq
	//     re-reads on SIGHUP;
	//   - CNAME records land as `--cname=` in /var/etc/dnsmasq.conf.*, which
	//     dnsmasq reads once, at startup.
	//
	// So nothing short of a restart picks up a CNAME. It costs roughly a
	// second of DNS/DHCP downtime and only runs when records actually changed;
	// DHCP leases survive in /tmp/dhcp.leases.
	ReloadStrategyRestart = "restart"

	// ReloadStrategyUciApply calls `uci apply` with no arguments. It commits
	// and applies EVERY pending UCI config, not just dhcp, so anything an
	// admin left staged on the router is applied too. Use it when the RPC user
	// has no access to rpc/sys.
	ReloadStrategyUciApply = "uci-apply"

	// ReloadStrategyNone reproduces the upstream behaviour: commit only.
	// Records land in /etc/config/dhcp but dnsmasq keeps serving the old set
	// until something else restarts it.
	ReloadStrategyNone = "none"
)

// ownershipOption is the UCI option used to mark records this provider owns.
// UCI section handlers read only the options they know — `dhcp_domain_add`
// reads `name`/`ip`, `dhcp_cname_add` reads `cname`/`target` — so an extra
// option is inert and never reaches the generated dnsmasq config.
const ownershipOption = "external_dns"

type Config struct {
	LuciRPC *lucirpc.Config `mapstructure:"lucirpc"`
	// ReloadStrategy selects how dnsmasq is reloaded after a commit.
	ReloadStrategy string `mapstructure:"reloadStrategy"`

	// OwnershipID scopes the provider to the records it created itself.
	//
	// When set, every record written gets `external_dns=<OwnershipID>` and
	// GetDNSRecords returns only sections carrying that exact value. Records
	// created by hand stay invisible: ExternalDNS cannot update or delete what
	// it never sees, which is what makes `policy: sync` safe on a router that
	// also holds manually maintained entries. An unmarked section that already
	// matches a record exactly is adopted rather than duplicated.
	//
	// Empty (the default) disables ownership entirely and every domain/cname
	// section is reported — do NOT combine that with `policy: sync`.
	OwnershipID string `mapstructure:"ownershipID"`
}

func DefaultConfig() *Config {
	return &Config{
		LuciRPC:        lucirpc.DefaultConfig(),
		ReloadStrategy: ReloadStrategyRestart,
	}
}

func validateReloadStrategy(strategy string) error {
	switch strategy {
	case ReloadStrategyRestart, ReloadStrategyUciApply, ReloadStrategyNone:
		return nil
	default:
		return fmt.Errorf(
			"invalid reload strategy %q, expected one of %q, %q, %q",
			strategy, ReloadStrategyRestart, ReloadStrategyUciApply, ReloadStrategyNone,
		)
	}
}
