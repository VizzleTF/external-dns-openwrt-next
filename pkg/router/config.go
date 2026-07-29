package router

type Config struct {
	HealthCheckPath string `mapstructure:"healthcheck_path"`
	Port            string `mapstructure:"port"`
}

func DefaultConfig() *Config {
	return &Config{
		HealthCheckPath: "/ping",
		Port:            "8888",
	}
}
