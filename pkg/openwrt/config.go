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

	// ReloadStrategyReload runs `/etc/init.d/dnsmasq reload`.
	//
	// WARNING: verified ineffective on OpenWrt 25 with dnsmasq under ujail.
	// `reload_service()` is `rc_procd start_service; procd_send_signal dnsmasq`,
	// and the signal does not reach the jailed process — the files are
	// regenerated but the running dnsmasq keeps serving the previous set. Kept
	// for routers where dnsmasq is not jailed, and it never applies CNAMEs.
	ReloadStrategyReload = "reload"

	// legacyReloadStrategyDnsmasq is the old name for ReloadStrategyReload,
	// accepted so an outdated config does not fail the provider outright.
	legacyReloadStrategyDnsmasq = "dnsmasq"

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

// normaliseReloadStrategy resolves aliases.
func normaliseReloadStrategy(strategy string) string {
	if strategy == legacyReloadStrategyDnsmasq {
		return ReloadStrategyReload
	}
	return strategy
}

// DefaultOwnershipOption is the UCI option used to mark records this provider
// owns. UCI section handlers read only the options they know — `dhcp_domain_add`
// reads `name`/`ip`, `dhcp_cname_add` reads `cname`/`target` — so an extra
// option is inert and never reaches the generated dnsmasq config.
const DefaultOwnershipOption = "external_dns"

type Config struct {
	LuciRPC *lucirpc.Config `mapstructure:"lucirpc"`
	// ReloadStrategy selects how dnsmasq is reloaded after a commit.
	ReloadStrategy string `mapstructure:"reloadStrategy"`

	// OwnershipID scopes the provider to the records it created itself.
	//
	// When set, every record written gets `<OwnershipOption>=<OwnershipID>` and
	// GetDNSRecords returns only sections carrying that exact value. Records
	// created by hand stay invisible: ExternalDNS cannot update or delete what
	// it never sees, which is what makes `policy: sync` safe on a router that
	// also holds manually maintained entries.
	//
	// Empty (the default) disables ownership entirely and every domain/cname
	// section is reported — do NOT combine that with `policy: sync`.
	OwnershipID string `mapstructure:"ownershipID"`

	// OwnershipOption is the UCI option name holding the ownership ID.
	OwnershipOption string `mapstructure:"ownershipOption"`

	// AdoptExisting makes the provider take over an unowned section instead of
	// creating a duplicate, when one already matches the record identity
	// exactly. This is what migrates an existing deployment: the first
	// reconcile stamps the marker onto the records already on the router
	// rather than adding a second copy of each.
	AdoptExisting bool `mapstructure:"adoptExisting"`
}

func DefaultConfig() *Config {
	return &Config{
		LuciRPC:         lucirpc.DefaultConfig(),
		ReloadStrategy:  ReloadStrategyRestart,
		OwnershipID:     "",
		OwnershipOption: DefaultOwnershipOption,
		AdoptExisting:   true,
	}
}

// OwnershipEnabled reports whether the provider is scoped to its own records.
func (c *Config) OwnershipEnabled() bool {
	return c.OwnershipID != ""
}

func validateReloadStrategy(strategy string) error {
	switch normaliseReloadStrategy(strategy) {
	case ReloadStrategyRestart, ReloadStrategyReload, ReloadStrategyUciApply, ReloadStrategyNone:
		return nil
	default:
		return fmt.Errorf(
			"invalid reload strategy %q, expected one of %q, %q, %q, %q",
			strategy, ReloadStrategyRestart, ReloadStrategyReload,
			ReloadStrategyUciApply, ReloadStrategyNone,
		)
	}
}
