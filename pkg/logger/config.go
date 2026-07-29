package logger

type Config struct {
	// Level is one of debug, info, warn, error.
	Level string `mapstructure:"level"`
	// Encoding is "json" or "console".
	Encoding string `mapstructure:"encoding"`
}

func DefaultConfig() *Config {
	return &Config{
		Level:    "info",
		Encoding: "json",
	}
}
