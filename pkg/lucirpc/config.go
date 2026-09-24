package lucirpc

type Auth struct {
	Username string `mapstructure:"username"`
	Password string `mapstructure:"password"`
}

type Config struct {
	Hostname           string `mapstructure:"hostname"`
	Port               int    `mapstructure:"port"`
	SSL                bool   `mapstructure:"ssl"`
	Timeout            int    `mapstructure:"timeout"`
	InsecureSkipVerify bool   `mapstructure:"insecure_skip_verify"`
	Auth               Auth   `mapstructure:"auth"`
}

func DefaultConfig() *Config {
	return &Config{
		Port:    443,
		SSL:     true,
		Timeout: 15,
	}
}
