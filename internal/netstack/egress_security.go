package netstack

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/asm"
	"github.com/coreos/go-iptables/iptables"
	authv3 "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	statuspb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"

	"github.com/openeuler/Conch/internal/runtimeapi"
	"github.com/openeuler/Conch/pkg/ulog"
)

const (
	defaultEgressProxyPort   = 15001
	defaultEgressAuthzSocket = "/run/conch/egress-authz.sock"
	defaultEgressAdminSocket = "/run/conch/envoy-admin.sock"
	defaultEgressTLSCertFile = "/etc/conch/egress-tls/envoy_tls.crt"
	defaultEgressTLSKeyFile  = "/etc/conch/egress-tls/envoy_tls.key"
	defaultEgressGuestCAFile = "/etc/conch/egress-tls/ca.crt"
	defaultEgressUpstreamCA  = "/etc/pki/tls/certs/ca-bundle.crt"
	defaultEgressProxyBinary = "/usr/bin/envoy"
	defaultEgressBootstrap   = "/run/conch/envoy-egress.yaml"
	egressTrafficMark        = 0xc011
	egressMangleChain        = "CONCH-EGRESS-L7"
	egressBPFName            = "conch_egress_mark"
	conchAgentPort           = 4064
	maxEgressRules           = 256
	maxGuestCAPEMSize        = 8 * 1024
)

type EgressSecurityConfig struct {
	Enabled     bool   `yaml:"enabled"`
	ProxyPort   int    `yaml:"proxy_port"`
	AuthzSocket string `yaml:"authz_socket"`
	AdminSocket string `yaml:"admin_socket"`
	TLSCertFile string `yaml:"tls_cert_file"`
	TLSKeyFile  string `yaml:"tls_key_file"`
	GuestCAFile string `yaml:"guest_ca_file"`
	UpstreamCA  string `yaml:"upstream_ca_file"`
	ProxyBinary string `yaml:"proxy_binary"`
	Bootstrap   string `yaml:"bootstrap_path"`
}

type egressSecurityController struct {
	authv3.UnimplementedAuthorizationServer

	cfg        EgressSecurityConfig
	bridgeName string
	program    *ebpf.Program
	server     *grpc.Server
	listener   net.Listener
	proxy      *egressProxyProcess
	guestCAPEM string

	mu       sync.RWMutex
	policies map[string]compiledEgressPolicy
	started  bool
}

type compiledEgressPolicy struct {
	sandboxID string
	rules     []runtimeapi.SandboxEgressRule
}

func newEgressSecurityController(cfg EgressSecurityConfig, bridgeName string) (*egressSecurityController, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	if cfg.ProxyPort == 0 {
		cfg.ProxyPort = defaultEgressProxyPort
	}
	if cfg.ProxyPort < 1 || cfg.ProxyPort > 65535 {
		return nil, fmt.Errorf("invalid network.egress_security.proxy_port=%d", cfg.ProxyPort)
	}
	if cfg.AuthzSocket == "" {
		cfg.AuthzSocket = defaultEgressAuthzSocket
	}
	if cfg.AdminSocket == "" {
		cfg.AdminSocket = defaultEgressAdminSocket
	}
	if cfg.TLSCertFile == "" {
		cfg.TLSCertFile = defaultEgressTLSCertFile
	}
	if cfg.TLSKeyFile == "" {
		cfg.TLSKeyFile = defaultEgressTLSKeyFile
	}
	if cfg.GuestCAFile == "" {
		cfg.GuestCAFile = defaultEgressGuestCAFile
	}
	if cfg.UpstreamCA == "" {
		cfg.UpstreamCA = defaultEgressUpstreamCA
	}
	if cfg.ProxyBinary == "" {
		cfg.ProxyBinary = defaultEgressProxyBinary
	}
	if cfg.Bootstrap == "" {
		cfg.Bootstrap = defaultEgressBootstrap
	}
	if !filepath.IsAbs(cfg.AuthzSocket) || !filepath.IsAbs(cfg.AdminSocket) ||
		!filepath.IsAbs(cfg.TLSCertFile) || !filepath.IsAbs(cfg.TLSKeyFile) || !filepath.IsAbs(cfg.GuestCAFile) ||
		!filepath.IsAbs(cfg.UpstreamCA) || !filepath.IsAbs(cfg.ProxyBinary) || !filepath.IsAbs(cfg.Bootstrap) {
		return nil, fmt.Errorf("network egress security paths must be absolute")
	}
	return &egressSecurityController{cfg: cfg, bridgeName: bridgeName, policies: make(map[string]compiledEgressPolicy)}, nil
}

func (c *egressSecurityController) Start() error {
	if c == nil || c.started {
		return nil
	}
	_, _, guestCAPEM, err := c.loadTLSMaterial()
	if err != nil {
		return err
	}
	c.guestCAPEM = string(guestCAPEM)
	program, err := ebpf.NewProgram(&ebpf.ProgramSpec{
		Name:    egressBPFName,
		Type:    ebpf.SchedCLS,
		License: "Dual MIT/GPL",
		Instructions: asm.Instructions{
			asm.Mov.Imm(asm.R2, egressTrafficMark),
			asm.StoreMem(asm.R1, 8, asm.R2, asm.Word),
			asm.Mov.Imm(asm.R0, int32(netlink.TC_ACT_OK)),
			asm.Return(),
		},
	})
	if err != nil {
		return fmt.Errorf("load eBPF traffic marker: %w", err)
	}
	c.program = program

	if err := c.startAuthorizationServer(); err != nil {
		_ = program.Close()
		c.program = nil
		return err
	}
	proxy, err := startEgressProxy(c.cfg)
	if err != nil {
		_ = c.closeAuthorizationServer()
		_ = program.Close()
		c.program = nil
		return err
	}
	c.proxy = proxy
	if err := c.waitProxyReady(context.Background()); err != nil {
		_ = proxy.Close()
		c.proxy = nil
		_ = os.Remove(c.cfg.Bootstrap)
		_ = c.closeAuthorizationServer()
		_ = program.Close()
		c.program = nil
		return err
	}
	if err := c.configureTransparentProxy(); err != nil {
		_ = c.removeTransparentProxy()
		_ = proxy.Close()
		c.proxy = nil
		_ = os.Remove(c.cfg.Bootstrap)
		_ = c.closeAuthorizationServer()
		_ = program.Close()
		c.program = nil
		return err
	}
	c.started = true
	return nil
}

func (c *egressSecurityController) Close() error {
	if c == nil || !c.started {
		return nil
	}
	c.started = false
	var errs []error
	errs = append(errs, c.removeTransparentProxy())
	if c.proxy != nil {
		errs = append(errs, c.proxy.Close())
		c.proxy = nil
	}
	errs = append(errs, c.closeAuthorizationServer())
	errs = append(errs, os.Remove(c.cfg.Bootstrap))
	if c.program != nil {
		errs = append(errs, c.program.Close())
		c.program = nil
	}
	return joinNonMissingErrors(errs...)
}

func (c *egressSecurityController) closeAuthorizationServer() error {
	if c.server != nil {
		c.server.Stop()
		c.server = nil
	}
	var errs []error
	if c.listener != nil {
		errs = append(errs, c.listener.Close())
		c.listener = nil
	}
	errs = append(errs, os.Remove(c.cfg.AuthzSocket))
	return joinNonMissingErrors(errs...)
}

func (c *egressSecurityController) Apply(ctx context.Context, slot *Slot, sandboxID string, cfg *SandboxNetworkConfig) error {
	if c == nil || !c.started || c.program == nil {
		return fmt.Errorf("egress security controller is not running")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if slot == nil || canonicalIP(slot.CNIIP()) == "" {
		return fmt.Errorf("sandbox network slot has no valid CNI IP")
	}
	if err := c.checkProxyReady(ctx); err != nil {
		return err
	}
	if err := c.validateHTTPSMaterial(cfg.Rules); err != nil {
		return err
	}
	if err := c.attachMarker(ctx, slot); err != nil {
		return err
	}
	c.mu.Lock()
	c.policies[canonicalIP(slot.CNIIP())] = compiledEgressPolicy{sandboxID: sandboxID, rules: cloneEgressRules(cfg.Rules)}
	c.mu.Unlock()
	return nil
}

func (c *egressSecurityController) Clear(ctx context.Context, slot *Slot) error {
	if c == nil || slot == nil {
		return nil
	}
	c.mu.Lock()
	key := canonicalIP(slot.CNIIP())
	_, attached := c.policies[key]
	delete(c.policies, key)
	c.mu.Unlock()
	if !attached || c.program == nil {
		return nil
	}
	return c.detachMarker(ctx, slot)
}

func validateL7EgressPolicy(cfg *SandboxNetworkConfig) error {
	if cfg == nil {
		return nil
	}
	if len(cfg.Rules) > maxEgressRules {
		return fmt.Errorf("%w: at most %d L7 egress rules are supported", ErrInvalidPolicy, maxEgressRules)
	}
	if len(cfg.Rules) != 0 && (cfg.EgressProxy == nil || !cfg.EgressProxy.Enabled) {
		return fmt.Errorf("%w: egress_proxy.enabled is required when rules are configured", ErrInvalidPolicy)
	}
	for i, rule := range cfg.Rules {
		action := strings.ToLower(strings.TrimSpace(rule.Action))
		if action != "allow" && action != "deny" {
			return fmt.Errorf("%w: rules[%d].action must be allow or deny", ErrInvalidPolicy, i)
		}
		protocol := strings.ToLower(strings.TrimSpace(rule.Protocol))
		if protocol != "" && protocol != "http" && protocol != "https" {
			return fmt.Errorf("%w: rules[%d].protocol must be http or https", ErrInvalidPolicy, i)
		}
		if !validPolicyHost(rule.Host) {
			return fmt.Errorf("%w: rules[%d].host is invalid", ErrInvalidPolicy, i)
		}
		if rule.PathPrefix != "" && !strings.HasPrefix(rule.PathPrefix, "/") {
			return fmt.Errorf("%w: rules[%d].path_prefix must start with /", ErrInvalidPolicy, i)
		}
	}
	return nil
}

func hasL7EgressPolicy(cfg *SandboxNetworkConfig) bool {
	return cfg != nil && (len(cfg.Rules) != 0 || (cfg.EgressProxy != nil && cfg.EgressProxy.Enabled))
}

func validPolicyHost(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	if strings.HasPrefix(host, "*.") {
		host = strings.TrimPrefix(host, "*.")
	}
	return host != "" && !strings.ContainsAny(host, "/:@ \\") && strings.Contains(host, ".")
}

func (c *egressSecurityController) startAuthorizationServer() error {
	if err := os.MkdirAll(filepath.Dir(c.cfg.AuthzSocket), 0o700); err != nil {
		return fmt.Errorf("create egress authorization socket directory: %w", err)
	}
	if err := os.Remove(c.cfg.AuthzSocket); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove stale egress authorization socket: %w", err)
	}
	listener, err := net.Listen("unix", c.cfg.AuthzSocket)
	if err != nil {
		return fmt.Errorf("listen on egress authorization socket: %w", err)
	}
	if err := os.Chmod(c.cfg.AuthzSocket, 0o600); err != nil {
		_ = listener.Close()
		return fmt.Errorf("secure egress authorization socket: %w", err)
	}
	c.listener = listener
	server := grpc.NewServer()
	c.server = server
	authv3.RegisterAuthorizationServer(server, c)
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, net.ErrClosed) {
			ulog.GetLogger().Error("egress authorization server stopped", ulog.F("error", err))
		}
	}()
	return nil
}

func (c *egressSecurityController) Check(_ context.Context, request *authv3.CheckRequest) (*authv3.CheckResponse, error) {
	attributes := request.GetAttributes()
	httpRequest := attributes.GetRequest().GetHttp()
	sourceIP := canonicalIP(attributes.GetSource().GetAddress().GetSocketAddress().GetAddress())
	c.mu.RLock()
	policy, ok := c.policies[sourceIP]
	c.mu.RUnlock()
	if !ok {
		c.audit("", httpRequest, "deny", "unknown sandbox source")
		return deniedEgressResponse(typev3.StatusCode_Forbidden, codes.PermissionDenied, "egress denied"), nil
	}
	rule, ok := matchingEgressRule(policy.rules, httpRequest)
	if !ok || strings.EqualFold(rule.Action, "deny") {
		c.audit(policy.sandboxID, httpRequest, "deny", "policy")
		return deniedEgressResponse(typev3.StatusCode_Forbidden, codes.PermissionDenied, "egress denied"), nil
	}
	c.audit(policy.sandboxID, httpRequest, "allow", "policy")
	return &authv3.CheckResponse{
		Status: &statuspb.Status{Code: int32(codes.OK)},
		HttpResponse: &authv3.CheckResponse_OkResponse{
			OkResponse: &authv3.OkHttpResponse{},
		},
	}, nil
}

func matchingEgressRule(rules []runtimeapi.SandboxEgressRule, request *authv3.AttributeContext_HttpRequest) (runtimeapi.SandboxEgressRule, bool) {
	host, port := requestHostPort(request)
	protocol := strings.ToLower(strings.TrimSpace(request.GetScheme()))
	var allowed *runtimeapi.SandboxEgressRule
	for i := range rules {
		rule := &rules[i]
		if !hostMatches(rule.Host, host) || (rule.Port != 0 && rule.Port != port) ||
			(rule.Protocol != "" && !strings.EqualFold(rule.Protocol, protocol)) ||
			(rule.Method != "" && !strings.EqualFold(rule.Method, request.GetMethod())) ||
			(rule.PathPrefix != "" && !strings.HasPrefix(request.GetPath(), rule.PathPrefix)) {
			continue
		}
		if strings.EqualFold(rule.Action, "deny") {
			return *rule, true
		}
		if allowed == nil {
			allowed = rule
		}
	}
	if allowed == nil {
		return runtimeapi.SandboxEgressRule{}, false
	}
	return *allowed, true
}

func requestHostPort(request *authv3.AttributeContext_HttpRequest) (string, uint16) {
	host := strings.TrimSpace(request.GetHost())
	name, portText, err := net.SplitHostPort(host)
	if err == nil {
		port, _ := strconv.ParseUint(portText, 10, 16)
		return strings.ToLower(strings.TrimSuffix(name, ".")), uint16(port)
	}
	protocol := strings.ToLower(strings.TrimSpace(request.GetScheme()))
	if protocol == "https" {
		return strings.ToLower(strings.TrimSuffix(host, ".")), 443
	}
	return strings.ToLower(strings.TrimSuffix(host, ".")), 80
}

func hostMatches(pattern, host string) bool {
	pattern = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(pattern), "."))
	if strings.HasPrefix(pattern, "*.") {
		suffix := strings.TrimPrefix(pattern, "*")
		return strings.HasSuffix(host, suffix) && host != strings.TrimPrefix(suffix, ".")
	}
	return host == pattern
}

func (c *egressSecurityController) validateHTTPSMaterial(rules []runtimeapi.SandboxEgressRule) error {
	hosts := make(map[string]struct{})
	for _, rule := range rules {
		if !strings.EqualFold(strings.TrimSpace(rule.Protocol), "http") {
			hosts[strings.ToLower(strings.TrimSpace(rule.Host))] = struct{}{}
		}
	}
	if len(hosts) == 0 {
		return nil
	}
	certificate, intermediates, guestCAPEM, err := c.loadTLSMaterial()
	if err != nil {
		return err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(guestCAPEM) {
		return fmt.Errorf("egress guest CA contains no certificates")
	}
	for host := range hosts {
		verifyHost := host
		if strings.HasPrefix(host, "*.") {
			verifyHost = "conch-check." + strings.TrimPrefix(host, "*.")
		}
		if _, err := certificate.Verify(x509.VerifyOptions{DNSName: verifyHost, Roots: roots, Intermediates: intermediates}); err != nil {
			return fmt.Errorf("egress TLS certificate does not cover policy host %q: %w", host, err)
		}
	}
	return nil
}

func (c *egressSecurityController) loadTLSMaterial() (*x509.Certificate, *x509.CertPool, []byte, error) {
	keyInfo, err := os.Stat(c.cfg.TLSKeyFile)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("inspect egress TLS key: %w", err)
	}
	if !keyInfo.Mode().IsRegular() || keyInfo.Mode().Perm()&0o077 != 0 {
		return nil, nil, nil, fmt.Errorf("egress TLS key must be a regular owner-only file")
	}
	pair, err := tls.LoadX509KeyPair(c.cfg.TLSCertFile, c.cfg.TLSKeyFile)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("load egress TLS certificate: %w", err)
	}
	if len(pair.Certificate) == 0 {
		return nil, nil, nil, fmt.Errorf("egress TLS certificate is empty")
	}
	certificate, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, nil, nil, fmt.Errorf("parse egress TLS certificate: %w", err)
	}
	intermediates := x509.NewCertPool()
	for _, raw := range pair.Certificate[1:] {
		parsed, err := x509.ParseCertificate(raw)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("parse egress TLS intermediate: %w", err)
		}
		intermediates.AddCert(parsed)
	}
	guestCAPEM, err := os.ReadFile(c.cfg.GuestCAFile)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("read egress guest CA: %w", err)
	}
	if len(guestCAPEM) == 0 || len(guestCAPEM) > maxGuestCAPEMSize {
		return nil, nil, nil, fmt.Errorf("egress guest CA must be between 1 and %d bytes", maxGuestCAPEMSize)
	}
	guestRoots := x509.NewCertPool()
	if !guestRoots.AppendCertsFromPEM(guestCAPEM) {
		return nil, nil, nil, fmt.Errorf("egress guest CA contains no certificates")
	}
	caPEM, err := os.ReadFile(c.cfg.UpstreamCA)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("read egress upstream CA bundle: %w", err)
	}
	if ok := x509.NewCertPool().AppendCertsFromPEM(caPEM); !ok {
		return nil, nil, nil, fmt.Errorf("egress upstream CA bundle contains no certificates")
	}
	return certificate, intermediates, guestCAPEM, nil
}

func (c *egressSecurityController) audit(sandboxID string, request *authv3.AttributeContext_HttpRequest, decision, reason string) {
	host, port := requestHostPort(request)
	ulog.GetLogger().Info("sandbox egress request",
		ulog.F("sandbox_id", sandboxID),
		ulog.F("host", host),
		ulog.F("port", port),
		ulog.F("method", request.GetMethod()),
		ulog.F("decision", decision),
		ulog.F("reason", reason),
	)
}

func deniedEgressResponse(httpCode typev3.StatusCode, grpcCode codes.Code, message string) *authv3.CheckResponse {
	return &authv3.CheckResponse{
		Status: &statuspb.Status{Code: int32(grpcCode), Message: message},
		HttpResponse: &authv3.CheckResponse_DeniedResponse{
			DeniedResponse: &authv3.DeniedHttpResponse{
				Status: &typev3.HttpStatus{Code: httpCode},
				Body:   message,
			},
		},
	}
}

func (c *egressSecurityController) checkProxyReady(ctx context.Context) error {
	dialer := &net.Dialer{Timeout: 2 * time.Second}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", c.cfg.AdminSocket)
		},
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://envoy/ready", nil)
	if err != nil {
		return fmt.Errorf("construct Envoy readiness request: %w", err)
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("egress proxy is unavailable: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("egress proxy is not ready: %s", response.Status)
	}
	return nil
}

func (c *egressSecurityController) attachMarker(ctx context.Context, slot *Slot) error {
	link, err := hostVethForSlot(ctx, slot)
	if err != nil {
		return err
	}
	attrs := netlink.QdiscAttrs{
		LinkIndex: link.Attrs().Index,
		Handle:    netlink.MakeHandle(0xffff, 0),
		Parent:    netlink.HANDLE_CLSACT,
	}
	if err := netlink.QdiscReplace(&netlink.Clsact{QdiscAttrs: attrs}); err != nil {
		return fmt.Errorf("install clsact qdisc: %w", err)
	}
	filter := &netlink.BpfFilter{
		FilterAttrs:  markerFilterAttrs(link.Attrs().Index),
		Fd:           c.program.FD(),
		Name:         egressBPFName,
		DirectAction: true,
	}
	if err := netlink.FilterReplace(filter); err != nil {
		return fmt.Errorf("attach eBPF traffic marker: %w", err)
	}
	return nil
}

func (c *egressSecurityController) detachMarker(ctx context.Context, slot *Slot) error {
	link, err := hostVethForSlot(ctx, slot)
	if err != nil {
		return err
	}
	filter := &netlink.BpfFilter{FilterAttrs: markerFilterAttrs(link.Attrs().Index), Fd: -1}
	if err := netlink.FilterDel(filter); err != nil && !errors.Is(err, unix.ENOENT) && !errors.Is(err, unix.EINVAL) {
		return fmt.Errorf("detach eBPF traffic marker: %w", err)
	}
	return nil
}

func hostVethForSlot(ctx context.Context, slot *Slot) (netlink.Link, error) {
	peerIndex := 0
	if err := runInNetNSPath(ctx, slot.NetNSPath(), func() error {
		link, err := netlink.LinkByName(cniOuterInterfaceName)
		if err != nil {
			return fmt.Errorf("find sandbox CNI interface: %w", err)
		}
		peerIndex = link.Attrs().ParentIndex
		if peerIndex == 0 {
			return fmt.Errorf("sandbox CNI interface has no host peer")
		}
		return nil
	}); err != nil {
		return nil, err
	}
	link, err := netlink.LinkByIndex(peerIndex)
	if err != nil {
		return nil, fmt.Errorf("find host-side sandbox veth: %w", err)
	}
	return link, nil
}

func markerFilterAttrs(linkIndex int) netlink.FilterAttrs {
	return netlink.FilterAttrs{
		LinkIndex: linkIndex,
		Parent:    netlink.HANDLE_MIN_INGRESS,
		Handle:    netlink.MakeHandle(0, 1),
		Priority:  1,
		Protocol:  unix.ETH_P_ALL,
	}
}

func (c *egressSecurityController) configureTransparentProxy() error {
	tables, err := iptables.New(iptables.Timeout(int(sandboxPolicyLockTimeout / time.Second)))
	if err != nil {
		return fmt.Errorf("initialize egress proxy iptables: %w", err)
	}
	exists, err := tables.ChainExists("mangle", egressMangleChain)
	if err != nil {
		return err
	}
	if !exists {
		if err := tables.NewChain("mangle", egressMangleChain); err != nil {
			return err
		}
	}
	if err := tables.ClearChain("mangle", egressMangleChain); err != nil {
		return err
	}
	mark := fmt.Sprintf("0x%x/0xffffffff", egressTrafficMark)
	agentReply := []string{
		"-p", "tcp", "--sport", strconv.Itoa(conchAgentPort),
		"-m", "conntrack", "--ctstate", "ESTABLISHED", "--ctdir", "REPLY",
	}
	if err := tables.Append("mangle", egressMangleChain, append(agentReply, "-j", "MARK", "--set-mark", "0")...); err != nil {
		return fmt.Errorf("clear proxy mark from sandbox agent reply: %w", err)
	}
	if err := tables.Append("mangle", egressMangleChain, append(agentReply, "-j", "RETURN")...); err != nil {
		return fmt.Errorf("allow sandbox agent reply outside egress proxy: %w", err)
	}
	for _, protocol := range []string{"udp", "tcp"} {
		if err := tables.Append("mangle", egressMangleChain, "-p", protocol, "--dport", "53", "-j", "MARK", "--set-mark", "0"); err != nil {
			return fmt.Errorf("allow sandbox DNS outside egress proxy: %w", err)
		}
		if err := tables.Append("mangle", egressMangleChain, "-p", protocol, "--dport", "53", "-j", "RETURN"); err != nil {
			return fmt.Errorf("complete sandbox DNS bypass: %w", err)
		}
	}
	proxyMatch := []string{"-p", "tcp", "-m", "multiport", "--dports", "80,443"}
	if err := tables.Append("mangle", egressMangleChain, append(proxyMatch, "-j", "RETURN")...); err != nil {
		return fmt.Errorf("allow proxyable sandbox traffic: %w", err)
	}
	if err := tables.Append("mangle", egressMangleChain, "-j", "DROP"); err != nil {
		return fmt.Errorf("block traffic that bypasses egress proxy: %w", err)
	}
	hook := []string{"-i", c.bridgeName, "-m", "mark", "--mark", mark, "-j", egressMangleChain}
	present, err := tables.Exists("mangle", "PREROUTING", hook...)
	if err != nil {
		return err
	}
	if !present {
		if err := tables.Insert("mangle", "PREROUTING", 1, hook...); err != nil {
			return err
		}
	}
	redirect := []string{
		"-i", c.bridgeName, "-m", "mark", "--mark", mark,
		"-p", "tcp", "-m", "multiport", "--dports", "80,443",
		"-j", "REDIRECT", "--to-ports", strconv.Itoa(c.cfg.ProxyPort),
	}
	present, err = tables.Exists("nat", "PREROUTING", redirect...)
	if err != nil {
		return err
	}
	if !present {
		if err := tables.Insert("nat", "PREROUTING", 1, redirect...); err != nil {
			return fmt.Errorf("install egress proxy redirect: %w", err)
		}
	}
	return nil
}

func (c *egressSecurityController) removeTransparentProxy() error {
	var errs []error
	tables, err := iptables.New(iptables.Timeout(int(sandboxPolicyLockTimeout / time.Second)))
	if err == nil {
		mark := fmt.Sprintf("0x%x/0xffffffff", egressTrafficMark)
		redirect := []string{
			"-i", c.bridgeName, "-m", "mark", "--mark", mark,
			"-p", "tcp", "-m", "multiport", "--dports", "80,443",
			"-j", "REDIRECT", "--to-ports", strconv.Itoa(c.cfg.ProxyPort),
		}
		errs = append(errs, tables.DeleteIfExists("nat", "PREROUTING", redirect...))
		hook := []string{"-i", c.bridgeName, "-m", "mark", "--mark", mark, "-j", egressMangleChain}
		errs = append(errs, tables.DeleteIfExists("mangle", "PREROUTING", hook...))
		errs = append(errs, tables.ClearAndDeleteChain("mangle", egressMangleChain))
	} else {
		errs = append(errs, err)
	}
	return joinNonMissingErrors(errs...)
}

func cloneEgressRules(rules []runtimeapi.SandboxEgressRule) []runtimeapi.SandboxEgressRule {
	cloned := make([]runtimeapi.SandboxEgressRule, len(rules))
	copy(cloned, rules)
	return cloned
}

func canonicalIP(raw string) string {
	if host, _, err := net.SplitHostPort(strings.TrimSpace(raw)); err == nil {
		raw = host
	}
	if ip, _, err := net.ParseCIDR(strings.TrimSpace(raw)); err == nil {
		return ip.String()
	}
	if ip := net.ParseIP(strings.TrimSpace(raw)); ip != nil {
		return ip.String()
	}
	return ""
}

func joinNonMissingErrors(errs ...error) error {
	kept := errs[:0]
	for _, err := range errs {
		if err != nil && !os.IsNotExist(err) && !errors.Is(err, net.ErrClosed) &&
			!errors.Is(err, unix.ENOENT) && !errors.Is(err, unix.ESRCH) {
			kept = append(kept, err)
		}
	}
	return errors.Join(kept...)
}
