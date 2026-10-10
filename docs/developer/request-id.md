# 请求 ID 与日志关联

conchd HTTP API 和 conch-init Agent API 使用同一请求 ID 约定。调用方无需提供请求 ID，已有 CLI 和 SDK 请求可以继续使用。

## HTTP 约定

| 输入 `X-Request-ID` | 行为 |
| --- | --- |
| 单个合法值 | 保留原值 |
| 缺失或空值 | 生成 UUID |
| 非法值、超长值或多个值 | 丢弃输入，生成 UUID；继续处理请求 |

合法值为 1–128 个 ASCII 字母、数字、`.`、`_` 或 `-`，区分大小写。不裁剪空白，不接受逗号列表。非法值不会作为请求 ID 写入日志或响应。

所有经 API 处理的响应都带 `X-Request-ID`，包括成功、业务错误、鉴权失败、路由 404、方法 405 和 Connect 流式响应。Connect 错误的元数据中也可读取该 header。响应 JSON 和 protobuf 消息结构保持不变。

例如，指定 ID 并查看响应头：

```bash
curl --unix-socket /var/run/conch/conchd.sock -i \
  -H 'X-Request-ID: client-operation-206' \
  http://localhost/api/v1/sandboxes
```

Agent RPC 同样允许添加该 header，例如：

```bash
curl -i -X POST "${AGENT_URL}/pb.ProcessService/List" \
  -H 'Connect-Protocol-Version: 1' \
  -H 'Content-Type: application/json' \
  -H "conch-init-token: ${AGENT_TOKEN}" \
  -H 'X-Request-ID: client-process-list-206' \
  -d '{}'
```

## 传播和日志

入口将 ID 放入私有类型键的请求上下文，已有的请求处理日志附加 `request_id` 字段。沙箱标识沿用既有日志格式：已有 `sandbox_id` 或 `sandboxId` 字段保持不变，guest 全局 logger 的 `sandboxId` 继续输出为 `[沙箱 ID]` 前缀。该功能保持原有日志条数和级别；原来没有日志的处理步骤继续只传递上下文。

host 到 guest 的 vsock 初始化请求携带可选 `request_id` JSON 字段，guest 在既有身份和就绪日志中附加该 ID，并在初始化响应中回传。guest 身份日志保留原有方括号前缀，每次 RPC 使用自己的请求 ID；不得把请求 ID 写入全局 logger。

异步工作保留来源关联：

- webhook 投递及重试使用来源 ID，并通过投递请求的 `X-Request-ID` 传给接收方；调用方取消不取消投递。
- 后台进程的既有告警和清理日志保留启动请求的 ID；之后的 Connect、List、SendSignal 请求各自使用自己的 ID。进程原有超时和取消行为保持不变。
- 沙箱退出清理沿用创建请求的 ID；由删除请求触发的清理使用删除请求的 ID。关联保存在内存中，daemon 重启后的恢复任务不具有历史请求 ID。
- singleflight 共享模板解析使用发起共享工作的请求 ID；其他等待请求保留各自的 ID。解析仍受服务生命周期和原有超时约束。

排障时使用响应中的 ID 搜索 conchd 和 guest 的日志：

```bash
rg -F 'request_id=client-operation-206' /var/log/conchd /path/to/guest/logs
```

该关联覆盖 Conch 中接受请求上下文的处理链路；外部 VMM、containerd 或内核自身输出不自动增加此字段。无来源请求的服务启动、网络池预热等任务不附加请求 ID。

## vsock 兼容性

初始化协议仍使用 version 1，可选字段省略时保持原有消息格式。新 guest 收到未带元数据的旧 host 请求时，响应省略 `request_id`，兼容旧 host 的严格 JSON 解码器。

旧 guest 会拒绝新增字段并在返回响应前关闭连接。新 host 遇到带元数据请求的响应 EOF 时，在后续重试中省略元数据；显式的初始化失败响应不会触发降级。host 侧保留请求 ID，旧 guest 的日志无法使用它关联。完整 guest 链路需要在模板中使用更新后的 conch-init。

请求 ID 是诊断关联标识，不是幂等键、沙箱 ID、身份凭证或 OpenTelemetry trace ID。调用方可以重复提供同一个 ID，这不改变操作执行次数；需要区分请求时，应为每次操作提供不同的 ID 或让服务生成。

## 开发约定

使用 `requestid.FromContext(ctx)` 读取 ID，使用 `ulog.WithContext(ctx)` 获取请求 logger。跨通信边界显式传递元数据；跨异步边界按原有生命周期保留来源 ID。长期任务仅保留必要的关联字段，避免持有整个 HTTP 请求上下文。

保留原有日志调用方式，有来源请求的日志通过 `WithContext` 附加请求 ID；网络池预热、启动清理等后台日志沿用 `ulog.GetLogger()`。`WithContext` 一次复制 logger 和上下文字段；上下文没有日志字段或仅重复绑定已有请求 ID 时直接复用原 logger。日志输出格式、writer 和原有同步策略保持不变。可通过 `go test ./pkg/ulog -run '^$' -bench '^BenchmarkRequestIDLogOverhead$' -benchmem` 测量分配与格式化开销；该基准不包含输出 I/O。
