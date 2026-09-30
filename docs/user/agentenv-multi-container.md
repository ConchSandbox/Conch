# AgentENV 多节点容器模拟测试

本文使用一台宿主机模拟多节点部署：Scheduler 和 Gateway 运行在宿主机，Node A 和 Node B 运行在 Docker 容器中。集群接入的完整说明见[AgentENV 集群接入](agentenv.md)。

## 1. 前置条件

宿主机准备已编译的 AgentENV Scheduler/Gateway、Conch `conchd`/`conch`、Docker 和 OpenEuler 基础镜像。

```bash
export AENV_API_KEY="e2b_$(openssl rand -hex 32)"
```

## 2. 配置宿主机

### 2.1 Scheduler

Scheduler 使用容器映射到宿主机的端口访问 Node：

```json
{
  "log_level": "info",
  "scheduler": {
    "grpc_listen_addr": ":9090",
    "metrics_listen_addr": ":9101",
    "strategy": "round_robin",
    "report_ttl": "30s",
    "binding_ttl": "30s",
    "redis_addr": "",
    "discovery": {"mode": "static"},
    "nodes": [
      {"id": "conch-node-a", "endpoint": "http://127.0.0.1:8001"},
      {"id": "conch-node-b", "endpoint": "http://127.0.0.1:8002"}
    ]
  }
}
```

```bash
AGENTENV_SERVICES_DIR="/path/to/AgentENV/services"
"$AGENTENV_SERVICES_DIR/bin/scheduler" -config "$AGENTENV_SERVICES_DIR/config/scheduler.local.json"
```

### 2.2 Gateway

```json
{
  "log_level": "info",
  "gateway": {
    "http_listen_addr": ":8080",
    "metrics_listen_addr": ":9102",
    "scheduler_addr": "127.0.0.1:9090",
    "request_timeout": "90s",
    "sandbox_proxy_domains": []
  }
}
```

```bash
export AENV_API_KEY="e2b_换成实际密钥"
AGENTENV_SERVICES_DIR="/path/to/AgentENV/services"
"$AGENTENV_SERVICES_DIR/bin/gateway" -config "$AGENTENV_SERVICES_DIR/config/gateway.local.json"
```

## 3. Node 配置容器

### 3.1 创建容器

```bash
docker run -dit --name nodeA --hostname nodeA --privileged \
  --add-host=host.docker.internal:host-gateway -p 8001:8000 \
  -e AENV_API_KEY="$AENV_API_KEY" \
  hub.oepkgs.net/openeuler/openeuler:24.03-lts-sp3 /bin/bash

docker run -dit --name nodeB --hostname nodeB --privileged \
  --add-host=host.docker.internal:host-gateway -p 8002:8000 \
  -e AENV_API_KEY="$AENV_API_KEY" \
  hub.oepkgs.net/openeuler/openeuler:24.03-lts-sp3 /bin/bash
```

### 3.2 安装容器依赖

```bash
for c in nodeA nodeB; do
  docker exec "$c" yum install -y wget
  docker exec "$c" wget -O /tmp/stratovirt.rpm https://atomgit.com/hu-zhangying/Conch/releases/download/conch-0.1.0/stratovirt-2.4.0-12.oe2403sp4.x86_64.rpm
  docker exec "$c" wget -O /tmp/erofs-utils.rpm https://atomgit.com/hu-zhangying/Conch/releases/download/conch-0.1.0/erofs-utils-1.9.3-2.oe2403sp3.x86_64.rpm
  docker exec "$c" yum install -y /tmp/erofs-utils.rpm /tmp/stratovirt.rpm
  docker exec "$c" wget -O /tmp/conch.rpm https://atomgit.com/hu-zhangying/Conch/releases/download/conch-0.1.0/conch-0.1.0-6.oe2403sp4.x86_64.rpm
  docker exec "$c" yum install -y /tmp/conch.rpm
done
```

### 3.3 复制二进制和配置

```bash
docker exec nodeA mkdir -p /usr/local/bin /etc/conch
docker exec nodeB mkdir -p /usr/local/bin /etc/conch
docker cp /path/to/node-a/bin/conchd nodeA:/usr/local/bin/conchd
docker cp /path/to/node-a/bin/conch nodeA:/usr/local/bin/conch
docker cp /path/to/node-b/bin/conchd nodeB:/usr/local/bin/conchd
docker cp /path/to/node-b/bin/conch nodeB:/usr/local/bin/conch
docker cp /path/to/node-a/config.yaml nodeA:/etc/conch/config.yaml
docker cp /path/to/node-b/config.yaml nodeB:/etc/conch/config.yaml
```

Node A 使用 CID 起始值为 `3` 的 `conchd`，Node B 使用 CID 起始值为 `10003` 的 `conchd`；不能把同一个版本复制到两个容器。

### 3.4 Node 配置

Node A 的 `/etc/conch/config.yaml`：

```yaml
app:
  name: conch
log:
  level: debug
  output: stdout
server:
  work_dir: /var/run/conch
  state_dir: /var/lib/conch
e2b:
  listen_addr: "0.0.0.0:8000"
  sandbox_proxy_domains: []
cluster:
  scheduler_addr: "host.docker.internal:9090"
  node_id: "conch-node-a"
  cluster_id: "conch-agentenv-demo"
sandbox:
  backend: stratovirt
  vsock_signal_retry: 10ms
  vsock_signal_timeout: 60s
  request_timeout: 60s
  default_spec:
    template_name: ""
    vcpu_num: 2
    vcpu_max: 2
    ram_mb: 2048
  stratovirt:
    binary: /usr/bin/stratovirt
network:
  warm_pool_size: 10
  cni:
    plugin_bin_dirs: [/usr/libexec/cni]
volume:
  max_mounts: 10
  backend: virtiofs
  virtiofs:
    binary: /usr/libexec/virtiofsd
```

Node B 配置与 Node A 相同，只将 `node_id` 改为 `conch-node-b`。

### 3.5 启动 Node、拉取 Template 和验证

```bash
docker exec -d nodeA sh -c '/usr/local/bin/conchd --config /etc/conch/config.yaml > /var/log/conchd.log 2>&1'
docker exec -d nodeB sh -c '/usr/local/bin/conchd --config /etc/conch/config.yaml > /var/log/conchd.log 2>&1'
docker exec -it nodeA sh -lc 'CONCH_API_TIMEOUT=30m /usr/local/bin/conch template pull hub.oepkgs.net/conch/e2b-rootfs-example:conch'
docker exec -it nodeB sh -lc 'CONCH_API_TIMEOUT=30m /usr/local/bin/conch template pull hub.oepkgs.net/conch/e2b-rootfs-example:conch'
curl -i http://127.0.0.1:8001/health
curl -i http://127.0.0.1:8002/health
curl -H "X-API-Key: $AENV_API_KEY" http://127.0.0.1:8080/nodes
```

## 4. Python SDK 使用

```bash
export E2B_API_KEY="$AENV_API_KEY"
export E2B_API_URL=http://127.0.0.1:8080
export E2B_SANDBOX_URL="$E2B_API_URL"
export E2B_TEMPLATE_ID=hub.oepkgs.net/conch/e2b-rootfs-example:conch
python3 -m venv .venv-agentenv
.venv-agentenv/bin/pip install 'e2b-code-interpreter==2.5.0'
.venv-agentenv/bin/python examples/agentenv/python-sdk.py
```

## 5. 清理容器

```bash
docker exec nodeA pkill conchd || true
docker exec nodeB pkill conchd || true
docker stop nodeA nodeB
docker rm nodeA nodeB
```

宿主机上的 Scheduler 和 Gateway 使用 `Ctrl-C` 停止。
