package netstack

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"syscall"
	"text/template"
	"time"

	"github.com/openeuler/Conch/pkg/ulog"
)

const (
	egressProxyStartTimeout = 15 * time.Second
	egressProxyStopTimeout  = 5 * time.Second
)

//go:embed envoy_egress.yaml.tmpl
var envoyBootstrapTemplate string

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
	if err := writeEnvoyBootstrap(cfg); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(cfg.AdminSocket), 0o700); err != nil {
		return nil, fmt.Errorf("create Envoy admin socket directory: %w", err)
	}
	if err := os.Remove(cfg.AdminSocket); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("remove stale Envoy admin socket: %w", err)
	}
	cmd := exec.Command(cfg.ProxyBinary, "-c", cfg.Bootstrap, "--log-level", "info")
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

func writeEnvoyBootstrap(cfg EgressSecurityConfig) error {
	if err := os.MkdirAll(filepath.Dir(cfg.Bootstrap), 0o700); err != nil {
		return fmt.Errorf("create Envoy bootstrap directory: %w", err)
	}
	tmpl, err := template.New("envoy-egress").Funcs(template.FuncMap{"quote": strconv.Quote}).Parse(envoyBootstrapTemplate)
	if err != nil {
		return fmt.Errorf("parse Envoy bootstrap template: %w", err)
	}
	var rendered bytes.Buffer
	if err := tmpl.Execute(&rendered, cfg); err != nil {
		return fmt.Errorf("render Envoy bootstrap: %w", err)
	}
	if err := os.WriteFile(cfg.Bootstrap, rendered.Bytes(), 0o600); err != nil {
		return fmt.Errorf("write Envoy bootstrap: %w", err)
	}
	return nil
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
