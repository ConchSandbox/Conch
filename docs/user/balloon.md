# 沙箱内存 Balloon

Balloon 允许在不修改沙箱配置内存的前提下，动态减少或恢复 guest 可使用的内存。调用时只需指定希望留给 guest 多少内存，Conch 会将其转换为 StratoVirt 或 Cloud Hypervisor 对应的参数。

查询返回的“实际内存”表示 VMM 报告的 guest 当前有效内存，不是 VMM 进程在主机上的物理内存占用（RSS）。因此，guest 可用内存减少了多少，并不意味着主机就立即回收了同样大小的物理内存。

## 创建时启用

创建沙箱时需传入 `enable_balloon: true`，默认值为 `false`。该选项决定 VMM 是否挂载 virtio-balloon 设备，创建后不能修改；禁用时，调整和查询接口都会直接返回失败，不会调用 VMM。

```python
from conch import Sandbox

sbx = Sandbox.create(template_name="my-template", ram_mb=4096, enable_balloon=True)
try:
    print(sbx.set_memory_target(target_mb=1024, timeout=20))
    print(sbx.get_balloon_info())
    print(sbx.set_memory_target(target_mb=4096, timeout=20))
finally:
    sbx.delete()
```

Conch 为 StratoVirt 添加 PCI balloon 设备，为 Cloud Hypervisor 添加 `--balloon size=0`。Guest 内核需要启用或加载 virtio_balloon 驱动，否则即使命令被接受，内存调整也可能无法生效。

## 调整与查询

```http
PUT /api/v1/sandboxes/{sandboxID}/balloon
Content-Type: application/json

{"target_mb": 1024, "timeout_seconds": 20}
```

当前 Balloon 请求只允许 READY 沙箱操作，不能通过此接口热添加设备或把内存扩展到原有 RAM 以上。
`target_mb` 必须是正整数且不大于配置 RAM，`timeout_seconds` 是 1 到 60 的整数，省略时使用 20 秒。
超时从调用开始计时，但等待生命周期锁期间不能中断；获得锁时若已超时，就直接返回错误，不提交命令。

PUT 在 VMM 接受目标后查询一次当前内存并返回，不等待 guest 完成调整。返回 200 表示命令已被接受且查询成功，因此 `actual_mb` 可以与 `target_mb` 不同；如需确认后续变化，可调用 GET 再次查询。

```json
{"configured_mb": 4096, "target_mb": 1024, "actual_mb": 4096}
```

GET 同一路径读取当前状态而不改变目标；Python 对应 `get_balloon_info()`。`actual_mb` 可以有小数，`target_mb` 是 Conch 最后确认提交的目标，恢复后或命令结果不确定时可能为 `null`。SDK 的 PUT HTTP 超时比服务端操作期限多 5 秒，GET 为 25 秒，以便接收正常的服务端错误响应。

## 失败与并发

| 结果 | 含义 |
| --- | --- |
| 400 | 缺少目标、目标越界或 timeout 参数无效，未提交调整命令 |
| 409 | 未启用设备、非 READY 状态，或 VMM 运行时不可用 |
| 500 | VMM 命令或状态查询失败，详细原因见 conchd 日志；不能据此断言驱动缺失 |
| 504 | 请求超时，或提交目标、查询内存时通信超时；已接受的目标仍可能继续生效 |

超时、客户端断连和状态查询失败都不会自动回滚目标。命令提交时若发生通信错误，也可能无法判断 VMM 是否已接受，故应在环境恢复后 GET 查询再根据实际情况决定是否重试。

同一沙箱的调整、查询、删除、暂停和 checkpoint 使用同一个生命周期锁，不会同时执行，但也不会阻塞其他沙箱的操作。等待锁可能使响应晚于设定的超时时间；等待较长时，SDK 可能先返回客户端超时。

## Checkpoint 与恢复

Checkpoint 的 Boot Index 记录是否启用了 virtio-balloon 设备，恢复时沿用捕获的设备布局及 balloon 状态而不是将其重置。
按当前语义，不能在恢复时通过 `enable_balloon=true` 强行添加 virtio-balloon 设备。旧快照未包含 balloon 设备时仍可正常恢复，但不提供 balloon 功能；若恢复时显式要求 `enable_balloon=true` 则返回错误。

在使用 `pause(memory=false)` 暂停时由于没有 VMM 状态可恢复，重新冷启动时仍启用原设备且启动后的有效内存回到初始配置 RAM。

从快照恢复沙箱后，`target_mb` 为 null表示尚未记录新的内存调整目标,`actual_mb` 从当前 VMM 查询，而`configured_mb` 为沙箱配置项，保留了沙箱原先配置的内存大小。

## 约束

- 内存调整需要 guest 的 virtio_balloon 驱动配合，但 Conch 不会在启动前检查镜像是否包含该驱动，部署时需自行确认。
- 当前不启用 OOM 自动放气：guest 内存不足时不会通过前者自动取回 balloon 占用的内存，也不会通过后者主动报告空闲页供主机回收，可作为后续演进方向考虑。
- VMM 接受调整命令后，guest 仍需要时间完成调整，PUT 不等待调整完成，也不会因实际内存尚未达到目标而报错。验证功能时，应设置一个与当前实际内存不同的目标，再通过 GET 确认实际内存随之变化；提交后的单次查询失败仍会使 PUT 返回错误，但不会撤回目标。
- Conch 有配置项去设定沙箱占用的内存配额并在沙箱启动、销毁时进行配额分配及回收管理，balloon 调整不会改变此配额。例如，配置为 4 GiB 的沙箱即使暂时调到 2 GiB，仍按 4 GiB 计入 conch 配额，为之后恢复内存保留预算，但主机的实际可用物理内存可能与配额存在差异。
