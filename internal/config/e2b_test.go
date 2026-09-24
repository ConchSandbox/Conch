package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func loadE2BYAML(t *testing.T, data string) (*Config, error) {
	t.Helper()
	filename := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(filename, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	return LoadConfig(filename)
}

func TestE2BDefaultsDisablePublicListeners(t *testing.T) {
	t.Setenv("AENV_API_KEY", "")
	for _, cfg := range []*Config{DefaultConfig(), func() *Config {
		cfg, err := loadE2BYAML(t, "app:\n  name: conch\n")
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}()} {
		if cfg.E2B.ListenAddr != "" || cfg.Cluster.SchedulerAddr != "" {
			t.Fatal("default config enabled E2B or Scheduler reporting")
		}

	}
}

func TestE2BConfigEnvironmentAndStandaloneHeaderRouting(t *testing.T) {
	t.Setenv("AENV_API_KEY", "test-env-key")
	cfg, err := loadE2BYAML(t, `e2b:
  listen_addr: ":8000"
  sandbox_proxy_domains: []
`)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.E2B.APIKey != "test-env-key" || cfg.Cluster.SchedulerAddr != "" || len(cfg.E2B.SandboxProxyDomains) != 0 {
		t.Fatal("standalone config did not preserve environment key or empty domains")
	}

}

func TestLoadE2BClusterConfig(t *testing.T) {
	t.Setenv("AENV_API_KEY", "test-env-key")
	cfg, err := loadE2BYAML(t, `e2b:
  listen_addr: "10.0.0.1:8000"
  sandbox_proxy_domains: [" Sandboxes.Example.COM. ", ""]
cluster:
  scheduler_addr: "http://scheduler:9090/"
  node_id: " node-a "
  cluster_id: " cluster-a "
`)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Cluster.SchedulerAddr != "scheduler:9090" || cfg.Cluster.NodeID != "node-a" || cfg.Cluster.ClusterID != "cluster-a" {
		t.Fatalf("incorrect cluster config: %+v", cfg.Cluster)
	}
	if cfg.E2B.ListenAddr != "10.0.0.1:8000" || cfg.E2B.APIKey != "test-env-key" || len(cfg.E2B.SandboxProxyDomains) != 1 || cfg.E2B.SandboxProxyDomains[0] != "sandboxes.example.com" {
		t.Fatal("incorrect E2B config")
	}
}

func TestRemovedE2BOptionsAreRejected(t *testing.T) {
	t.Setenv("AENV_API_KEY", "test-env-key")
	for section, fields := range map[string][]string{
		"e2b":     {"api_key", "create_timeout", "envd_timeout", "default_user", "default_workdir", "max_sandboxes", "max_cpus", "max_memory_mb"},
		"cluster": {"heartbeat_interval", "heartbeat_timeout"},
	} {
		for _, field := range fields {
			t.Run(section+"."+field, func(t *testing.T) {
				_, err := loadE2BYAML(t, section+":\n  "+field+": 1\n")
				if err == nil || !strings.Contains(err.Error(), field) {
					t.Fatalf("removed option accepted: %v", err)
				}
			})
		}
	}
}

func TestInvalidE2BConfig(t *testing.T) {
	t.Setenv("AENV_API_KEY", "test-env-key")
	cases := []struct {
		name, yaml, want string
	}{

		{"zero listener port", "e2b:\n  listen_addr: ':0'\n", "listen_addr"},
		{"large listener port", "e2b:\n  listen_addr: ':65536'\n", "listen_addr"},
		{"listener url", "e2b:\n  listen_addr: 'http://localhost:8000'\n", "listen_addr"},
		{"domain port", "e2b:\n  sandbox_proxy_domains: ['example.com:8080']\n", "sandbox_proxy_domains"},
		{"domain url", "e2b:\n  sandbox_proxy_domains: ['https://example.com']\n", "sandbox_proxy_domains"},
		{"domain label", "e2b:\n  sandbox_proxy_domains: ['-invalid.example.com']\n", "sandbox_proxy_domains"},
		{"missing Node API", "cluster:\n  scheduler_addr: 'localhost:9090'\n  node_id: a\n  cluster_id: c\n", "requires e2b.listen_addr"},
		{"missing identity", "e2b:\n  listen_addr: ':8000'\ncluster:\n  scheduler_addr: 'localhost:9090'\n", "cluster.node_id"},
		{"TLS scheduler", "cluster:\n  scheduler_addr: 'https://localhost:9090'\n", "scheduler_addr"},
		{"missing scheduler host", "cluster:\n  scheduler_addr: ':9090'\n", "scheduler_addr"},
		{"scheduler path", "cluster:\n  scheduler_addr: 'http://localhost:9090/path'\n", "scheduler_addr"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := loadE2BYAML(t, tc.yaml); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("LoadConfig() error = %v, want error mentioning %q", err, tc.want)
			}
		})
	}
}

func TestE2BKeyValidationDoesNotExposeSecret(t *testing.T) {
	t.Setenv("AENV_API_KEY", "test-secret\nbroken-key")
	_, err := loadE2BYAML(t, "e2b:\n  listen_addr: ':8000'\n")
	if err == nil || strings.Contains(err.Error(), "test-secret") || strings.Contains(err.Error(), "broken-key") {
		t.Fatalf("key validation error missing or contains secret: %v", err)
	}
}

func TestEnabledE2BRequiresEnvironmentKey(t *testing.T) {
	t.Setenv("AENV_API_KEY", "")
	if _, err := loadE2BYAML(t, "e2b:\n  listen_addr: ':8000'\n"); err == nil || !strings.Contains(err.Error(), "AENV_API_KEY") {
		t.Fatalf("missing env key: %v", err)
	}
}
