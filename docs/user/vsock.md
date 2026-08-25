# VSOCK 通信

Conch 为每个 Sandbox 挂载 virtio-vsock，guest 进程可以通过 `AF_VSOCK` stream
socket 与 host 进程通信。该通道不经过 guest 的 CNI/IP 网络。

VSOCK endpoint 使用 `(CID, port)` 地址。Host CID 固定为 `2`；guest 连接
`(2, port)` 后，请求会由 host 上监听相同 AF_VSOCK port 的进程接受，连接建立后
双方可以交换字节流。

## 用法

以下示例适用于 StratoVirt，并使用 `19090` 作为示例 port。Host 先启动 listener：

```bash
socat -u VSOCK-LISTEN:19090,fork -
```

Guest 使用 host CID `2` 主动连接：

```bash
printf 'hello from guest\n' | socat -u - VSOCK-CONNECT:2:19090
```

连接成功后，host 终端会输出 `hello from guest`。示例要求两端的 socat 均包含
VSOCK 支持。

## 注意事项

- Port `4065` 由 Conch 初始化协议保留，应用不得使用。
- `allowOut`、`denyOut` 和 `allow_internet_access` 等 CNI/IP 网络策略不限制 VSOCK。
- 仅 StratoVirt 后端支持上述 guest → host VSOCK 用法，Cloud Hypervisor 后端不支持。
