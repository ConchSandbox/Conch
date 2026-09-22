# Sandbox 域名出站策略

Conch 可以按 Sandbox 控制 HTTP/HTTPS 出站请求。iptables 直接匹配 Conch 专用网桥的入站流量，经主机 NAT `REDIRECT` 交给 Envoy；Envoy 调用 Conch 的授权接口，根据 Sandbox 来源地址和域名规则决定允许或拒绝。

该功能当前仅支持 IPv4 HTTP/HTTPS。它不注入或替换凭据，也不支持任意 TCP/UDP 协议。启用域名策略的 Sandbox 会阻止 QUIC/HTTP3 等绕过代理的流量。

## 1. 主机准备

主机需要：

- 配置路径中的 Envoy 二进制，默认 `/usr/bin/envoy`。
- 使用 Envoy 1.39.1 或更新版本，需包含 Lua、ext_authz 和支持目标地址请求头的 `ORIGINAL_DST` cluster。
- 支持 iptables `REDIRECT`、连接跟踪和 IPv4 转发的 Linux 内核。
- 启用 `br_netfilter`，并由管理员明确配置 `net.bridge.bridge-nf-call-iptables=1`，确保网桥端口间直接转发的流量也经过 iptables。Conch 启动时检查该设置，不自动修改，因为它会影响其他网桥。
- Conch 原有的 CNI 插件及 root 或等效网络权限。
- CNI 网桥仅供 Conch 使用；不得为其流量配置绕过这些规则的 flow offloading 或其他转发路径。`NOTRACK` 流量会被拒绝，不能关闭连接跟踪。主机配置在运行期间也必须满足这些条件。
- Envoy 使用的证书、私钥、Sandbox 信任的 CA，以及验证外部 HTTPS 服务的 CA bundle。

以下命令生成一套仅供测试的本地 CA，并签发覆盖目标域名（以下示例包含 `google.com`、`github.io`、`*.github.io` 和 `bing.com` ）的 Envoy 证书：

```bash
sudo install -d -o root -g root -m 0700 /etc/conch/egress-tls
cd /etc/conch/egress-tls

sudo openssl req -x509 -newkey rsa:3072 -sha256 -nodes \
  -days 365 -subj '/CN=Conch Test Egress CA' \
  -keyout ca.key -out ca.crt

sudo openssl req -newkey rsa:2048 -nodes \
  -subj '/CN=Conch Test Egress Proxy' \
  -keyout envoy_tls.key -out envoy_tls.csr

cat <<'EOF' | sudo tee envoy_tls.ext >/dev/null
subjectAltName=DNS:google.com,DNS:github.io,DNS:*.github.io,DNS:bing.com
extendedKeyUsage=serverAuth
keyUsage=digitalSignature,keyEncipherment
EOF

sudo openssl x509 -req -sha256 -days 90 \
  -in envoy_tls.csr -CA ca.crt -CAkey ca.key -CAcreateserial \
  -extfile envoy_tls.ext -out envoy_tls.crt

sudo chown root:root ca.crt ca.key envoy_tls.crt envoy_tls.key
sudo chmod 0644 ca.crt envoy_tls.crt
sudo chmod 0600 ca.key envoy_tls.key
```

这套 CA 的私钥保存在 Conch 主机，只适合功能验证。生产环境应使用受控 PKI，并限制、审计和轮换 CA 私钥。`envoy_tls.crt` 必须覆盖所有允许或显式拒绝的 HTTPS 域名，否则 Conch 会拒绝应用该策略。Sandbox 镜像使用其他 trust store 路径时，还需要在 Template 中安装 `ca.crt`。

## 2. 启用主机功能

在 `config.yaml` 中配置：

```yaml
network:
  egress_security:
    enabled: true
    proxy_port: 15001
    authz_socket: /run/conch/egress-authz.sock
    admin_socket: /run/conch/envoy-admin.sock
    tls_cert_file: /etc/conch/egress-tls/envoy_tls.crt
    tls_key_file: /etc/conch/egress-tls/envoy_tls.key
    guest_ca_file: /etc/conch/egress-tls/ca.crt
    upstream_ca_file: /etc/pki/tls/certs/ca-bundle.crt
    proxy_binary: /usr/bin/envoy
    bootstrap_path: /run/conch/envoy-egress.yaml
```

`conchd` 启动时会校验证书和文件权限、生成 Envoy bootstrap、启动 Envoy、等待其就绪，并安装主机重定向规则。任何一步失败都会使网络初始化失败。Envoy 意外退出会记录错误；当前不会自动重启 Envoy。

## 3. 为 Sandbox 配置策略

conchd 的 `network.egress_security.enabled=true` 时，所有新创建的 Sandbox 均进入 Envoy 授权流程，不再使用每个 Sandbox 的 `egress_proxy.enabled` 开关。创建 Sandbox 时，在请求的 `network` 字段中提供规则和默认动作：

```json
{
  "sandbox_id": "egress-demo",
  "template_id": "sha256:<template-digest>",
  "network": {
    "default_action": "deny",
    "rules": [
      {"action": "allow", "protocol": "https", "host": "google.com"},
      {"action": "allow", "protocol": "https", "host": "github.io"},
      {"action": "allow", "protocol": "https", "host": "*.github.io"},
      {"action": "deny", "protocol": "https", "host": "bing.com"}
    ]
  }
}
```

规则语义如下：

- `action` 必须是 `allow` 或 `deny`；匹配的 `deny` 优先于匹配的 `allow`。
- 域名规则匹配 `deny` 时拒绝；否则 Conch 在主机解析 IPv4，排除匹配 `denyOut` 的地址，再选择满足域名允许、`allowOut` 或 `default_action=allow` 的地址。没有可用地址时拒绝；`default_action` 省略时按 `deny` 处理。
- `allowOut` 和 `denyOut` 支持 IPv4 地址及 CIDR，例如 `198.51.100.0/24`，不支持 `*` 地址写法。代理连接的是 Conch 检查并选定的上游地址，而不是 guest 最初连接的地址。
- 域名允许仅匹配请求主机名，不自动放行直接 IP 访问；IP 允许可放行解析到该 IP 的域名请求，但显式拒绝仍优先。
- 启用出站安全功能时，出站 IP 规则仅适用于经代理的 HTTP/HTTPS，不适用于豁免的 DNS 流量；未启用时，普通 IP 策略行为不变。
- Conch 根据请求主机名使用主机解析配置（包括 `/etc/hosts`）选择目标，解析限时一秒。因此 `curl --resolve` 等客户端地址覆盖不决定最终连接 IP。Envoy 不再次解析域名或切换到未经检查的地址；HTTPS 的 SNI 和证书校验仍使用原请求主机名。
- `protocol` 可为 `http` 或 `https`；省略时同时匹配两者。
- `*.github.io` 匹配 `pages.github.io`，但不匹配根域名 `github.io`。
- `port`、`method` 和 `path_prefix` 是可选的附加条件；方法匹配不区分大小写，路径前缀匹配区分大小写。
- 每个 Sandbox 最多 256 条规则。未提供网络配置或规则为空时，HTTP/HTTPS 默认全部拒绝；设置 `default_action=allow` 可允许未匹配规则的请求，显式拒绝规则仍然有效。
- `default_action=allow` 不关闭代理，也不等于允许所有网络流量：HTTPS 仍需证书覆盖目标域名且 guest 信任 CA；域名/IP 显式拒绝、入站 IP 策略以及其他端口和协议限制仍然生效。DNS 仅允许访问 CNI 配置的解析服务器。
- conchd 的全局开关关闭时，Sandbox 仅使用普通 IP 层网络策略，不能配置 L7 规则或 `default_action`。

不同 Sandbox 可以提交不同的 `network.rules`。Conch 按 Slot 的 CNI 来源 IP 隔离策略，并在 Slot 回收前清除内存策略，避免策略跨 Sandbox 复用。共享 iptables 规则在网络池预热和 Sandbox 启动前安装，安装失败则网络初始化失败；Slot 回收不会移除共享规则。

例如，允许域名 `example.com`、同时设置 `denyOut=["1.1.1.1"]` 时，Conch 不会选择 `1.1.1.1`，但可以选择该域名的其他获准 IPv4。设置 `allowOut=["1.1.1.1"]` 时，无域名拒绝规则且解析结果包含该 IP 的请求可被允许，即使没有对应域名允许规则。全局代理启用时，`allow_internet_access=false` 将未匹配请求的默认动作设为拒绝，显式域名/IP 允许仍然有效，显式拒绝始终优先。

HTTP 和 HTTPS 均先移除客户端提供的目标地址请求头，再由 Conch 按来源 Sandbox 检查组合策略，并通过授权响应写入 `x-conch-egress-destination`，指定唯一获准的 IPv4 和端口。Envoy 使用该地址转发；缺少获准地址时返回 503，不回退到原始目标。解析失败返回 503，策略拒绝返回 403。

运行中的 READY 或 SUSPENDED Sandbox 可通过以下接口整体替换网络配置：

替换时省略 `default_action` 会恢复默认拒绝；删除全部规则不会关闭 Envoy 拦截。全局启用功能时，应为每个需要出网的 Sandbox 提供允许规则或显式设置 `default_action=allow`。该变更不保证接管启用功能前已有的直接连接，应重新创建这些 Sandbox。

```text
PUT /api/v1/sandboxes/{sandboxID}/network
```

更新不是增量操作；请求必须包含希望保留的完整网络配置。当前 Python SDK 的 `update_network()` 仅覆盖 IP 层字段，更新 L7 规则时应直接调用 HTTP API。

## 4. 验证

在 Sandbox 中执行：

```bash
curl -I https://google.com
curl -I https://pages.github.io
curl -I https://bing.com
curl -I http://example.com
```

使用上面的 `default_action=deny` 示例策略时，预期 `google.com` 和 `pages.github.io` 可以到达上游，`bing.com` 和 `example.com` 返回 `403`。`example.com` 使用 HTTP，以验证未匹配规则的拒绝行为，避免测试证书未覆盖该域名导致 TLS 验证先失败。同时检查 `conchd` 日志中的 `sandbox_id`、目标 host、method 和 allow/deny 决策。

如果所有 HTTPS 请求都出现证书错误，先确认 guest 使用的 trust bundle 包含 `/etc/conch/egress-ca.crt`。如果请求没有到达授权日志，检查 Envoy readiness、网桥的 iptables 匹配和 NAT `REDIRECT` 规则、`bridge-nf-call-iptables` 设置及 conchd 的 `network.egress_security.enabled`。
