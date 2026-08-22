# Sandbox 生命周期 Webhook

conchd 可在 Sandbox 创建、主动删除或非预期退出后，将生命周期事件异步发送到已注册的 HTTP/HTTPS 回调地址。Webhook 配置仅保存在当前 conchd 进程内存中；conchd 重启后必须重新注册。

## 1. 注册 Webhook

conchd 默认通过 Unix socket 提供 API。以下示例中的 socket 路径请替换为 `server.work_dir/conchd.sock`。

```bash
curl --unix-socket /var/run/conch/conchd.sock \
  -X POST http://localhost/api/v1/events/webhooks \
  -H 'Content-Type: application/json' \
  -d '{
    "name": "reliability-service",
    "url": "https://reliability.example.com/conch/events",
    "events": [
      "sandbox.lifecycle.created",
      "sandbox.lifecycle.killed"
    ]
  }'
```

`name` 和 `url` 必填。`url` 必须是 HTTP 或 HTTPS 地址；`events` 省略时订阅全部当前支持的事件。

成功时返回 `201 Created`：

```json
{
  "webhook_id": "wh_0123456789abcdef0123456789abcdef",
  "name": "reliability-service",
  "url": "https://reliability.example.com/conch/events",
  "events": ["sandbox.lifecycle.created", "sandbox.lifecycle.killed"],
  "createdAt": "2026-08-21T10:20:30Z"
}
```

## 2. 查询和删除

查询当前 conchd 实例注册的全部 Webhook：

```bash
curl --unix-socket /var/run/conch/conchd.sock \
  http://localhost/api/v1/events/webhooks
```

成功时返回 `200 OK`：

```json
{
  "webhooks": [
    {
      "webhook_id": "wh_0123456789abcdef0123456789abcdef",
      "name": "reliability-service",
      "url": "https://reliability.example.com/conch/events",
      "events": ["sandbox.lifecycle.created", "sandbox.lifecycle.killed"],
      "createdAt": "2026-08-21T10:20:30Z"
    }
  ]
}
```

删除一个 Webhook：

```bash
curl --unix-socket /var/run/conch/conchd.sock \
  -X DELETE \
  http://localhost/api/v1/events/webhooks/wh_0123456789abcdef0123456789abcdef
```

删除成功时返回 `200 OK`：

```json
{
  "webhook_id": "wh_0123456789abcdef0123456789abcdef",
  "status": "deleted"
}
```

删除返回成功后，conchd 不会再为该 `webhook_id` 创建新的投递任务；已经开始的投递可以继续完成。

## 3. 事件载荷

回调地址会收到 `POST` 请求和 JSON 请求体：

```json
{
  "event_id": "evt_0123456789abcdef0123456789abcdef",
  "version": "v1",
  "type": "sandbox.lifecycle.created",
  "timestamp": "2026-08-21T10:20:30Z",
  "sandbox_id": "sandbox-001",
  "event_data": {
    "execution": {
      "created_at": "2026-08-21T10:20:30Z",
      "vcpu_num": 2,
      "ram_mb": 512
    }
  }
}
```

`sandbox.lifecycle.killed` 在通用的 `event_data.execution` 之外，还包含 `event_data.kill_reason`：主动删除为 `request`，Sandbox 非预期退出为 `orphaned`。

支持的事件与发送时机如下：

| 事件类型 | 发送时机 |
| --- | --- |
| `sandbox.lifecycle.created` | Sandbox 已创建成功，且状态已持久化为 `READY` 后。 |
| `sandbox.lifecycle.killed` | 主动删除完成后，`kill_reason` 为 `request`。 |
| `sandbox.lifecycle.killed` | Sandbox 非预期退出且状态已持久化为 `UNKNOWN` 后，`kill_reason` 为 `orphaned`。 |

## 4. 请求头、重试与幂等

每次投递包含以下请求头：

| 请求头 | 说明 |
| --- | --- |
| `Content-Type: application/json` | 事件正文的媒体类型。 |
| `conch-webhook-id` | 触发本次投递的 `webhook_id`。 |

一次逻辑事件的所有重试使用相同的 `event_id`。接收端应以 `event_id` 去重并实现幂等处理。

conchd 对每个匹配 Webhook 最多尝试投递 3 次。任意 2xx 响应均视为成功；网络错误、超时或非 2xx 响应均视为失败。投递为异步操作，不阻塞 Sandbox 生命周期操作。三次均失败时 conchd 记录错误日志，但不会保存事件或投递任务。

第一阶段不提供回调请求签名、投递 ID 或事件持久化。请将回调端部署在受控网络中，并保护 conchd 的 Unix socket 访问权限。
