// Shared iptables rules send sandbox HTTP/HTTPS traffic to Envoy. This file:
// checks sandbox HTTP/HTTPS requests against their network rules,
// resolves the destination hostname and returns an allowed IP to Envoy,
// and clears policy and invalidates old connections' authorization when slot released.
package netstack

import (
	"context"
	"crypto/rand"
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
	"time"

	"github.com/coreos/go-iptables/iptables"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	authv3 "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"golang.org/x/sys/unix"
	statuspb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/openeuler/Conch/internal/runtimeapi"
	"github.com/openeuler/Conch/pkg/ulog"
)

const (
	defaultEgressProxyPort   = 15001
	defaultEgressTLSCertFile = "/etc/conch/egress-tls/envoy_tls.crt"
	defaultEgressTLSKeyFile  = "/etc/conch/egress-tls/envoy_tls.key"
	defaultEgressGuestCAFile = "/etc/conch/egress-tls/ca.crt"
	defaultEgressUpstreamCA  = "/etc/pki/tls/certs/ca-bundle.crt"
	defaultEgressProxyBinary = "/usr/bin/envoy"
	egressMangleChain        = "CONCH-EGRESS-L7"
	bridgeIPTablesSysctl     = "/proc/sys/net/bridge/bridge-nf-call-iptables"
	maxEgressRules           = 256
	maxGuestCAPEMSize        = 8 * 1024
	egressResolveTimeout     = time.Second
	egressDestinationHeader  = "x-conch-egress-destination"
	egressConnectionMetadata = "envoy.filters.network.ext_authz"
	egressAssignmentKey      = "assignment_id"
)

type EgressSecurityConfig struct {
	Enabled     bool   `yaml:"enabled"`
	ProxyPort   int    `yaml:"proxy_port"`
	ProxyBinary string `yaml:"proxy_binary"`

	// Runtime settings are supplied by Conch, not exposed as YAML options.
	WorkDir     string `yaml:"-"`
	AuthzSocket string `yaml:"-"`
	AdminSocket string `yaml:"-"`
	TLSCertFile string `yaml:"-"`
	TLSKeyFile  string `yaml:"-"`
	GuestCAFile string `yaml:"-"`
	Bootstrap   string `yaml:"-"`
	UpstreamCA  string `yaml:"-"`
}

type egressSecurityController struct {
	authv3.UnimplementedAuthorizationServer

	cfg        EgressSecurityConfig
	bridgeName string
	dnsServers []string
	server     *grpc.Server
	listener   net.Listener
	proxy      *egressProxyProcess
	guestCAPEM string
	resolver   egressResolver

	pool    *Pool
	started bool
}

type egressResolver interface {
	LookupIP(context.Context, string, string) ([]net.IP, error)
}

type compiledEgressPolicy struct {
	sandboxID    string
	assignmentID string
	rules        []runtimeapi.SandboxEgressRule
	defaultAllow bool
	allowOut     []string
	denyOut      []string
}

func newEgressSecurityController(cfg EgressSecurityConfig, bridgeName string) (*egressSecurityController, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	if !filepath.IsAbs(cfg.WorkDir) {
		return nil, fmt.Errorf("egress security requires an absolute server.work_dir")
	}
	runDir := filepath.Join(cfg.WorkDir, "egress")
	if cfg.ProxyPort == 0 {
		cfg.ProxyPort = defaultEgressProxyPort
	}
	if cfg.ProxyPort < 1 || cfg.ProxyPort > 65535 {
		return nil, fmt.Errorf("network.egress_security.proxy_port must be between 1 and 65535")
	}
	if cfg.ProxyBinary == "" {
		cfg.ProxyBinary = defaultEgressProxyBinary
	}
	if !filepath.IsAbs(cfg.ProxyBinary) {
		return nil, fmt.Errorf("network.egress_security.proxy_binary must be an absolute path")
	}
	cfg.AuthzSocket = filepath.Join(runDir, "authz.sock")
	cfg.AdminSocket = filepath.Join(runDir, "envoy-admin.sock")
	cfg.Bootstrap = filepath.Join(runDir, "envoy.yaml")
	cfg.TLSCertFile = defaultEgressTLSCertFile
	cfg.TLSKeyFile = defaultEgressTLSKeyFile
	cfg.GuestCAFile = defaultEgressGuestCAFile
	cfg.UpstreamCA = defaultEgressUpstreamCA
	return &egressSecurityController{
		cfg: cfg, bridgeName: bridgeName,
		resolver: &net.Resolver{PreferGo: true},
	}, nil
}

func (c *egressSecurityController) Start() error {
	if c == nil || c.started {
		return nil
	}
	// Bridged sandbox-to-sandbox traffic must not bypass the host rules.
	value, err := os.ReadFile(bridgeIPTablesSysctl)
	if err != nil {
		return fmt.Errorf("egress security requires br_netfilter and %s=1: %w", bridgeIPTablesSysctl, err)
	}
	if strings.TrimSpace(string(value)) != "1" {
		return fmt.Errorf("egress security requires %s=1; configure it explicitly because it affects other bridges", bridgeIPTablesSysctl)
	}
	if err := writeEnvoyBootstrap(&c.cfg); err != nil {
		return err
	}
	defer func() {
		if !c.started {
			_ = os.Remove(c.cfg.Bootstrap)
		}
	}()
	_, _, guestCAPEM, err := c.loadTLSMaterial()
	if err != nil {
		return err
	}
	c.guestCAPEM = string(guestCAPEM)
	if err := c.startAuthorizationServer(); err != nil {
		return err
	}
	proxy, err := startEgressProxy(c.cfg)
	if err != nil {
		_ = c.closeAuthorizationServer()
		return err
	}
	c.proxy = proxy
	if err := c.waitProxyReady(context.Background()); err != nil {
		_ = proxy.Close()
		c.proxy = nil
		_ = os.Remove(c.cfg.Bootstrap)
		_ = c.closeAuthorizationServer()
		return err
	}
	if err := c.configureTransparentProxy(); err != nil {
		_ = c.removeTransparentProxy()
		_ = proxy.Close()
		c.proxy = nil
		_ = os.Remove(c.cfg.Bootstrap)
		_ = c.closeAuthorizationServer()
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
	if c == nil || !c.started {
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
	if cfg == nil {
		cfg = &SandboxNetworkConfig{}
	}
	if err := validateL7EgressPolicy(cfg); err != nil {
		return err
	}
	if err := c.validateHTTPSMaterial(cfg.Rules); err != nil {
		return err
	}
	sourceIP := canonicalIP(slot.CNIIP())
	policy := &compiledEgressPolicy{
		sandboxID: sandboxID, rules: cloneEgressRules(cfg.Rules),
		assignmentID: rand.Text(),
		defaultAllow: cfg.AllowInternetAccess == nil || *cfg.AllowInternetAccess,
		allowOut:     append([]string(nil), cfg.AllowOut...),
		denyOut:      append([]string(nil), cfg.DenyOut...),
	}
	if old := slot.egressPolicy.Load(); old != nil && old.sandboxID == sandboxID {
		policy.assignmentID = old.assignmentID
	}
	// Publish an immutable snapshot before making the slot discoverable.
	slot.egressPolicy.Store(policy)
	c.pool.slotsByIP.Store(sourceIP, slot)
	return nil
}

func (c *egressSecurityController) Clear(_ context.Context, slot *Slot) error {
	if c == nil || slot == nil {
		return nil
	}
	slot.egressPolicy.Store(nil)
	sourceIP := canonicalIP(slot.CNIIP())
	c.pool.slotsByIP.CompareAndDelete(sourceIP, slot)
	return nil
}

func validateL7EgressPolicy(cfg *SandboxNetworkConfig) error {
	if cfg == nil {
		return nil
	}
	if len(cfg.Rules) > maxEgressRules {
		return fmt.Errorf("%w: at most %d L7 egress rules are supported", ErrInvalidPolicy, maxEgressRules)
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
		if !validEgressPath(rule.Path) {
			return fmt.Errorf("%w: rules[%d].path must start with / and use only whole-segment * or trailing **, without query strings or fragments", ErrInvalidPolicy, i)
		}
	}
	return nil
}

func hasL7EgressPolicy(cfg *SandboxNetworkConfig) bool {
	return cfg != nil && len(cfg.Rules) != 0
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

func (c *egressSecurityController) Check(ctx context.Context, request *authv3.CheckRequest) (*authv3.CheckResponse, error) {
	attributes := request.GetAttributes()
	httpRequest := attributes.GetRequest().GetHttp()
	sourceIP := canonicalIP(attributes.GetSource().GetAddress().GetSocketAddress().GetAddress())
	value, ok := c.pool.slotsByIP.Load(sourceIP)
	if !ok {
		c.logEgressDecision("", httpRequest, "deny", "unknown sandbox source")
		return deniedEgressResponse(typev3.StatusCode_Forbidden, codes.PermissionDenied, "egress denied"), nil
	}
	slot := value.(*Slot)
	policy := slot.egressPolicy.Load()
	if policy == nil || policy.assignmentID == "" {
		return deniedEgressResponse(typev3.StatusCode_Forbidden, codes.PermissionDenied, "egress assignment missing"), nil
	}
	if httpRequest == nil {
		// The network filter binds ownership once, before HTTP processing.
		return &authv3.CheckResponse{
			Status: &statuspb.Status{Code: int32(codes.OK)},
			DynamicMetadata: &structpb.Struct{Fields: map[string]*structpb.Value{
				egressAssignmentKey: structpb.NewStringValue(policy.assignmentID),
			}},
		}, nil
	}
	assignmentID := attributes.GetMetadataContext().GetFilterMetadata()[egressConnectionMetadata].GetFields()[egressAssignmentKey].GetStringValue()
	if assignmentID == "" || assignmentID != policy.assignmentID {
		return deniedEgressResponse(typev3.StatusCode_Forbidden, codes.PermissionDenied, "egress assignment expired"), nil
	}
	host, port := requestHostPort(httpRequest)
	scheme := strings.ToLower(strings.TrimSpace(httpRequest.GetScheme()))
	if host == "" || port == 0 || strings.ContainsAny(host, ":/@\\?# \t\r\n") ||
		(scheme != "http" && scheme != "https") {
		return deniedEgressResponse(typev3.StatusCode_BadRequest, codes.InvalidArgument, "invalid egress destination"), nil
	}
	rule, ok := matchingEgressRule(policy.rules, httpRequest)
	if ok && rule.Action == "deny" {
		c.logEgressDecision(policy.sandboxID, httpRequest, "deny", "explicit domain deny")
		return deniedEgressResponse(typev3.StatusCode_Forbidden, codes.PermissionDenied, "egress denied"), nil
	}
	resolver := c.resolver
	if resolver == nil {
		// Use host resolver configuration, including /etc/hosts, with cancellable DNS.
		resolver = &net.Resolver{PreferGo: true}
	}
	resolveCtx, cancel := context.WithTimeout(ctx, egressResolveTimeout)
	defer cancel()
	addresses, err := resolver.LookupIP(resolveCtx, "ip4", host)
	if err != nil || len(addresses) == 0 {
		c.logEgressDecision(policy.sandboxID, httpRequest, "deny", "destination resolution failed")
		return deniedEgressResponse(typev3.StatusCode_ServiceUnavailable, codes.Unavailable, "egress destination resolution failed"), nil
	}
	address := ""
	for _, ip := range addresses {
		if ip.To4() == nil || matchesEgressIP(policy.denyOut, ip) {
			continue
		}
		if ok || matchesEgressIP(policy.allowOut, ip) || policy.defaultAllow {
			address = net.JoinHostPort(ip.To4().String(), strconv.Itoa(int(port)))
			break
		}
	}
	if address == "" {
		c.logEgressDecision(policy.sandboxID, httpRequest, "deny", "destination IP or policy")
		return deniedEgressResponse(typev3.StatusCode_Forbidden, codes.PermissionDenied, "egress denied"), nil
	}
	if ctx.Err() != nil {
		return deniedEgressResponse(typev3.StatusCode_ServiceUnavailable, codes.Unavailable, "egress authorization canceled"), nil
	}
	// Resolution may overlap cleanup/reassignment. Do not approve a revoked owner.
	current := slot.egressPolicy.Load()
	active := current != nil && current.assignmentID == assignmentID
	if !active {
		return deniedEgressResponse(typev3.StatusCode_Forbidden, codes.PermissionDenied, "egress assignment expired"), nil
	}
	c.logEgressDecision(policy.sandboxID, httpRequest, "allow", "policy")
	return &authv3.CheckResponse{
		Status: &statuspb.Status{Code: int32(codes.OK)},
		HttpResponse: &authv3.CheckResponse_OkResponse{
			OkResponse: &authv3.OkHttpResponse{Headers: []*corev3.HeaderValueOption{{
				Header:       &corev3.HeaderValue{Key: egressDestinationHeader, Value: address},
				AppendAction: corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD,
			}}},
		},
	}, nil
}

func matchesEgressIP(entries []string, ip net.IP) bool {
	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		if address := net.ParseIP(entry); address != nil {
			if address.Equal(ip) {
				return true
			}
			continue
		}
		if _, network, err := net.ParseCIDR(entry); err == nil && network.Contains(ip) {
			return true
		}
	}
	return false
}

func matchingEgressRule(rules []runtimeapi.SandboxEgressRule, request *authv3.AttributeContext_HttpRequest) (runtimeapi.SandboxEgressRule, bool) {
	host, port := requestHostPort(request)
	protocol := strings.ToLower(strings.TrimSpace(request.GetScheme()))
	var allowed *runtimeapi.SandboxEgressRule
	for i := range rules {
		rule := rules[i]
		rule.Action = strings.ToLower(strings.TrimSpace(rule.Action))
		rule.Protocol = strings.ToLower(strings.TrimSpace(rule.Protocol))
		if !hostMatches(rule.Host, host) || (rule.Port != 0 && rule.Port != port) ||
			(rule.Protocol != "" && !strings.EqualFold(rule.Protocol, protocol)) ||
			(rule.Method != "" && !strings.EqualFold(rule.Method, request.GetMethod())) ||
			!egressPathMatches(rule.Path, request.GetPath()) {
			continue
		}
		if strings.EqualFold(rule.Action, "deny") {
			return rule, true
		}
		if allowed == nil {
			allowed = &rule
		}
	}
	if allowed == nil {
		return runtimeapi.SandboxEgressRule{}, false
	}
	return *allowed, true
}

func validEgressPath(pattern string) bool {
	if pattern == "" {
		return true
	}
	if !strings.HasPrefix(pattern, "/") || strings.ContainsAny(pattern, "?#\\") {
		return false
	}
	segments := strings.Split(pattern, "/")
	for i, segment := range segments {
		if strings.Contains(segment, "*") && segment != "*" && !(segment == "**" && i == len(segments)-1) {
			return false
		}
	}
	return true
}

func egressPathMatches(pattern, requestPath string) bool {
	if pattern == "" {
		return true
	}
	// Match Envoy's supplied path without the query; do not decode or clean it.
	requestPath, _, _ = strings.Cut(requestPath, "?")
	segments, actual := strings.Split(pattern, "/"), strings.Split(requestPath, "/")
	for i, segment := range segments {
		if i >= len(actual) {
			return false
		}
		if segment == "**" {
			return true
		}
		if segment == "*" {
			if actual[i] == "" {
				return false
			}
		} else if segment != actual[i] {
			return false
		}
	}
	return len(segments) == len(actual)
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
	return certificate, intermediates, guestCAPEM, nil
}

func (c *egressSecurityController) logEgressDecision(sandboxID string, request *authv3.AttributeContext_HttpRequest, decision, reason string) {
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
	// Do not let NOTRACK or invalid connection state bypass direction checks.
	if err := tables.Append("mangle", egressMangleChain,
		"-m", "conntrack", "--ctstate", "INVALID,UNTRACKED", "-j", "DROP"); err != nil {
		return fmt.Errorf("block untracked sandbox traffic: %w", err)
	}
	// Replies to inbound connections are not sandbox-initiated egress.
	inboundReply := []string{
		"-m", "conntrack", "--ctstate", "ESTABLISHED", "--ctdir", "REPLY",
	}
	if err := tables.Append("mangle", egressMangleChain, append(inboundReply, "-j", "RETURN")...); err != nil {
		return fmt.Errorf("allow sandbox inbound reply outside egress proxy: %w", err)
	}
	for _, protocol := range []string{"udp", "tcp"} {
		for _, server := range c.dnsServers {
			match := []string{"-p", protocol, "--dport", "53", "-d", server}
			if err := tables.Append("mangle", egressMangleChain, append(match, "-j", "RETURN")...); err != nil {
				return fmt.Errorf("complete sandbox DNS bypass: %w", err)
			}
		}
	}
	proxyMatch := []string{"-p", "tcp", "-m", "multiport", "--dports", "80,443"}
	if err := tables.Append("mangle", egressMangleChain, append(proxyMatch, "-j", "RETURN")...); err != nil {
		return fmt.Errorf("allow proxyable sandbox traffic: %w", err)
	}
	if err := tables.Append("mangle", egressMangleChain, "-j", "DROP"); err != nil {
		return fmt.Errorf("block traffic that bypasses egress proxy: %w", err)
	}
	hook := []string{"-i", c.bridgeName, "-j", egressMangleChain}
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
		"-i", c.bridgeName,
		"-p", "tcp", "-m", "multiport", "--dports", "80,443",
		"-m", "conntrack", "--ctstate", "NEW", "--ctdir", "ORIGINAL",
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
		redirect := []string{
			"-i", c.bridgeName,
			"-p", "tcp", "-m", "multiport", "--dports", "80,443",
			"-m", "conntrack", "--ctstate", "NEW", "--ctdir", "ORIGINAL",
			"-j", "REDIRECT", "--to-ports", strconv.Itoa(c.cfg.ProxyPort),
		}
		errs = append(errs, tables.DeleteIfExists("nat", "PREROUTING", redirect...))
		hook := []string{"-i", c.bridgeName, "-j", egressMangleChain}
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
