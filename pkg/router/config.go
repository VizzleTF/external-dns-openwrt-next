package router

type Config struct {
	// Address the provider API binds to. ExternalDNS runs this webhook as a
	// sidecar and calls it over the loopback interface, so the default keeps it
	// off the pod IP: nothing here authenticates a caller, and a caller can
	// rewrite the router's DNS.
	Address string `mapstructure:"address"`
	Port    string `mapstructure:"port"`
	// HealthCheckPort carries /healthz and /metrics, on every interface, because the
	// kubelet reaches a probe through the pod IP rather than loopback, and a
	// scrape comes from Prometheus rather than the sidecar next door. It is a
	// separate listener for exactly that reason.
	HealthCheckPort string `mapstructure:"healthcheck_port"`
}

// DefaultConfig follows the ExternalDNS webhook provider specification: the
// provider API on 8888, localhost only, and the observability endpoints on
// 8080, bound to all interfaces.
func DefaultConfig() *Config {
	return &Config{
		Address:         "127.0.0.1",
		Port:            "8888",
		HealthCheckPort: "8080",
	}
}
