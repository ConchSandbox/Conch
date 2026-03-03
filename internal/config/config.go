package config

type Config struct {
	AppName  string
	Version  string
	Debug    bool
	LogLevel string
}

func DefaultConfig() *Config {
	return &Config{
		AppName:  "Conch",
		Version:  "v1.0.0",
		Debug:    false,
		LogLevel: "info",
	}
}
