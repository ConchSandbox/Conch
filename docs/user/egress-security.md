# 沙箱域名出站策略

Conch 通过共享的 iptables 规则和 NAT `REDIRECT` 把沙箱出口的 HTTP/HTTPS 请求交给代理（Envoy），再按各自沙箱的域名、IP 和请求条件等决定是否放行。目标由 conchd 根据请求中的域名解析 IP，并与对应沙箱的访问规则进行校验；只有检查通过，Envoy 才会连接对应的服务器。

## 1. 主机准备

主机需要安装 `/usr/bin/envoy`（1.39.1 或更新版本，包含 Lua、网络和 HTTP ext_authz，以及支持目标地址请求头的 `ORIGINAL_DST` cluster）和原有 CNI 插件，并以 root 或等效权限运行 Conch。内核需要支持 IPv4 转发、连接跟踪和 iptables `REDIRECT`。

管理员还需启用 `br_netfilter` 并设置 `net.bridge.bridge-nf-call-iptables=1`，使网桥内沙箱间的流量也经过检查；Conch 只检查该设置，不会自动修改，以免影响其他网桥。出站规则作用于整个 CNI 网桥，因此该网桥不能与其他业务共用。

HTTPS 所使用的证书目前默认路径为：

| 文件 | 用途 |
| --- | --- |
| `/etc/conch/egress-tls/envoy_tls.crt` | Envoy 服务端证书及中间证书链 |
| `/etc/conch/egress-tls/envoy_tls.key` | 对应私钥，权限须为仅所有者可访问，例如 `0600` |
| `/etc/conch/egress-tls/ca.crt` | 供 guest 信任的公开 CA，不包含私钥 |

证书必须覆盖规则中允许或显式拒绝的 HTTPS 域名，否则策略无法应用；拒绝域名也需要证书，是因为客户端须先完成 TLS 握手才能收到 HTTP 403。测试环境可使用文末的本地 CA，生产环境应使用受控 PKI。

Conch 在启动握手中传入公开 CA，由 guest 初始化程序写入 `/etc/conch/egress-ca.crt` 并追加到常见的系统 trust bundle，恢复时也会重新应用。使用如 Java keystore 等独立证书库的应用仍需自行配置。


## 2. 启用主机功能

在 `config.yaml` 中启用功能（当前默认为关闭），并指定 Envoy 二进制和默认代理端口：

```yaml
network:
  egress_security:
    enabled: true
    proxy_port: 15001
    proxy_binary: /usr/bin/envoy
```

在仓库根目录执行，在宿主机本地配置路径（`/etc/conch`）下安装 Envoy bootstrap config 模板：

```bash
sudo install -d -o root -g root -m 0755 /etc/conch/envoy
sudo install -o root -g root -m 0644 config/envoy/egress.yaml /etc/conch/envoy/egress.yaml
```

Conch 运行时优先读取 `/etc/conch/envoy/egress.yaml`，不存在时才读取工作目录下的 `config/envoy/egress.yaml`。监听端口仅通过 `config.yaml` 的 `network.egress_security.proxy_port` 配置；上游 CA bundle 路径仍可在模板中修改。修改模板时应保留原有拦截和授权过滤器。

默认 socket 和生成后的 Envoy 配置位于 `<server.work_dir>/egress/`，分别为 `authz.sock`、`envoy-admin.sock` 和 `envoy.yaml`；`server.work_dir` 默认是 `/var/run/conch`。
Envoy 验证外部网站所用的 CA bundle 在模板的 HTTPS 上游 `trusted_ca.filename` 中指定，默认 `/etc/pki/tls/certs/ca-bundle.crt`，其他发行版需改为实际路径。

`conchd` 会在网络池预热前校验证书、启动 Envoy、等待就绪并安装重定向规则，任一步失败都会使网络初始化失败。Envoy 意外退出会记录错误，但当前不会自动重启；更换 CA 或静态 TLS 文件后需重启 conchd 才能生效。

## 3. 为沙箱配置策略

未启用 `network.egress_security.enabled` 时仍可使用 IP 策略，但提交非空的 `rules` 会报错。
全局功能启用后，所有新沙箱的出口流量都将经过代理，即使没有提交网络配置也不例外。
READY 或 SUSPENDED 状态的沙箱可通过 Python SDK 的 `update_network()` 或 `PUT /api/v1/sandboxes/{sandboxID}/network` 整体替换网络配置，请求中应包含希望保留的全部字段，也可在创建沙箱时通过 `network` 指定各自的规则，例如：


```json
{
  "sandbox_id": "egress-demo",
  "template_id": "sha256:<template-digest>",
  "network": {
    "allow_internet_access": false,
    "rules": [
      {"action": "allow", "protocol": "https", "host": "google.com"},
      {"action": "allow", "protocol": "https", "host": "*.github.io"},
      {"action": "deny", "protocol": "https", "host": "bing.com"}
    ]
  }
}
```

请求处理流程如下；域名和 IP 的匹配、解析均由 conchd 完成，Envoy 等待授权结果后转发。

```text
Guest 发起 HTTP/HTTPS 请求
    │
    ▼
Guest TAP → slot 网络命名空间 → CNI veth → 主机 CNI 网桥
    │
    ▼
iptables NAT PREROUTING：将 TCP 80/443 重定向到 Envoy
    │
    ▼
Envoy 接收请求（HTTPS 先终止 TLS），向 conchd 请求授权
    │
    ▼
conchd：命中域名 DENY 规则？
    ├─ 是 → 拒绝
    └─ 否
        │
        ▼
记录是否命中域名 ALLOW 规则
    │
    ▼
conchd 解析域名，获取 IPv4 地址
    ├─ 失败 → 返回解析错误
    └─ 成功
        │
        ▼
选取下一个解析到的 IP
    ├─ 已无剩余 IP → 拒绝
    └─ 有 IP
        │
        ▼
命中 denyOut？
    ├─ 是 → 跳过，检查下一个 IP
    └─ 否
        │
        ▼
之前已命中域名 ALLOW？
    ├─ 是 → 批准此 IP
    └─ 否
        │
        ▼
命中 allowOut？
    ├─ 是 → 批准此 IP
    └─ 否
        │
        ▼
allow_internet_access 为 true 或省略？
    ├─ 是 → 批准此 IP
    └─ 否 → 跳过，检查下一个 IP

批准此 IP
    │
    ▼
conchd 将获准的 IP 和端口返回 Envoy
    │
    ▼
Envoy 连接目标服务器并转发请求
    │
    ▼
服务器响应 → Envoy → Guest
```

每个沙箱最多 256 条域名规则，每条规则中，`protocol` 可选 `http` 或 `https`，省略时匹配两者，还可用 `port`、`method`、`path` 限定请求；所有指定条件须同时满足，方法匹配不区分大小写。

`path` 省略时不限制路径，普通路径按大小写精确匹配；`*` 只能单独匹配一个非空段，`**` 只能位于末尾，匹配剩余路径。例如 `/api/*/read` 匹配 `/api/user/read`，不匹配 `/api/team/user/read`；`/api/**` 匹配 `/api/` 及其下所有路径，但不匹配 `/api`。查询参数不参与匹配，Conch 不额外进行 URL 解码或路径归一化。

例如可配置以下规则；

```json
[
  {"action": "allow", "host": "example.com", "path": "/api/*/read"},
  {"action": "deny", "host": "example.com", "path": "/api/*/write"}
]
```

若只允许指定路径访问，还需显式设置 `allow_internet_access=false`，避免未匹配路径被默认放行：

`allowOut`、`denyOut` 接受 IPv4 地址或 CIDR，不接受 `*` 通配符。
`*` 通配符可用于域名策略中，但使用时需注意：
 -  `*.github.io` 匹配子域名（`pages.github.io`），根域名（`github.io`）无法匹配，如需使用应额外注明；
 - 策略通配符可匹配多层子域名如（`a.pages.github.io`），但证书通配符通常只覆盖一层（`pages.github.io`），HTTPS 仍受实际证书范围限制

Conch 使用主机解析配置（包括 `/etc/hosts`）查询 IPv4，限时一秒，再把选定地址交给 Envoy；guest 自行指定的 IP（如 `curl --resolve`）不决定最终上游地址，HTTPS 的 SNI 和证书校验仍使用请求的域名。正常策略拒绝返回 403，解析失败或缺少获准地址返回 503。


## 4. 验证

使用上面的示例策略创建沙箱后，在 guest 中执行：

```bash
curl -I https://google.com
curl -I https://pages.github.io
curl -I https://bing.com
curl -I http://example.com
```

前两个请求应到达上游，后两个应被 Conch 拒绝并返回 403；同时检查 conchd 的 `sandbox egress request` 日志，确认 sandbox_id、host、method 和决策，以免把上游自己的 403 当作策略生效。

HTTPS 证书错误时，先检查 guest 的信任库和证书域名覆盖范围；没有授权日志时，再检查 Envoy 是否就绪、网桥是否命中重定向规则，以及 `bridge-nf-call-iptables` 是否仍为 1。

## 5. 使用边界

### 约束

该功能只支持 IPv4 HTTP/HTTPS，其他出站端口、QUIC/HTTP3 等协议被阻止，DNS 则仅豁免到 CNI 配置解析服务器的流量，且不受 HTTP/HTTPS 的 `allowOut`、`denyOut` 检查。`allow_internet_access=true` 或省略该字段都不关闭这些限制，也不取消 HTTPS 证书要求。启用前已有的直接连接不保证被接管，应重新创建相应沙箱。

Envoy 每条连接先做一次归属查询并绑定沙箱分配标识，通过内部元数据传递，而后续请求都会对该标识进行校验。slot 清理会把标识撤销，但已获准发送的请求不会因此撤回；极端情况下，如果旧连接的归属查询时 slot 已经被用于创建新沙箱，则该 Envoy 连接仍有可能被绑定到新的沙箱及其策略上。

主机默认使用 `/etc/pki/tls/certs/ca-bundle.crt` 校验上游证书，这是 Fedora/RHEL 系常用路径。Debian/Ubuntu 系通常使用 `/etc/ssl/certs/ca-certificates.crt`，需在 Envoy 模板的上游 TLS 配置中调整 `trusted_ca.filename`；文件不存在或不可读时，Envoy 无法正常启动。

Guest CA 在沙箱启动时传入，无需预先写入镜像。Conch 只向以下已存在的证书包追加 CA，不自动识别其他发行版的信任库：

| Guest 常见发行版 | 证书包路径 |
| --- | --- |
| CentOS/RHEL 系 | `/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem` |
| Fedora/RHEL 系 | `/etc/pki/tls/certs/ca-bundle.crt`（可能为兼容链接） |
| Debian/Ubuntu 系 | `/etc/ssl/certs/ca-certificates.crt` |

这些文件依赖 guest 已安装相应证书包，且须可写；读写失败会导致初始化失败。如果均不存在，Conch 仅通过 `SSL_CERT_FILE` 指向单独保存的 CA，不能保证所有应用信任它。使用其他系统信任库、Java keystore 等独立证书库的应用仍需自行配置。

### 性能影响

启用 Envoy 后，每个 HTTP 请求都需要在 Envoy 与 conchd 之间完成授权 RPC，再由代理转发到上游。以下按不同并发数比较启用前后的 HTTP 请求吞吐量和 P99 延迟，以观察这条额外处理路径带来的开销。

每组对比使用相同的主机、镜像、目标服务、连接复用方式和请求内容，启用过滤时无配置规则。测试结果如下，RPS 表示每秒完成的请求数，P99 表示 99% 请求的延迟不超过该值。

| 请求并发数 | Envoy 关闭 RPS | Envoy 启用 RPS | 吞吐下降 | Envoy 关闭 P99（ms） | Envoy 启用 P99（ms） | 延迟增加 |
| --- | --- | --- | --- | --- | --- | --- |
| 1 | 6.6k | 1.4k | 78.8% | 0.2 | 1.0 | 400.0% |
| 16 | 7.0k | 5.0k | 28.6% | 5.9 | 8.4 | 42.4% |
| 256 | 6.1k | 4.6k | 24.6% | 126 | 161 | 27.8% |
| 4096 | 6.0k | 4.5k | 25.0% | 161 | 187 | 16.1% |
| 65536 | 5.3k | 4.0k | 24.5% | 161 | 187 | 16.1% |

吞吐下降和延迟增加均以关闭 Envoy 时的数据为基准；单并发吞吐下降约 79%，并发数达到 16 后下降约 25%–29%，支持将逐请求授权 RPC 列为主要疑似瓶颈。

另外在百并发创建沙箱的测试中，测得创建的 P99 时延由于启动 Envoy 增加约 `2.1 %`。

## 附：生成测试证书

下面的本地 CA 仅用于功能验证，其私钥留在主机且须妥善保护；Conch 只向 guest 传递公开的 `ca.crt`。示例证书覆盖验证所用的域名，包括用于测试拒绝的 `bing.com`。

```bash
sudo install -d -o root -g root -m 0700 /etc/conch/egress-tls
cd /etc/conch/egress-tls
sudo openssl req -x509 -newkey rsa:3072 -sha256 -nodes \
  -days 365 -subj '/CN=Conch Test Egress CA' -keyout ca.key -out ca.crt
sudo openssl req -newkey rsa:2048 -nodes \
  -subj '/CN=Conch Test Egress Proxy' -keyout envoy_tls.key -out envoy_tls.csr
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
