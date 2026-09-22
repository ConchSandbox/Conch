# Sandbox 域名出站策略

Conch 可以按 Sandbox 控制 HTTP/HTTPS 出站请求。受保护的流量由 eBPF 标记，经主机 NAT `REDIRECT` 交给 Envoy；Envoy 调用 Conch 的授权接口，根据 Sandbox 来源地址和域名规则决定允许或拒绝。

该功能当前仅支持 IPv4 HTTP/HTTPS。它不注入或替换凭据，也不支持任意 TCP/UDP 协议。启用域名策略的 Sandbox 会阻止 QUIC/HTTP3 等绕过代理的流量。

## 1. 主机准备

主机需要：

- 配置路径中的 Envoy 二进制，默认 `/usr/bin/envoy`。
- 支持 eBPF、TC、iptables `REDIRECT` 和 IPv4 转发的 Linux 内核。
- Conch 原有的 CNI 插件及 root 或等效网络/eBPF 权限。
- Envoy 使用的证书、私钥、Sandbox 信任的 CA，以及验证外部 HTTPS 服务的 CA bundle。

以下命令生成一套仅供测试的本地 CA，并签发覆盖 `google.com`、`github.io`、`*.github.io` 和 `bing.com` 的 Envoy 证书：

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

创建 Sandbox 时，在请求的 `network` 字段中启用代理并提供规则：

```json
{
  "sandbox_id": "egress-demo",
  "template_id": "sha256:<template-digest>",
  "network": {
    "egress_proxy": {"enabled": true},
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
- 未匹配任何 `allow` 规则的请求默认拒绝，因此上例拒绝除列出域名外的所有请求。
- `protocol` 可为 `http` 或 `https`；省略时同时匹配两者。
- `*.github.io` 匹配 `pages.github.io`，但不匹配根域名 `github.io`。
- `port`、`method` 和 `path_prefix` 是可选的附加条件；方法匹配不区分大小写，路径前缀匹配区分大小写。
- 每个 Sandbox 最多 256 条规则。配置 `rules` 时必须设置 `egress_proxy.enabled=true`。
- 仅设置 `egress_proxy.enabled=true` 且规则为空时，HTTP/HTTPS 默认全部拒绝。
- 不启用 `egress_proxy` 的 Sandbox 不进入 L7 域名授权流程，仍遵循普通 IP 层网络策略。

不同 Sandbox 可以提交不同的 `network.rules`。Conch 按 Slot 的 CNI 来源 IP 隔离策略，并在 Slot 回收前清除 eBPF attachment 和内存策略，避免策略跨 Sandbox 复用。

运行中的 READY 或 SUSPENDED Sandbox 可通过以下接口整体替换网络配置：

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
curl -I https://example.com
```

预期 `google.com` 和 `pages.github.io` 可以到达上游，`bing.com` 和 `example.com` 返回 `403`。同时检查 `conchd` 日志中的 `sandbox_id`、目标 host、method、path 和 allow/deny 决策。

如果所有 HTTPS 请求都出现证书错误，先确认 guest 使用的 trust bundle 包含 `/etc/conch/egress-ca.crt`。如果请求没有到达授权日志，检查 Envoy readiness、NAT `REDIRECT` 规则、TC/eBPF attachment 和 Sandbox 是否启用了 `egress_proxy`。
