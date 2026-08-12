# Issue #83：基于 channel 的 virtiofsd 退出监控与 Sandbox 清理

状态：Proposed
目标基线：`dev@a69ab02`
关联 Issue：<https://github.com/ConchSandbox/Conch/issues/83>

## 1. 文档目的

本文档只基于 Issue #83、当前 `dev` 代码和以下已确定方向设计：

1. `virtiofsd` 启动或接管成功后立即启动一个 goroutine，唯一地执行底层 `processHandle.Wait()`；child 实现调用 `cmd.Wait()`，adopted 实现调用 pidfd poll。
2. `Wait()` 返回后通过 channel 将“退出观察已经完成”这一事实通知 Sandbox Manager。
3. volume 层不判断正常退出或异常退出，也不接收 callback。
4. 正常或异常由 `sandboxEntry` 的内部生命周期状态判断。
5. 运行期异常退出复用现有 Sandbox cleanup，关闭整个 Sandbox。

本方案刻意保持控制面范围小：不设计 virtiofsd 在线重启，不增加 bbolt terminal 状态，不扩展 API/CLI。进程层必须同时覆盖 Conch 持有的 child process 和 adopted process，但两者通过同一个底层接口接入，不能把差异泄漏给 Sandbox Manager。

## 2. 当前代码事实与问题

### 2.1 当前调用链

```text
conchruntime.Service.CreateSandbox
  -> sandbox.Manager.Create
    -> volume.Manager.PrepareSandbox
      -> virtiofsBackend.Prepare
        -> cmd.Start
        -> waitUnixSocket
        -> procs.Store(*exec.Cmd)
    -> 启动 VMM / 等待 guest ready
    -> registerSandboxVolumeCleanup
    -> entry.state = sandboxReady
    -> trackSandbox 只等待 VMM
```

当前 `virtiofsBackend.Prepare` 在 `cmd.Start()` 后只等待 socket；直到 `Cleanup` 才调用 `Process.Wait()`。因此运行期间杀死 virtiofsd 后：

- 没有任何 goroutine 及时发现退出；
- Sandbox Manager 不会触发 Sandbox cleanup；
- VMM 和 guest 继续运行，guest 文件访问可能一直阻塞；
- 子进程没有及时回收，可能留下 zombie。

### 2.2 当前两种进程持有方式

从目标生命周期看，Conch 需要处理两类 virtiofsd：

- child process：由当前 conchd 通过 `exec.Cmd.Start()` 启动，conchd 是父进程，可以调用 `cmd.Wait()` 回收并取得退出状态；
- adopted process：进程不是当前 conchd 的 child，只能依靠持久化的 PID/start-time 身份和 pidfd 等内核句柄观察、发送信号与确认退出。

当前 `dev@a69ab02` 已明确实现 child 路径，并在 `Device.PID/StartTime`、`isOurVirtiofsd` 和 `os.FindProcess` 中存在非 child cleanup fallback；但这个 fallback 只负责尽量安全地 kill，并不是完整的 adopted runtime monitor。实现本方案时应把两者正式建模为两个适配器。

### 2.3 当前锁模型不能直接加 watcher

当前 `sandbox.Manager.Create` 从 `reserveSandboxEntry` 开始一直持有 `entry.mu`，直到 Create 返回。若新 watcher 收到退出后也必须拿 `entry.mu` 判断状态，它会在整个 VMM 启动阶段被阻塞，无法及时取消创建。

因此本方案不仅要加 Wait goroutine，还必须把 `entry.mu` 改为短临界区：锁只保护状态、指针和错误，不包围启动 VMM、等待 guest、kill、unmount 或完整 cleanup。

## 3. 核心语义

### 3.1 Wait 只报告事实

`processHandle.Wait()` 的返回值不能决定退出是否正常：

- Sandbox 已经 `READY` 时，virtiofsd 即使退出码为 0，也属于异常退出；
- Sandbox 已经进入 `STOPPING` 时，即使 `Wait()` 返回 SIGKILL，也属于主动 cleanup 导致的正常退出；
- `Cause`、exit code和 signal只用于日志和诊断，不参与生命周期分支。

唯一分类依据是同一个 `sandboxEntry` 在观察事件时的状态。

### 3.2 所有主动清理必须先改状态

用户 Delete、Create 失败回滚、VMM 退出清理等所有主动清理路径，都必须先在 `entry.mu` 下把状态改为 `STOPPING`，再解锁并 kill virtiofsd。

这样 monitor 通知到达时只需读取状态：

| 收到 virtiofsd 退出时的状态 | 分类 | 动作 |
| --- | --- | --- |
| `CREATING` | 异常，依赖在创建期死亡 | 保存原始错误，取消 Create，由 Create owner 回滚 |
| `READY` | 异常，运行期 backend 死亡 | 抢占 cleanup ownership，关闭 Sandbox |
| `SUSPENDED` | 异常，暂停不等于允许 backend 死亡 | 抢占 cleanup ownership，关闭 Sandbox |
| `STOPPING` | 正常/预期退出 | 不再发起 cleanup |
| `EXITED` 或 entry 已被替换/删除 | 已完成或旧事件 | 忽略 |

## 4. 底层进程接口与 virtiofsProcess

### 4.1 分层

```text
Sandbox Manager
  -> ProcessWatch.Done / Result
    -> virtiofsProcess
      -> processHandle interface
         |- childProcess
         `- adoptedProcess
```

职责边界：

- `processHandle` 只抽象操作系统进程的观察、kill、退出确认和句柄释放；
- `virtiofsProcess` 统一实现唯一 monitor、channel 通知和幂等 Close；
- Sandbox Manager 只观察 channel并根据 `sandboxEntry.state` 分类；
- 任何一层都不使用 callback。

### 4.2 processHandle 接口

接口应保持最小，但必须表达“观察结束”和“目标进程确实退出”的区别：

```go
type processWaitResult struct {
    Exited   bool
    Cause    error
    ExitCode *int
    Signal   string
}

type processHandle interface {
    PID() int
    StartTime() uint64

    // Wait 由 virtiofsProcess 的唯一 monitor 调用一次。
    Wait() processWaitResult

    // Kill 必须针对同一个进程身份，重复或进程已退出可安全收敛。
    Kill() error

    // ConfirmExit 不得再次调用 Wait；它在 timeout 内确认目标已退出，
    // 并且必须允许与尚未返回的 Wait 并发。
    ConfirmExit(timeout time.Duration) error

    // Close 释放 pidfd 等底层句柄，不负责 Sandbox 生命周期判断。
    Close() error
}
```

`Wait() error` 不够，因为：

- child 的 `cmd.Wait()` 返回 `*exec.ExitError` 时，进程已经退出，应该是 `Exited=true, Cause=exitErr`；
- adopted 的 pidfd poll 返回系统调用错误时，可能只是观察器失败，目标进程仍活着，应该是 `Exited=false, Cause=pollErr`。

`Exited` 只用于 `virtiofsProcess.Close()` 决定是否还要 kill/confirm；它不能决定 Sandbox 退出是正常还是异常。

### 4.3 childProcess

```go
type childProcess struct {
    cmd       *exec.Cmd
    pid       int
    startTime uint64

    waitDone chan struct{}
    waitOnce sync.Once
    result   processWaitResult
}
```

契约：

- 只在 `cmd.Start()` 成功后构造；
- `Wait()` 是唯一调用 `cmd.Wait()` 的位置；
- `cmd.Wait()` 的任何正常返回路径都表示 `Exited=true`；
- `Cause` 保存 nil 或 `exec.ExitError`，退出码/信号仅用于诊断；
- `Kill()` 使用 `cmd.Process.Kill()`，忽略 `os.ErrProcessDone/ESRCH`；
- `ConfirmExit()` 只等待 child 自己的 `waitDone`，绝不第二次调用 `cmd.Wait()`；
- `Close()` 为 no-op。

### 4.4 adoptedProcess

```go
type adoptedProcess struct {
    pid       int
    startTime uint64
    pidfd     int
    closeOnce sync.Once
}
```

构造 adopted handle 时必须绑定进程身份：

1. 校验 `PID > 0` 且 `StartTime != 0`；
2. 读取 `/proc/<pid>/stat` 并与记录的 start-time 比较；
3. 调用 `pidfd_open(pid, 0)`；
4. 再读取一次 start-time并比较；
5. 任一次不一致都关闭 pidfd并返回“原进程已退出或 PID 已复用”，不得继续 adoption。

契约：

- `Wait()` 使用 `poll(pidfd)` 等待退出；
- pidfd ready 表示 `Exited=true`，但通常拿不到可靠的 exit code/signal；
- poll 本身失败表示 `Exited=false, Cause=err`；
- `Kill()` 使用 `pidfd_send_signal(SIGKILL)`，不能在已拿到 pidfd 后退回裸 PID kill；
- `ConfirmExit(timeout)` 再次有界 poll pidfd；
- `Close()` 幂等关闭 pidfd；
- pidfd 不可用时应明确返回 adoption 失败，由上层决定是否拒绝恢复或走 stale cleanup，不能悄悄降级成存在 PID 复用窗口的长期 watcher。

### 4.5 virtiofsProcess

```go
type virtiofsProcess struct {
    sandboxID string
    handle    processHandle

    observeDone chan struct{}

    mu          sync.Mutex
    observation ProcessObservation
    observed    bool

    stopOnce sync.Once
    stopErr  error
}
```

`virtiofsProcess` 不保存 `*exec.Cmd`、pidfd 或 adopted bool，也不对 `processHandle` 做 type switch。所有底层差异只能存在于 child/adopted 适配器。

### 4.6 对外 channel 结果

```go
type ProcessObservation struct {
    PID       int
    StartTime uint64
    Exited    bool
    Cause     error
    ExitCode  *int
    Signal    string
    ObservedAt time.Time
}

type ProcessWatch struct {
    process *virtiofsProcess
}

func (w *ProcessWatch) Done() <-chan struct{}
func (w *ProcessWatch) Result() (ProcessObservation, bool)

type PreparedSandbox struct {
    Devices []Device
    Watch   *ProcessWatch
}
```

不要让 `Prepare` 和 Sandbox Manager 竞争读取同一个 `chan ProcessObservation`。普通 channel 中的一个值只能被一个消费者取走，而这两个位置都必须能观察 monitor 完成。

正确契约是：monitor 先保存不可变 Result，再关闭 `observeDone`。channel close 向全部观察者广播；调用者在 `<-Done()` 后读取 Result。Result 中 `Exited=false` 也必须通知外层，因为失去 adopted process 的可靠观察能力本身就是运行期故障。

`PreparedSandbox.Watch` 还承担 active process identity：即使 monitor 已从 backend map 删除记录，Sandbox cleanup 仍通过这个 Watch 找到同一个 `virtiofsProcess`，从而让 adopted observer failure 走原 pidfd 的 `Kill + ConfirmExit`。active cleanup 不能退回只凭 sandboxID重新查 map。

### 4.7 统一 monitor

```go
func (b *virtiofsBackend) monitorProcess(p *virtiofsProcess) {
    wait := p.handle.Wait()

    p.mu.Lock()
    p.observation = ProcessObservation{
        PID:        p.handle.PID(),
        StartTime:  p.handle.StartTime(),
        Exited:     wait.Exited,
        Cause:      wait.Cause,
        ExitCode:   wait.ExitCode,
        Signal:     wait.Signal,
        ObservedAt: time.Now(),
    }
    p.observed = true
    p.mu.Unlock()

    b.procs.CompareAndDelete(p.sandboxID, p)
    close(p.observeDone)
}
```

无论底层 Wait 报告“进程已退出”还是“观察器失败”，monitor 都关闭 channel。Sandbox Manager 收到后仍只依据 sandboxEntry 状态决定正常/异常。这里允许从 backend map 删除，是因为当前 active owner 已由 `PreparedSandbox.Watch` 精确持有；不得因此丢失底层 handle。

### 4.8 统一 Close

`virtiofsProcess.Close()` 是 strict-once，建议流程：

1. 若已有 observation 且 `Exited=true`，无需 kill；
2. 否则调用 `handle.Kill()`；
3. 有界等待 monitor 的 `observeDone`；
4. 若 observation 未完成或结果为 `Exited=false`，调用 `handle.ConfirmExit(timeout)`；
5. ConfirmExit 后再次有界等待 monitor 收尾；
6. 若 observer 仍不返回，记录 monitor shutdown error，并以 `handle.Close()` 作为最终解除 poll 的手段；
7. 确认或 observer 收尾失败都保留错误，但继续安全的文件/unmount cleanup；
8. 并发或重复 Close 返回同一个缓存结果。

硬约束：

- `virtiofsProcess` 不能再次调用底层 Wait；
- child ConfirmExit 不能再次调用 `cmd.Wait()`；
- adopted observer failure 不能被伪装成进程已经退出；
- 不允许无限等待 observer或进程退出；
- Close 不判断 expected/unexpected，调用它之前 Sandbox Manager 必须先进入 STOPPING。

### 4.9 Prepare 返回值

建议 volume Manager 接口：

```go
func (m *Manager) PrepareSandbox(
    sandboxID string,
    mounts []Mount,
) (PreparedSandbox, error)

func (m *Manager) CleanupSandbox(
    sandboxID string,
    prepared PreparedSandbox,
) error
```

没有 volume mount 时返回零值 `PreparedSandbox`，`Watch == nil`。child 路径从刚启动的 `exec.Cmd` 创建 handle；adopted 路径从持久化的 Device 创建 handle，然后同样包装成 `virtiofsProcess + ProcessWatch`。

active cleanup 接收完整 `PreparedSandbox` 并调用其中精确 process 的 Close，再清理 bind mount/runtime dir。没有 active owner 的 daemon 启动残局继续走独立的 stale cleanup；两条路径不能混成按 sandboxID猜测进程身份的一个函数。

## 5. Prepare 阶段状态与竞态

Prepare 内部需要覆盖以下阶段：

```text
资源准备
  -> cmd.Start 失败
  -> monitoring / 等待 socket
  -> socket observed
  -> Prepare 返回给 Sandbox Manager
```

### 5.1 Start 失败

`cmd.Start()` 失败时尚无可等待的子进程：清理 bind mount/runtime dir，直接返回错误，不创建 monitor。

### 5.2 Start 成功后立即 monitor

本节只描述新启动的 child 路径：先用 `exec.Cmd` 构造 `childProcess`，再把它交给统一的 `virtiofsProcess`。后续 Prepare、channel watcher和 Sandbox Manager 都不再接触 `exec.Cmd`。

顺序必须是：

1. 创建 process 对象；
2. 存入 `procs`；
3. 启动 monitor goroutine；
4. 等待 socket。

不能等 socket ready 后才启动 monitor，否则进程可能在等待期间退出且无人回收。

### 5.3 等待 socket 时进程退出

`waitUnixSocket` 必须同时观察 socket、超时和 `process.Done()`：

```go
select {
case <-process.Done():
    result, _ := process.Result()
    return startupExitError(result)
case <-ticker.C:
    // stat socket
case <-timer.C:
    return socketTimeout
}
```

此时 `PreparedSandbox` 尚未返回，外部还没有 watcher owner，所以由 Prepare 自己清理 bind mount/runtime dir并返回创建错误。进程已经由 monitor 回收，不能再 Wait。

### 5.4 socket 超时或 Prepare 自身失败

Prepare 主动失败时：

1. 向进程发送 kill；
2. 等待 Done；
3. 清理 bind mount/runtime dir；
4. 返回原始错误与 cleanup error。

这属于创建函数内部回滚，不产生运行期异常通知。

### 5.5 socket ready 与退出并发

看到 socket 后必须进行一次最终存活检查。推荐在 process mutex 下执行 `markPrepared()`：

- 若 monitor 已保存退出结果，Prepare 返回失败；
- 若尚未退出，允许 Prepare 返回；
- 如果进程在 `markPrepared()` 后、函数返回前退出，Done 已关闭或即将关闭。Sandbox Manager 拿到 `PreparedSandbox` 后启动 watcher，会立即观察该事件；此时 entry 仍是 `CREATING`。

这里不要求 volume 层判断正常或异常，只要求不丢退出事实。

### 5.6 adopted 接入阶段

adopted process 不执行 cmd.Start或等待新 socket；它从 Device 中恢复 PID/start-time和已有 socket/runtime 信息。推荐顺序：

1. entry 保持 `CREATING` 或等价的尚未发布状态；
2. 构造 `adoptedProcess` 并完成两次身份校验；
3. 包装为 `virtiofsProcess` 并立即启动统一 monitor；
4. 立即启动 Sandbox Manager channel watcher；
5. 完成 VMM/Sandbox 其他 ownership 恢复；
6. 最后 commit Ready。

若 adopted monitor 在 commit Ready 前完成，处理方式与 child 创建期退出相同：记录 dependencyErr、取消恢复、由恢复流程 owner 清理。不能先发布 Ready 再补 watcher。

## 6. Sandbox Manager 状态机

### 6.1 sandboxEntry

```go
type sandboxLifecycleState uint8

const (
    sandboxCreating sandboxLifecycleState = iota
    sandboxReady
    sandboxSuspended
    sandboxStopping
    sandboxExited
)

type sandboxEntry struct {
    mu    sync.Mutex
    state sandboxLifecycleState
    sbx   *Sandbox

    dependencyErr error

    cleanupDone chan struct{}
    cleanupErr  error
}
```

`reserveSandboxEntry` 初始化为 `CREATING`，但不把 mutex 一直交给 Create 持有。所有长操作都在锁外完成。

### 6.2 创建期 ownership

Create 是所有未发布资源的 owner。推荐顺序：

1. reserve entry，状态为 `CREATING`；
2. 创建可取消的 create context；
3. 准备 boot、CID；
4. Prepare volume；
5. 立即启动 volume watcher；
6. 启动 VMM并等待 guest ready；
7. 把 volume cleanup 注册到 `sbx.cleanup`；
8. 在 `entry.mu` 下执行 `commitReady`；
9. 只有 `dependencyErr == nil` 时设置 `entry.sbx` 和 `READY`。

若第 4 步之后任何步骤失败，Create defer 必须先把 entry 改为 `STOPPING`，再清理 volume 和其他资源。

不要继续依赖当前多个分散 defer 的偶然执行顺序。建议明确记录 volume ownership：

```go
prepared := volume.PreparedSandbox{}
volumeOwnedByCreate := false
var sbx *Sandbox

defer func() {
    if err == nil {
        return
    }
    markCreatingEntryStopping(entry)
    if sbx != nil {
        // 已转交给 sbx.cleanup 的 volume 会在这里清理。
        err = errors.Join(err, sbx.Close(context.Background()))
    }
    if volumeOwnedByCreate {
        err = errors.Join(err, m.volumeManager.CleanupSandbox(
            req.SandboxID,
            prepared,
        ))
    }
}()
```

Prepare 成功后设置 `volumeOwnedByCreate=true`；注册进 `sbx.cleanup` 的 closure 必须捕获完整 `prepared`，只有注册成功后才改为 false。这样无论异常发生在 VMM 创建前、VMM 创建中还是 cleanup 注册前，都不会丢失或重复清理 volume，也不会把 adopted cleanup 降级为裸 PID fallback。

### 6.3 channel watcher

```go
func (m *Manager) watchVolumeProcess(
    mapKey string,
    entry *sandboxEntry,
    watch *volume.ProcessWatch,
    cancelCreate context.CancelCauseFunc,
) {
    if watch == nil {
        return
    }
    go func() {
        <-watch.Done()
        result, ok := watch.Result()
        if !ok {
            return
        }
        m.handleVolumeProcessObservation(mapKey, entry, result, cancelCreate)
    }()
}
```

这是单向 channel 消费，没有任何 volume -> sandbox callback。watcher 生命周期最多持续到 virtiofsd 退出。

### 6.4 创建期退出

handler 在锁内看到 `CREATING` 时：

1. 验证 map 中仍是同一个 entry；
2. 若 `dependencyErr` 为空，保存 `virtiofsd exited during create`，附带 Cause 仅作诊断；
3. 复制 cancel 函数；
4. 解锁；
5. 使用原始 dependency error 取消 create context。

必须先记录 `dependencyErr` 再 cancel，避免 VMM/guest wait 返回 `context canceled` 后覆盖真正根因。

handler 不直接清理创建了一半的 Sandbox；Create defer 才拥有这些局部资源。

### 6.5 运行期退出

handler 在锁内看到 `READY` 或 `SUSPENDED` 时：

1. 验证 entry identity；
2. 将状态改为 `STOPPING`，这一步同时取得唯一 cleanup ownership；
3. 复制 `sbx`；
4. 解锁；
5. 调用现有 `cleanupSandbox`；
6. 保存 cleanupErr、改为 `EXITED`、关闭 `cleanupDone`；
7. `CompareAndDelete` 删除当前 entry。

cleanup 期间不得持有 `entry.mu`，否则正常 kill virtiofsd 后，channel watcher或并发 Delete 会死锁。

### 6.6 正常退出

Delete、Create rollback、VMM exit owner 在调用 `cleanupSandbox` 前已经把 entry 改为 `STOPPING`。virtiofsd 被 kill 后，monitor 仍然发布退出事实，但 channel handler 看到 `STOPPING` 后直接返回。

因此不需要猜测 SIGTERM、SIGKILL 或 exit code，也不需要 volume 层的 stopping flag来分类。

## 7. 多出口 cleanup ownership

Volume exit、VMM exit 和用户 Delete 可能并发。它们必须复用一个短锁 helper：

```go
func beginStopping(entry *sandboxEntry) (
    sbx *Sandbox,
    done <-chan struct{},
    owner bool,
)
```

语义：

- `READY/SUSPENDED -> STOPPING` 的调用者得到 `owner=true`；
- 已经 `STOPPING` 的调用者得到现有 `cleanupDone`，不得重复 cleanup；
- `EXITED` 返回已完成结果；
- owner 在锁外执行 cleanup，最后调用 `finishStopping`；
- 用户 Delete 若遇到已有 owner，可等待 `cleanupDone` 并返回同一 cleanup 结果；
- 后台 VMM/volume watcher 若遇到已有 owner，直接退出即可。

当前 `handleSandboxExit` 和 `Delete` 都在持有 `entry.mu` 时执行完整 cleanup，必须一并改掉，否则新增 channel watcher 会放大死锁风险。

## 8. 完整事件链

### 8.1 正常创建

```text
entry=CREATING
  -> Start virtiofsd
  -> start Wait goroutine
  -> socket ready
  -> return PreparedSandbox
  -> start channel watcher
  -> start VMM
  -> register volume cleanup
  -> commit entry=READY
```

### 8.2 运行期被 kill -9

```text
cmd.Wait returns ExitError(SIGKILL)
  -> monitor stores ProcessObservation
  -> close Done channel
  -> Sandbox watcher wakes
  -> entry is READY
  -> entry=STOPPING, watcher wins cleanup ownership
  -> Sandbox.Close
       -> stop VMM
       -> volume Cleanup sees process already reaped
       -> unmount/remove runtime resources
       -> release network
  -> release boot/CID
  -> entry=EXITED and remove from map
```

### 8.3 用户 Delete

```text
Delete locks entry
  -> READY/SUSPENDED -> STOPPING
  -> unlock
  -> Sandbox.Close kills VMM and virtiofsd
  -> virtiofsd monitor closes Done
  -> watcher sees STOPPING and does nothing
  -> Delete owner completes cleanup
```

### 8.4 创建期退出

```text
entry=CREATING
  -> virtiofsd Done closes
  -> watcher stores dependencyErr
  -> cancel Create
  -> VMM/guest wait returns
  -> Create reads dependencyErr as primary error
  -> entry=STOPPING
  -> Create owner rolls back
  -> remove entry
```

### 8.5 adopted observer 失败

```text
adoptedProcess.poll(pidfd) returns error
  -> processWaitResult{Exited:false, Cause:pollErr}
  -> virtiofsProcess stores observation and closes Done
  -> Sandbox watcher reads sandboxEntry state
  -> READY/SUSPENDED means abnormal and claims cleanup
  -> virtiofsProcess.Close calls pidfd Kill
  -> ConfirmExit(timeout)
  -> cleanup continues even if confirmation reports an error
```

这里触发 cleanup 的原因不是 Cause 内容，而是“channel 在 Sandbox 仍应运行时完成”。`Exited=false` 只告诉底层 Close：不能假装目标已经死亡。

## 9. 文件级修改清单

| 文件 | 修改 |
| --- | --- |
| `internal/volume/types.go` | 增加 `ProcessObservation`、`ProcessWatch`、`PreparedSandbox`；不增加 callback |
| `internal/volume/process.go` | `processHandle`、`processWaitResult` 与统一 `virtiofsProcess` |
| `internal/volume/process_child.go` | 基于 `exec.Cmd` 的 childProcess |
| `internal/volume/process_adopted_linux.go` | PID/start-time校验、pidfd poll/signal的 adoptedProcess |
| `internal/volume/manager.go` | Prepare/Adopt 返回 `PreparedSandbox`；active Cleanup消费完整 owner |
| `internal/volume/virtiofs.go` | child 启动、socket/exit 竞争、资源目录和 backend map |
| `internal/volume/process_test.go` | 统一 wrapper和 child adapter测试 |
| `internal/volume/process_adopted_linux_test.go` | adopted identity、poll failure、kill/confirm测试 |
| `internal/volume/virtiofs_test.go` | helper 子进程与 Prepare/cleanup测试 |
| `internal/sandbox/manager.go` | `CREATING/STOPPING/EXITED`、短锁、channel watcher、dependencyErr、公共 cleanup ownership |
| `internal/sandbox/manager_test.go` | 状态分类、创建竞态和多出口 cleanup 测试 |
| `docs/developer/volume.md` | 记录 channel 监控与 fail-close 策略 |

本方案不要求修改：

- `internal/daemon/state`；
- `internal/conchruntime` 的持久化模型；
- bbolt terminal 状态、daemon API 和 CLI。

## 10. 实施顺序

1. 定义 `processHandle` 和 `processWaitResult{Exited, Cause}` 契约。
2. 实现 childProcess并证明只有一次 `cmd.Wait()`。
3. 实现 adoptedProcess的身份校验、pidfd Wait/Kill/ConfirmExit。
4. 在两个 adapter之上实现统一 `virtiofsProcess`、Done/Result和 strict-once Close。
5. 让 `waitUnixSocket` 同时观察 process Done，补齐 Prepare 阶段测试。
6. 将 volume Prepare/Adopt 返回值统一成 `PreparedSandbox`。
7. 将 `sandboxEntry` 改为 `CREATING/READY/SUSPENDED/STOPPING/EXITED`。
8. 缩短 Create、Delete、VMM exit 对 `entry.mu` 的持锁范围。
9. 启动 channel watcher并实现创建期 dependencyErr/cancel。
10. 统一 volume exit、VMM exit、Delete 的 cleanup ownership。
11. 补 adapter、wrapper、Sandbox race test和真实 VOL-011。

## 11. 单元测试矩阵

### 11.1 Process adapters

- child 的 Wait返回 nil或 ExitError时都设置 `Exited=true`。
- child Kill/ConfirmExit不产生第二次 `cmd.Wait()`。
- adopted pidfd ready设置 `Exited=true`。
- adopted poll错误设置 `Exited=false, Cause!=nil`。
- adopted 构造前后 start-time变化会拒绝 adoption。
- adopted Kill只使用绑定的 pidfd，不误杀复用 PID。
- adopted ConfirmExit超时返回错误且不无限等待。
- 两个 adapter都满足同一接口，不需要上层 type switch。

### 11.2 Volume

- `cmd.Start` 失败：没有 monitor，资源被清理。
- 进程在 socket 前退出：Prepare 立即失败，进程已回收。
- socket 出现与退出并发：不能丢 Done，不能返回假健康结果。
- Prepare 返回后进程退出：Done 对外可观察，Result 正确。
- `Cause == nil` 仍发布退出事实。
- 正常 Cleanup：只 kill，不二次调用 Wait。
- 重复 Cleanup：不会重复 kill或等待。
- monitor 与 Cleanup 并发：无 panic、无 double Wait、无死锁。
- 旧 monitor 完成时不能删除同 ID 的新 process map entry。
- monitor 已删除 map entry后，active cleanup仍通过 PreparedSandbox关闭同一个底层 handle。

### 11.3 Sandbox Manager

- `CREATING + process exit`：记录 dependencyErr并取消 Create。
- dependencyErr 在 context canceled 之前成为主错误。
- `READY + Cause nil`：仍然触发 Sandbox cleanup。
- `READY + ExitError`：触发一次 cleanup。
- `SUSPENDED + exit`：触发一次 cleanup。
- `STOPPING + ExitError(SIGKILL)`：不重复 cleanup。
- Delete 与 volume exit 并发：只有一个 owner。
- VMM exit 与 volume exit 并发：只有一个 owner。
- channel 事件来自已经替换的旧 entry：不得清理新 Sandbox。
- cleanup 执行期间不持有 `entry.mu`。
- watcher 在 Prepare 返回时 Done 已关闭：仍能立即处理创建失败。

建议运行：

```bash
go test -count=1 ./internal/volume ./internal/sandbox
go test -race -count=1 ./internal/volume ./internal/sandbox
```

## 12. VOL-011 验收

最终必须在 Linux/KVM 真实 guest 环境执行：

1. 创建带 volume mount 的 Sandbox。
2. 精确找到该 Sandbox 对应的 virtiofsd PID。
3. 执行 `kill -9 <pid>`，不能使用宽泛的 `pkill virtiofsd`。
4. 验证 monitor 很快关闭 Done，Sandbox Manager 观察时状态为 READY。
5. 验证只有一个 cleanup owner。
6. 验证 VMM/guest 被关闭，原先阻塞的 guest 工作负载不再无限存在。
7. 验证 virtiofsd 已被 Wait 回收，不是 zombie。
8. 验证 volume bind、socket、runtime dir、network、boot 和 CID 被清理。
9. 验证正常 Delete 不会被记录或日志误判为异常退出。

Issue 指向的 `scripts/volume-vmm/run_volume_failure_chapter.sh` 位于外部测试仓，当前 Conch 仓库没有该脚本。helper-process 单测和 macOS 静态检查不能替代真实 VOL-011。

## 13. 明确非目标与后续风险

- 本次不尝试重启 virtiofsd或恢复 guest mount。
- 本次不持久化 EXITED/退出原因；异常原因先进入结构化日志。
- adopted process 的接入限定为进程观察/终止抽象；完整的 daemon 恢复策略仍由现有恢复流程决定。
- 本次不通过 Cause 推断 expected/unexpected。
- 本次不使用 callback、health callback或 failure callback。
- 当前 `vmm.Process.Stop` 存在无界等待 `exitSignal` 的风险；VOL-011 若证明这里会卡住，应单独增加 SIGTERM timeout和 SIGKILL escalation，但不要把它与本次进程通知协议耦合。
- 当前 bbolt 记录可能仍保持 READY，这是既有的控制面状态一致性问题；若产品要求异常关闭后 API 可查询 terminal 状态，应另立持久化设计，不应塞进本次最小修复。

## 14. 完成标准

- [ ] childProcess和 adoptedProcess实现同一个最小 processHandle接口。
- [ ] virtiofsProcess和 Sandbox Manager没有 adopted/child type switch。
- [ ] child只有一次 cmd.Wait，adopted observer失败不会伪装为进程退出。
- [ ] virtiofsd 从 Start或 Adopt成功起始终有唯一 Wait/observe goroutine。
- [ ] Prepare 的 start/socket/exit 三方竞态均不会丢事件或产生 zombie。
- [ ] volume 与 sandbox 之间只有 channel，没有退出 callback。
- [ ] Cause 只用于诊断，正常/异常只由 sandboxEntry 状态判断。
- [ ] 所有主动清理在 kill virtiofsd 前先进入 STOPPING。
- [ ] 创建期退出能取消 Create并返回真实 dependency error。
- [ ] 运行期退出能关闭整个 Sandbox。
- [ ] Delete、VMM exit、volume exit 并发时只执行一次 cleanup。
- [ ] cleanup期间不持有 entry.mu，不发生自等待死锁。
- [ ] 正常 Delete 不被误判为异常退出。
- [ ] targeted unit/race tests通过。
- [ ] Linux/KVM VOL-011通过。
