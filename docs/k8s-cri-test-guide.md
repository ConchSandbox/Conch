# Conch CRI 测试文档

本文档用于在单节点 Linux 环境中验证 Conch 第一版 CRI 功能。测试重点是 CRI 基础链路、PodSandbox 生命周期、状态持久化和 conchd 重启恢复。

## 1. 测试范围

覆盖内容：

- conchd 启动内建 containerd host 和 CRI gRPC server。
- crictl 通过 Conch CRI socket 调用 runtime/image 基础接口。
- `RunPodSandbox` 创建 Conch VM sandbox。
- container create/start/stop/remove 占位状态流转。
- `PullImage` 基础链路。
- conchd 重启后的 state reconcile/rehydrate。

非覆盖内容：

- 完整 OCI container rootfs 语义。
- VM 内真实业务进程启动。
- `ImageStatus`、`ListImages`、`RemoveImage`、`ImageFsInfo` 的完整 image inventory 语义。
- 多节点调度场景。
- warm VM pool。

## 2. 环境要求

建议使用独立测试机，不要在生产 Kubernetes 节点上执行测试。

基础要求：

- Linux 5.10+
- root 权限
- Go 1.26+
- Cloud-Hypervisor
- buildah
- erofs-utils
- iptables
- crictl

可选要求：

- 单节点 kubelet/kubeadm 环境

Conch 会在 conchd 进程内启动 containerd，不需要单独启动系统 containerd。

## 3. 测试配置

复制配置文件：

```bash
cp config/config.yaml /tmp/conch-cri-test.yaml
```

确认关键配置：

```yaml
server:
  unix_socket: /var/run/conchd/conchd.sock
  pid_file: /var/run/conchd/conchd.pid
  work_dir: /var/run/conch

containerd:
  root_dir: /var/lib/conch/containerd
  state_dir: /run/conch/containerd
  default_namespace: default

state:
  path: /var/lib/conch/state.db

sandbox:
  default_image: hub.oepkgs.net/conch/openeuler:odd-x86
  default_vmm_name: cloud-hypervisor
  default_vcpu_num: 2
  default_vcpu_max: 2
  default_ram_mb: 4096

cri:
  enabled: true
  socket: /var/run/conchd/conch-cri.sock
```

可选：写入 crictl 默认配置。

```bash
sudo tee /etc/crictl.yaml >/dev/null <<'EOF'
runtime-endpoint: unix:///var/run/conchd/conch-cri.sock
image-endpoint: unix:///var/run/conchd/conch-cri.sock
timeout: 30
debug: true
pull-image-on-create: false
disable-pull-on-run: false
EOF
```

## 4. 启动 conchd

编译：

```bash
make build-conchd
```

首次测试机可先准备运行依赖：

```bash
sudo ./scripts/conch-env-setup.sh provisioning
make build-conchd
```

启动：

```bash
sudo ./bin/conchd --config /tmp/conch-cri-test.yaml
```

检查 socket：

```bash
sudo test -S /var/run/conchd/conchd.sock && echo "HTTP socket OK"
sudo test -S /var/run/conchd/conch-cri.sock && echo "CRI socket OK"
```

预期结果：

- HTTP socket 存在。
- CRI socket 存在。
- conchd 日志显示 containerd host、state store、reconcile 和 CRI server 初始化完成。

## 5. 测试用例

### 5.1 Runtime 基础接口

执行：

```bash
sudo crictl --runtime-endpoint unix:///var/run/conchd/conch-cri.sock version
sudo crictl --runtime-endpoint unix:///var/run/conchd/conch-cri.sock info
```

预期结果：

- runtime name 为 `conch`。
- runtime API version 为 `v1`。
- `RuntimeReady` 为 true。
- `NetworkReady` 为 true。

### 5.2 PodSandbox 冷启动

创建 `/tmp/conch-pod.json`：

```json
{
  "metadata": {
    "name": "conch-cri-smoke",
    "uid": "conch-cri-smoke-uid",
    "namespace": "default",
    "attempt": 0
  },
  "labels": {
    "app": "conch-cri-smoke"
  },
  "annotations": {
    "conch.io/sandbox-image": "hub.oepkgs.net/conch/openeuler:odd-x86",
    "conch.io/use-snapshot": "false",
    "conch.io/vmm-name": "cloud-hypervisor",
    "conch.io/vcpu": "2",
    "conch.io/vcpu-max": "2",
    "conch.io/ram-mb": "4096"
  },
  "log_directory": "/tmp/conch-cri-logs",
  "linux": {}
}
```

执行：

```bash
POD_ID=$(sudo crictl --runtime-endpoint unix:///var/run/conchd/conch-cri.sock runp /tmp/conch-pod.json)
echo "${POD_ID}"

sudo crictl --runtime-endpoint unix:///var/run/conchd/conch-cri.sock pods -a
sudo crictl --runtime-endpoint unix:///var/run/conchd/conch-cri.sock inspectp "${POD_ID}"
```

预期结果：

- `runp` 返回 sandbox ID。
- `crictl pods -a` 能看到该 sandbox。
- `inspectp` 显示 sandbox metadata、labels、annotations 和 runtime 状态。
- conchd 日志显示 sandbox 创建成功。

### 5.3 PodSandbox 快照镜像启动

如果 `conch.io/sandbox-image` 指向的是可恢复的 Conch 快照镜像，将 `/tmp/conch-pod.json` 中的 annotation 改为：

```json
"conch.io/use-snapshot": "true"
```

预期结果：

- Conch 在当前节点通过 `sandbox-image` 解析 rootfs snapshot。
- sandbox 创建走恢复启动路径。
- 如果 rootfs snapshot 缺少 mem/vm snapshot 关联标签，创建失败并返回明确错误。

注意：CRI 不支持传本地 `snapshot-id`。snapshot ID 是 worker 节点本地 containerd 状态，不应写入 Pod 配置。

### 5.4 Container 占位状态

创建 `/tmp/conch-container.json`：

```json
{
  "metadata": {
    "name": "placeholder-container",
    "attempt": 0
  },
  "image": {
    "image": "conch-placeholder.invalid/not-executed:latest"
  },
  "command": ["sh"],
  "args": ["-c", "echo placeholder"],
  "labels": {
    "app": "conch-cri-smoke"
  },
  "annotations": {
    "conch.io/placeholder": "true"
  },
  "log_path": "placeholder-container.log",
  "linux": {}
}
```

执行：

```bash
CONTAINER_ID=$(sudo crictl --runtime-endpoint unix:///var/run/conchd/conch-cri.sock create "${POD_ID}" /tmp/conch-container.json /tmp/conch-pod.json)
echo "${CONTAINER_ID}"

sudo crictl --runtime-endpoint unix:///var/run/conchd/conch-cri.sock start "${CONTAINER_ID}"
sudo crictl --runtime-endpoint unix:///var/run/conchd/conch-cri.sock ps -a
sudo crictl --runtime-endpoint unix:///var/run/conchd/conch-cri.sock inspect "${CONTAINER_ID}"
```

预期结果：

- container 创建成功。
- `start` 后状态变为 running。
- running 是 Conch state store 中的占位状态，不表示 VM 内已执行业务进程。

清理：

```bash
sudo crictl --runtime-endpoint unix:///var/run/conchd/conch-cri.sock stop "${CONTAINER_ID}"
sudo crictl --runtime-endpoint unix:///var/run/conchd/conch-cri.sock rm "${CONTAINER_ID}"
```

### 5.5 PodSandbox 删除

执行：

```bash
sudo crictl --runtime-endpoint unix:///var/run/conchd/conch-cri.sock stopp "${POD_ID}"
sudo crictl --runtime-endpoint unix:///var/run/conchd/conch-cri.sock rmp "${POD_ID}"
sudo crictl --runtime-endpoint unix:///var/run/conchd/conch-cri.sock pods -a
```

预期结果：

- sandbox 停止并删除。
- 对应 state record 被删除或更新为删除后不可见状态。
- 关联 runtime 资源进入正常清理流程。

### 5.6 重启恢复

创建 sandbox 后停止 conchd：

```bash
sudo pkill -TERM conchd
```

使用同一配置重新启动：

```bash
sudo ./bin/conchd --config /tmp/conch-cri-test.yaml
```

检查：

```bash
sudo crictl --runtime-endpoint unix:///var/run/conchd/conch-cri.sock pods -a
```

预期结果：

- conchd 启动时执行 reconcile/rehydrate。
- 可确认的 sandbox runtime 状态被恢复到内存。
- 不可确认状态被降级为 `NOTREADY` 或 `UNKNOWN`。
- namespace runtime lease 被确认存在。

## 6. 可选 kubelet smoke test

将 kubelet runtime endpoint 指向 Conch CRI：

```bash
sudo mkdir -p /etc/systemd/system/kubelet.service.d
sudo tee /etc/systemd/system/kubelet.service.d/20-conch-cri.conf >/dev/null <<'EOF'
[Service]
Environment="KUBELET_EXTRA_ARGS=--container-runtime-endpoint=unix:///var/run/conchd/conch-cri.sock --image-service-endpoint=unix:///var/run/conchd/conch-cri.sock"
EOF

sudo systemctl daemon-reload
sudo systemctl restart kubelet
```

创建 `/tmp/conch-k8s-pod.yaml`：

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: conch-cri-smoke
  namespace: default
  annotations:
    conch.io/sandbox-image: hub.oepkgs.net/conch/openeuler:odd-x86
    conch.io/use-snapshot: "false"
    conch.io/vmm-name: cloud-hypervisor
    conch.io/vcpu: "2"
    conch.io/vcpu-max: "2"
    conch.io/ram-mb: "4096"
spec:
  restartPolicy: Never
  containers:
    - name: placeholder
      image: <conch-compatible-image>
      command: ["sh", "-c", "sleep 3600"]
```

执行：

```bash
kubectl apply -f /tmp/conch-k8s-pod.yaml
kubectl get pod conch-cri-smoke -w
```

预期结果：

- kubelet 能调用 Conch CRI 的 `Version`、`Status`、`RunPodSandbox`、`CreateContainer`、`StartContainer`。
- Pod 可能因为 image 语义不完整而停留在重试或非 Running 状态。
- 该测试只验证 kubelet 到 Conch CRI 的调用链，不验证 VM 内业务进程。

恢复 kubelet 原 runtime endpoint 后重启 kubelet。

## 7. 排障检查

CRI socket 不存在：

```bash
sudo test -S /var/run/conchd/conch-cri.sock
sudo journalctl -u kubelet -n 100 --no-pager
```

检查项：

- `cri.enabled` 是否为 true。
- conchd 是否使用了预期配置文件。
- `/var/run/conchd` 目录权限是否正确。

PodSandbox 创建失败：

- 检查 `conch.io/sandbox-image` 或 `sandbox.default_image` 是否存在并已完成 Conch image 处理。
- `use-snapshot=true` 时，确认镜像解包后的 rootfs snapshot 带有 mem/vm snapshot 关联标签。
- 检查 Cloud-Hypervisor、网络、snapshot mount 和 vsock 相关日志。

资源残留：

- conchd 关闭会保留 runtime 资源，重启后由 reconcile/rehydrate 接管。
- 如果需要彻底清理测试环境，应先停止 kubelet 和 conchd，再由测试脚本或人工流程清理 `/var/run/conch`、`/run/conch/containerd`、`/var/lib/conch/containerd` 等目录。
