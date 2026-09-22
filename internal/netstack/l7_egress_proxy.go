// Conch generates Envoy's bootstrap and manages the proxy as a child process.
// Startup waits for readiness; shutdown stops the process and bounds the wait.
// Request authorization and sandbox policy ownership live in l7_egress_security.go.
package netstack

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"syscall"
	"text/template"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/openeuler/Conch/pkg/ulog"
)

const (
	egressProxyLogLevel     = "info"
	egressProxyStartTimeout = 15 * time.Second
	egressProxyStopTimeout  = 5 * time.Second
)

type egressProxyProcess struct {
	cmd      *exec.Cmd
	done     chan error
	stopping atomic.Bool
}

func startEgressProxy(cfg EgressSecurityConfig) (*egressProxyProcess, error) {
	info, err := os.Stat(cfg.ProxyBinary)
	if err != nil {
		return nil, fmt.Errorf("inspect Envoy binary: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return nil, fmt.Errorf("Envoy binary %s is not executable", cfg.ProxyBinary)
	}
	if err := os.MkdirAll(filepath.Dir(cfg.AdminSocket), 0o700); err != nil {
		return nil, fmt.Errorf("create Envoy admin socket directory: %w", err)
	}
	if err := os.Remove(cfg.AdminSocket); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("remove stale Envoy admin socket: %w", err)
	}
	cmd := exec.Command(cfg.ProxyBinary, "-c", cfg.Bootstrap, "--log-level", egressProxyLogLevel)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
	if err := cmd.Start(); err != nil {
		_ = os.Remove(cfg.Bootstrap)
		return nil, fmt.Errorf("start Envoy egress proxy: %w", err)
	}
	process := &egressProxyProcess{cmd: cmd, done: make(chan error, 1)}
	go func() {
		err := cmd.Wait()
		if !process.stopping.Load() {
			ulog.GetLogger().Error("Envoy egress proxy exited unexpectedly", ulog.F("error", err))
		}
		process.done <- err
	}()
	return process, nil
}

func writeEnvoyBootstrap(cfg *EgressSecurityConfig) error {
	// Match conchd's installed-config-first, local-development fallback order.
	var source []byte
	var sourcePath string
	for _, path := range []string{"/etc/conch/envoy/egress.yaml", "config/envoy/egress.yaml"} {
		contents, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read Envoy bootstrap template %s: %w", path, err)
		}
		source, sourcePath = contents, path
		break
	}
	if sourcePath == "" {
		return fmt.Errorf("Envoy bootstrap template not found: install /etc/conch/envoy/egress.yaml or provide config/envoy/egress.yaml: %w", os.ErrNotExist)
	}
	tmpl, err := template.New("envoy-egress").Option("missingkey=error").Funcs(template.FuncMap{"quote": strconv.Quote}).Parse(string(source))
	if err != nil {
		return fmt.Errorf("parse Envoy bootstrap template %s: %w", sourcePath, err)
	}
	var rendered bytes.Buffer
	if err := tmpl.Execute(&rendered, cfg); err != nil {
		return fmt.Errorf("render Envoy bootstrap template %s: %w", sourcePath, err)
	}
	var document yaml.Node
	decoder := yaml.NewDecoder(bytes.NewReader(rendered.Bytes()))
	if err := decoder.Decode(&document); err != nil {
		return fmt.Errorf("parse rendered Envoy bootstrap: %w", err)
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); err != io.EOF {
		return fmt.Errorf("Envoy bootstrap must contain exactly one YAML document")
	}
	resolved := *cfg
	if err := resolveEnvoySettings(&document, &resolved); err != nil {
		return fmt.Errorf("resolve Envoy bootstrap settings: %w", err)
	}
	sourceAbs, err := filepath.Abs(sourcePath)
	if err != nil {
		return fmt.Errorf("resolve Envoy template path: %w", err)
	}
	// Startup removes stale sockets and replaces the runtime bootstrap. Never
	// let those paths overwrite the source configuration or TLS material.
	paths := []string{resolved.AdminSocket, resolved.AuthzSocket, resolved.Bootstrap}
	for i, path := range paths {
		for _, protected := range append(paths[i+1:], sourceAbs, resolved.TLSCertFile, resolved.TLSKeyFile, resolved.GuestCAFile) {
			if filepath.Clean(path) == filepath.Clean(protected) {
				return fmt.Errorf("Envoy runtime path %s conflicts with another configuration or TLS file", path)
			}
		}
	}
	bootstrap, err := yaml.Marshal(&document)
	if err != nil {
		return fmt.Errorf("encode Envoy bootstrap: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(cfg.Bootstrap), 0o700); err != nil {
		return fmt.Errorf("create Envoy bootstrap directory: %w", err)
	}
	if err := os.WriteFile(cfg.Bootstrap, bootstrap, 0o600); err != nil {
		return fmt.Errorf("write Envoy bootstrap: %w", err)
	}
	*cfg = resolved
	return nil
}

// Read the shared settings from the actual Envoy configuration. Missing scalar
// values receive defaults; explicit values also become Conch's runtime settings.
func resolveEnvoySettings(document *yaml.Node, cfg *EgressSecurityConfig) error {
	if document.Kind != yaml.DocumentNode || len(document.Content) != 1 {
		return fmt.Errorf("expected one bootstrap YAML document")
	}
	root := document.Content[0]
	listener, err := namedBootstrapEntry(root, "listeners", "conch_egress")
	if err != nil {
		return err
	}
	cluster, err := namedBootstrapEntry(root, "clusters", "conch_authz")
	if err != nil {
		return err
	}
	httpsCluster, err := namedBootstrapEntry(root, "clusters", "conch_https_upstream")
	if err != nil {
		return err
	}
	port, err := bootstrapField(listener, "address", "socket_address", "port_value")
	if err != nil {
		return err
	}
	if port.Tag == "!!null" {
		if err := port.Encode(cfg.ProxyPort); err != nil {
			return err
		}
	}
	if port.Tag != "!!int" {
		return fmt.Errorf("conch_egress port_value must be an integer")
	}
	if err := port.Decode(&cfg.ProxyPort); err != nil || cfg.ProxyPort < 1 || cfg.ProxyPort > 65535 {
		return fmt.Errorf("conch_egress port_value must be an integer from 1 to 65535")
	}
	chains, err := bootstrapField(listener, "filter_chains")
	if err != nil {
		return err
	}
	var certificate *yaml.Node
	for _, chain := range chains.Content {
		var match struct {
			TransportSocket struct {
				Name string `yaml:"name"`
			} `yaml:"transport_socket"`
		}
		if err := chain.Decode(&match); err != nil {
			return err
		}
		if match.TransportSocket.Name != "envoy.transport_sockets.tls" {
			continue
		}
		if certificate != nil {
			return fmt.Errorf("conch_egress requires a single TLS certificate configuration")
		}
		certificates, err := bootstrapField(chain, "transport_socket", "typed_config", "common_tls_context", "tls_certificates")
		if err != nil {
			return err
		}
		if certificates.Tag == "!!null" {
			*certificates = yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}
		}
		if certificates.Kind != yaml.SequenceNode || len(certificates.Content) != 1 {
			return fmt.Errorf("conch_egress requires one file-based TLS certificate and key")
		}
		certificate = certificates.Content[0]
	}
	if certificate == nil {
		return fmt.Errorf("conch_egress TLS filter chain is missing")
	}
	endpoints, err := bootstrapField(cluster, "load_assignment", "endpoints")
	if err != nil {
		return err
	}
	if endpoints.Kind != yaml.SequenceNode || len(endpoints.Content) != 1 {
		return fmt.Errorf("conch_authz requires one endpoint group")
	}
	backends, err := bootstrapField(endpoints.Content[0], "lb_endpoints")
	if err != nil {
		return err
	}
	if backends.Kind != yaml.SequenceNode || len(backends.Content) != 1 {
		return fmt.Errorf("conch_authz requires one Unix socket endpoint")
	}
	for _, setting := range []struct {
		root *yaml.Node
		path []string
		dest *string
	}{
		{root, []string{"admin", "address", "pipe", "path"}, &cfg.AdminSocket},
		{backends.Content[0], []string{"endpoint", "address", "pipe", "path"}, &cfg.AuthzSocket},
		{certificate, []string{"certificate_chain", "filename"}, &cfg.TLSCertFile},
		{certificate, []string{"private_key", "filename"}, &cfg.TLSKeyFile},
		{httpsCluster, []string{"transport_socket", "typed_config", "common_tls_context", "validation_context", "trusted_ca", "filename"}, &cfg.UpstreamCA},
	} {
		field, err := bootstrapField(setting.root, setting.path...)
		if err != nil {
			return err
		}
		if field.Tag == "!!null" {
			if err := field.Encode(*setting.dest); err != nil {
				return err
			}
		}
		if err := field.Decode(setting.dest); err != nil || !filepath.IsAbs(*setting.dest) {
			return fmt.Errorf("%v must be an absolute file path", setting.path)
		}
	}
	return nil
}

func namedBootstrapEntry(root *yaml.Node, collection, name string) (*yaml.Node, error) {
	entries, err := bootstrapField(root, "static_resources", collection)
	if err != nil {
		return nil, err
	}
	if entries.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("static_resources.%s must be a sequence", collection)
	}
	var found *yaml.Node
	for _, entry := range entries.Content {
		var value struct {
			Name string `yaml:"name"`
		}
		if err := entry.Decode(&value); err != nil {
			return nil, err
		}
		if value.Name == name {
			if found != nil {
				return nil, fmt.Errorf("duplicate Envoy %s %q", collection, name)
			}
			found = entry
		}
	}
	if found == nil {
		return nil, fmt.Errorf("required Envoy %s %q is missing", collection, name)
	}
	return found, nil
}

// Return a mapping field, creating omitted fields without touching other options.
func bootstrapField(node *yaml.Node, path ...string) (*yaml.Node, error) {
	for _, key := range path {
		if node.Tag == "!!null" {
			*node = yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		}
		if node.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("expected a mapping before %q", key)
		}
		var next *yaml.Node
		for i := 0; i < len(node.Content); i += 2 {
			if node.Content[i].Value == key {
				if next != nil {
					return nil, fmt.Errorf("duplicate bootstrap field %q", key)
				}
				next = node.Content[i+1]
			}
		}
		if next == nil {
			next = &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null"}
			node.Content = append(node.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, next)
		}
		node = next
	}
	return node, nil
}

func (p *egressProxyProcess) Close() error {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return nil
	}
	p.stopping.Store(true)
	select {
	case <-p.done:
		return nil
	default:
	}
	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		if errors.Is(err, os.ErrProcessDone) {
			return nil
		}
		return fmt.Errorf("stop Envoy egress proxy: %w", err)
	}
	timer := time.NewTimer(egressProxyStopTimeout)
	defer timer.Stop()
	select {
	case <-p.done:
		return nil
	case <-timer.C:
		if err := p.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return fmt.Errorf("kill Envoy egress proxy: %w", err)
		}
		select {
		case <-p.done:
			return nil
		case <-time.After(time.Second):
			return fmt.Errorf("Envoy egress proxy did not exit after SIGKILL")
		}
	}
}

func (c *egressSecurityController) waitProxyReady(parent context.Context) error {
	ctx, cancel := context.WithTimeout(parent, egressProxyStartTimeout)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var lastErr error
	for {
		if err := c.checkProxyReady(ctx); err == nil {
			return nil
		} else {
			lastErr = err
		}
		select {
		case err := <-c.proxy.done:
			return fmt.Errorf("Envoy egress proxy exited before becoming ready: %w", err)
		case <-ctx.Done():
			return fmt.Errorf("wait for Envoy egress proxy readiness: %w: %v", ctx.Err(), lastErr)
		case <-ticker.C:
		}
	}
}
