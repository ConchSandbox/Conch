package rootfs

// Option defines the type for configuration option functions.
type Option func(*Config)

func WithKey(key string) Option {
	return func(c *Config) {
		c.Key = key
	}
}

// WithExtraArgs appends additional command-line arguments to the Config.
func WithExtraArgs(args ...string) Option {
	return func(c *Config) {
		c.ExtraArgs = append(c.ExtraArgs, args...)
	}
}

// NewConfig creates a new Config instance and applies the given options.
func NewConfig(opts ...Option) *Config {
	config := &Config{}
	for _, opt := range opts {
		opt(config)
	}
	return config
}
