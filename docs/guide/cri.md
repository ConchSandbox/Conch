# Conch CRI 使用文档

本文说明 Conch CRI 的启用方式、PodSandbox 参数语义，以及当前第一版 CRI 的能力边界。

## 1. 能力边界

当前 CRI 版本用于接通 kubelet / crictl 到 Conch VM sandbox 的基础链路：

- 支持 `RuntimeService` 基础接口。
- 支持 `RunPodSandbox` 创建 Conch VM sandbox。
- 支持 `StopPodSandbox` / `RemovePodSandbox` 删除 sandbox。
- 支持 container create/start/stop/remove 的占位状态流转。
- 支持 `PullImage` / `ListImages` / `ImageStatus` / `RemoveImage` 基础链路，用于 kubelet image 语义和 Conch image 管理入口。
- 支持通过 `sandbox-image + use-snapshot` 控制冷启动或快照恢复启动。

当前版本不实现完整 OCI container rootfs 语义。Kubernetes `containers[].image` 只用于 kubelet/CRI 的 image 和 container config 语义，不会变成 VM 内 rootfs，也不会在 VM 内启动真实业务进程。`ImageFsInfo` 当前仍为基础占位实现，暂不返回真实镜像文件系统用量。

## 2. conchd 配置

在 `config/config.yaml` 中开启 CRI：

```yaml
cri:
  enabled: true
  socket: /var/run/conchd/conch-cri.sock
```

Conch sandbox 默认参数放在 `sandbox` 配置域。HTTP API、SDK 和 CRI 共用这些默认值：

```yaml
sandbox:
  default_image: hub.oepkgs.net/conch/openeuler:odd-x86
  default_vmm_name: cloud-hypervisor
  default_vcpu_num: 2
  default_vcpu_max: 2
  default_ram_mb: 4096
```

启动 conchd：

```bash
make build-conchd
sudo ./bin/conchd --config ./config/config.yaml
```

确认 socket：

```bash
sudo test -S /var/run/conchd/conchd.sock && echo "HTTP socket OK"
sudo test -S /var/run/conchd/conch-cri.sock && echo "CRI socket OK"
```

## 3. CRI endpoint

`crictl` 可以直接指定 endpoint：

```bash
sudo crictl \
  --runtime-endpoint unix:///var/run/conchd/conch-cri.sock \
  --image-endpoint unix:///var/run/conchd/conch-cri.sock \
  info
```

kubelet 接入时需要把 runtime endpoint 指向 Conch CRI：

```text
--container-runtime-endpoint=unix:///var/run/conchd/conch-cri.sock
--image-service-endpoint=unix:///var/run/conchd/conch-cri.sock
```

## 4. PodSandbox annotation

CRI 入口通过 PodSandbox annotation 控制 Conch sandbox。支持字段如下：

| Annotation | 含义 | 默认值 |
| --- | --- | --- |
| `conch.io/sandbox-image` | Conch VM sandbox 镜像。 | `sandbox.default_image` |
| `conch.io/use-snapshot` | 是否把 `sandbox-image` 解析出的 rootfs snapshot 当作可恢复快照启动。`"false"` 为冷启动路径，`"true"` 为快照恢复路径。 | `false` |
| `conch.io/vmm-name` | VMM 名称。 | `sandbox.default_vmm_name` |
| `conch.io/vcpu` | vCPU 数量。 | `sandbox.default_vcpu_num` |
| `conch.io/vcpu-max` | 最大 vCPU 数量。 | `sandbox.default_vcpu_max` |
| `conch.io/ram-mb` | 内存大小，单位 MB。 | `sandbox.default_ram_mb` |

CRI annotation 不支持传本地 `snapshot-id`。snapshot ID 是单个 worker 节点上的 containerd 本地状态，写入 Pod 配置后无法跨节点调度。CRI 入口统一使用 `sandbox-image + use-snapshot`，由 Conch 在当前节点解析镜像对应的本地 snapshot。

`use-snapshot=true` 要求 `sandbox-image` 解包后的 rootfs snapshot 已带有可恢复所需的 mem/vm snapshot 关联标签。否则 sandbox 创建会失败。

## 5. crictl PodSandbox 示例

`crictl runp` 只触发 `RunPodSandbox`，适合验证 Conch VM sandbox 创建链路。

`pod.json`：

```json
{
  "metadata": {
    "name": "conch-smoke",
    "namespace": "default",
    "uid": "conch-smoke-uid",
    "attempt": 1
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

运行：

```bash
POD_ID=$(sudo crictl \
  --runtime-endpoint unix:///var/run/conchd/conch-cri.sock \
  runp pod.json)

sudo crictl --runtime-endpoint unix:///var/run/conchd/conch-cri.sock pods -a
sudo crictl --runtime-endpoint unix:///var/run/conchd/conch-cri.sock inspectp "${POD_ID}"
```

删除：

```bash
sudo crictl --runtime-endpoint unix:///var/run/conchd/conch-cri.sock stopp "${POD_ID}"
sudo crictl --runtime-endpoint unix:///var/run/conchd/conch-cri.sock rmp "${POD_ID}"
```

## 6. Kubernetes Pod YAML 示例

Kubernetes Pod 必须包含 `containers[].image`。当前 Conch CRI 不执行该业务容器镜像，因此该字段仅用于满足 Kubernetes API 和 kubelet CRI 调用流程。

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: conch-cri-smoke
  namespace: default
  annotations:
    conch.io/sandbox-image: "hub.oepkgs.net/conch/openeuler:odd-x86"
    conch.io/use-snapshot: "false"
    conch.io/vmm-name: "cloud-hypervisor"
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

说明：

- `containers[].image` 不是 Conch VM sandbox 镜像。
- Conch VM sandbox 镜像由 `conch.io/sandbox-image` 或 `sandbox.default_image` 决定。
- 通过 kubelet 创建 Pod 只能作为 CRI 链路 smoke test，不能用于验证 VM 内业务容器执行。

## 7. 关闭与重启恢复

conchd 关闭时默认保留 runtime 资源、state DB 和 runtime lease。下一次使用相同配置启动时，conchd 会执行 reconcile/rehydrate，重新接管可确认的运行态资源：

```bash
sudo ./bin/conchd --config ./config/config.yaml
```
