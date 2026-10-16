package openwrt

import (
	"github.com/VizzleTF/external-dns-openwrt-next/pkg/ubus"
)

// ownershipOption is the UCI option used to mark records this provider owns.
// UCI section handlers read only the options they know — `dhcp_domain_add`
// reads `name`/`ip`, `dhcp_cname_add` reads `cname`/`target` — so an extra
// option is inert and never reaches the generated dnsmasq config.
const ownershipOption = "external_dns"

type Config struct {
	// Tagged "lucirpc" so the PROVIDER_OPENWRT_LUCIRPC_* variables keep working.
	Ubus *ubus.Config `mapstructure:"lucirpc"`

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
		Ubus: ubus.DefaultConfig(),
	}
}
