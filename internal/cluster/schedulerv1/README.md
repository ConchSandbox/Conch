# AgentENV Scheduler protocol

`scheduler.proto` is copied without modification from
[AgentENV](https://github.com/kvcache-ai/AgentENV/blob/1d742e4e149092be895f2c3cf0a097201229a250/services/api/proto/scheduler.proto),
commit `1d742e4e149092be895f2c3cf0a097201229a250`. The upstream MIT license is
included in `LICENSE`. The generated Go files derive from that protocol and
retain its `scheduler.v1` package, RPC paths, and message field numbers.

Regenerate from the repository root with `protoc` 3.21.12,
`protoc-gen-go` v1.36.11, and `protoc-gen-go-grpc` v1.6.2 on `PATH`:

```sh
protoc -I internal/cluster/schedulerv1 \
  --go_out=internal/cluster/schedulerv1 --go_opt=paths=source_relative \
  --go_opt=Mscheduler.proto=github.com/openeuler/Conch/internal/cluster/schedulerv1 \
  --go-grpc_out=internal/cluster/schedulerv1 --go-grpc_opt=paths=source_relative \
  --go-grpc_opt=Mscheduler.proto=github.com/openeuler/Conch/internal/cluster/schedulerv1 \
  internal/cluster/schedulerv1/scheduler.proto
```

The import mapping keeps the upstream source intact while generating the
client into Conch's Go module. Scheduler and Gateway need no changes.
