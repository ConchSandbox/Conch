"""Conch AgentENV E2B 单机全量接口测试（SDK 直连单节点，无 Gateway/Scheduler）。

本脚本面向单机部署：只启动一个
conchd，SDK 通过 e2b.listen_addr 直接访问 Node API，控制面与数据面共用同一监听
地址（E2B_SANDBOX_URL=E2B_API_URL，SDK commands/files/pty 经 E2b-Sandbox-Id/
E2b-Sandbox-Port 头路由）。覆盖：

  1. 控制面 (Node REST)：/health(200)、/sandboxes CRUD、鉴权、
     分页 (limit/nextToken/order/游标顺序校验)、metadata 过滤（重复键
     last-value-wins）、template/startedAfter/state 过滤、X-Total-Running、
     /v2/sandboxes 列表别名、错误码（含 413 超限请求体）、无 /nodes(404)、
     未路由端点（v2 create/v2 connect、logs、sandboxes/metrics → 404）、
     显式 501（/templates、/volumes、fork/resume/get_metrics/secure/mcp）、
     未知沙箱各操作 404、checkpoint 快照（指定名/自动名/同名更新/list_snapshots
     分页与名称映射、delete_snapshot 501）、get_info envd_version 回读、
     pause/connect/set_timeout/refreshes/update_network 语义（TTL 钳制、
     全内存保留进程（后台心跳进程 connect 后继续）与 envs 恢复、
     egress 白/黑名单实际拦截与全量替换（含 create 时携带 allowOut/
     allow_internet_access=false 生效，需外网，不可达则跳过）、memory=false 400、
     paused 态 409 与数据面 503、kill paused）。
  2. envd 数据接口 (SDK commands/files/pty)：前台/后台命令、stdin、流式回调、
     user/cwd/envs、目录与文件增删查读写、rename、get_info(Stat)、
     write_files 批量上传、watch_dir、/files 上传下载、upload_url/download_url。
  3. code-interpreter (Jupyter 49999)：run_code、会话状态、多 context、
     stdout/stderr、富文本结果、执行错误/超时、envs。
  4. 数据面 (节点内嵌 sandboxproxy)：E2b-Sandbox-Id/Port 与 X-Agentenv-* 头
     路由、/proxy 前缀路由、HTTP/SSE 流式、WebSocket 回显与大帧、ping/pong、
     maskRequestHost 的 ${PORT} 改写、路由错误码。
  5. 生命周期：列表回收（无 /nodes 指标）、创建 timeout 到期自动删除、
     kill 后 404、幂等删除、on_timeout=pause 到期自动暂停、autoResume 数据面
     唤醒（paused 沙箱经路由请求直接恢复，300s TTL 下限，运行中不动 TTL）。
  6. 数据面 Host 域名路由（get_host 应用 URL）：仅当节点配置了
     sandbox_proxy_domains（create 响应携带 domain）时测试，否则整节跳过。
  7. 收尾清理：删除本套件残留的 checkpoint 模板（*-ckpt:v1、自动命名
     checkpoint-*、可能残留的 pause-*；仍被 paused 沙箱引用的除外）。
     E2B API 无模板管理端点，经本机 conch CLI（unix socket）清理；镜像模板按
     origin 过滤天然保留。

依赖：e2b==2.46.4、e2b-code-interpreter、httpx、websockets==15.0.1。
环境变量：E2B_API_KEY、E2B_API_URL（节点监听地址）、E2B_TEMPLATE_ID；
E2B_SANDBOX_URL 缺省时自动取 E2B_API_URL（单机数据面与控制面同地址）。
模板需要 Python3 与 envd（e2b-rootfs-example 即可，code-interpreter 需 Jupyter）。
清理可选：CONCH_CLI（默认 conch，可用 "sudo conch"）、CONCH_CLI_CONFIG
（--config 路径，默认自动探测）。
"""

import io
import json
import os
import re
import shlex
import shutil
import subprocess
import sys
import time
import traceback
import urllib.parse
import uuid

import httpx
from e2b import PtySize, Sandbox, SandboxQuery, Secret, Template
from e2b.exceptions import TimeoutException
from e2b.sandbox.commands.command_handle import CommandExitException
from e2b.sandbox.sandbox_api import SandboxNetworkOpts, SandboxNetworkUpdate
from websockets.sync.client import connect

from e2b_code_interpreter import Sandbox as CISandbox

# 访客内运行的 HTTP 应用（含 WebSocket 回显），用于数据面验证。
APPLICATION = r'''
import base64, hashlib, json, struct, time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from socketserver import TCPServer

class Server(ThreadingHTTPServer):
    def server_bind(self):
        TCPServer.server_bind(self)
        self.server_name = "localhost"
        self.server_port = self.server_address[1]

class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    def do_GET(self):
        if self.path == "/ws":
            key = self.headers["Sec-WebSocket-Key"]
            accept = base64.b64encode(hashlib.sha1((key +
                "258EAFA5-E914-47DA-95CA-C5AB0DC85B11").encode()).digest()).decode()
            self.send_response(101)
            self.send_header("Upgrade", "websocket")
            self.send_header("Connection", "Upgrade")
            self.send_header("Sec-WebSocket-Accept", accept)
            self.end_headers()
            while True:
                header = self.rfile.read(2)
                if len(header) != 2:
                    return
                opcode, length = header[0] & 15, header[1] & 127
                if length == 126:
                    length = struct.unpack("!H", self.rfile.read(2))[0]
                elif length == 127:
                    length = struct.unpack("!Q", self.rfile.read(8))[0]
                if length > 1048576:
                    return
                mask = self.rfile.read(4) if header[1] & 128 else b""
                data = self.rfile.read(length)
                if mask:
                    data = bytes(v ^ mask[i % 4] for i, v in enumerate(data))
                opcode = 10 if opcode == 9 else opcode
                if len(data) < 126:
                    size = bytes([len(data)])
                elif len(data) < 65536:
                    size = b"\x7e" + struct.pack("!H", len(data))
                else:
                    size = b"\x7f" + struct.pack("!Q", len(data))
                self.wfile.write(bytes([128 | opcode]) + size + data)
                self.wfile.flush()
                if opcode == 8:
                    return
        elif self.path == "/stream":
            self.close_connection = True
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.send_header("Connection", "close")
            self.end_headers()
            for index in range(3):
                self.wfile.write(f"data: {index}\n\n".encode())
                self.wfile.flush()
                time.sleep(0.4)
        else:
            body = json.dumps({"path": self.path,
                "marker": self.headers.get("X-Conch-Marker"),
                "host": self.headers.get("Host"),
                "forwarded_host": self.headers.get("X-Forwarded-Host")}).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
    def log_message(self, *args):
        pass

Server(("0.0.0.0", 8080), Handler).serve_forever()
'''

# guest 出站探测脚本：TCP 连通打印 OPEN，失败打印 BLOCKED+原因；退出码恒为 0，
# 避免 commands.run 因非零退出码抛 CommandExitException。
EGRESS_PROBE = r'''
import socket, sys
try:
    sock = socket.socket()
    sock.settimeout(8)
    sock.connect((sys.argv[1], int(sys.argv[2])))
    print("OPEN")
except Exception as err:
    print(f"BLOCKED {type(err).__name__}: {err}")
'''

PASS, FAIL, SKIPPED = [], [], []
SUITE = f"conch-sn-fullsdk-{uuid.uuid4()}"
API_URL = os.environ.get("E2B_API_URL", "").rstrip("/")


def check(name, condition, detail=""):
    if condition:
        PASS.append(name)
        print(f"  PASS {name}")
    else:
        FAIL.append((name, detail))
        print(f"  FAIL {name} :: {detail}")


def section(title):
    print(f"\n=== {title} ===")


def skip_section(name, reason):
    SKIPPED.append((name, reason))
    print(f"\n=== {name}：跳过（{reason}）===")


def eventually(probe, accepts, description, timeout=30):
    deadline = time.monotonic() + timeout
    last = None
    while True:
        last = probe()
        if accepts(last):
            return last
        if time.monotonic() >= deadline:
            raise AssertionError(f"{description}: {last}")
        time.sleep(0.5)


def egress_probe(box, host, port):
    """在 guest 内探测 host:port 连通性，返回探测脚本最后一行输出。"""
    try:
        r = box.commands.run(f"python3 /tmp/conch-e2e-egress-probe.py {host} {port}", timeout=30)
    except Exception as err:
        return f"PROBE_ERROR {type(err).__name__}: {str(err)[:80]}"
    lines = (r.stdout or "").strip().splitlines()
    return lines[-1] if lines else "(无输出)"


def probe_open(box, host, port, attempts=3):
    """OPEN 期望的探测：外网连通偶发抖动，失败后短暂重试再判定。"""
    out = ""
    for _ in range(attempts):
        out = egress_probe(box, host, port)
        if out.startswith("OPEN"):
            break
        time.sleep(2)
    return out


def last_heartbeat(box):
    """读后台心跳进程写入 /tmp/alive.txt 的最后一个时间戳。"""
    try:
        lines = (box.files.read("/tmp/alive.txt") or "").strip().splitlines()
    except Exception:
        return ""  # 进程尚未创建文件
    return lines[-1] if lines else ""


def expect_status(name, response, status):
    check(name, response.status_code == status,
          f"HTTP {response.status_code} != {status}: {response.text[:120]}")


def sdk_error(fn):
    """运行 SDK 调用，返回 (异常类型名, 错误文本)；无异常返回 (None, None)。"""
    try:
        fn()
        return None, None
    except Exception as err:
        return type(err).__name__, str(err)


def expect_sdk_501(name, fn):
    """断言 SDK 调用报 501（或含 not supported）。"""
    etype, etext = sdk_error(fn)
    check(f"SDK {name} 501", etype is not None and ("501" in etext or "not supported" in etext),
          f"{etype}: {etext[:120] if etext else 'no exception'}")


def expect_sdk_status(name, fn, code):
    """断言 SDK 调用异常文本包含指定状态码。"""
    etype, etext = sdk_error(fn)
    check(f"SDK {name} {code}", etype is not None and str(code) in (etext or ""),
          f"{etype}: {etext[:120] if etext else 'no exception'}")


def routing_headers(box, port):
    return {"e2b-sandbox-id": box.sandbox_id, "e2b-sandbox-port": str(port)}


def suite_sandboxes(client):
    """列出本套件（metadata.suite=SUITE）的全部沙箱。"""
    return client.get("/sandboxes", params={"metadata": f"suite={SUITE}", "limit": 100}).json()


def api_host_port():
    """从 E2B_API_URL 解析 (host, port)，用于裸 socket 的 WS 握手。"""
    parsed = urllib.parse.urlparse(API_URL)
    host = parsed.hostname or "127.0.0.1"
    port = parsed.port or (443 if parsed.scheme == "https" else 80)
    return host, port


# ---------------------------------------------------------------- 控制面
def test_control_plane(client, template):
    section("控制面 Node REST（直连）")
    # 节点 /health 返回 200（Gateway 为 204），无需鉴权
    expect_status("health 无需鉴权(200)", httpx.get(f"{API_URL}/health"), 200)
    expect_status("缺 API key 401", client.get("/sandboxes", headers={"X-API-Key": ""}), 401)
    expect_status("错误 API key 401", client.get("/sandboxes", headers={"X-API-Key": "invalid"}), 401)
    # 直连模式无集群视图：/nodes 是 Gateway 端点，节点上不存在
    expect_status("单节点无 /nodes 端点", client.get("/nodes"), 404)

    # ---- 创建参数校验：优先 SDK 调用形态 ----
    # secure=True 是 SDK 默认值；SDK 层断言 + HTTP 层核对状态码
    expect_sdk_status("create(secure=True) 501", lambda: Sandbox.create(template, request_timeout=60), 501)
    expect_status("secure=true 501 (HTTP)", client.post("/sandboxes", json={"templateID": template, "secure": True}), 501)
    expect_sdk_status("create(未知模板) 404", lambda: Sandbox.create("does/not:exist", secure=False, request_timeout=60), 404)
    expect_sdk_status("create(负 timeout) 400", lambda: Sandbox.create(template, secure=False, timeout=-1, request_timeout=60), 400)
    expect_sdk_501("create(mcp)", lambda: Sandbox.create(template, secure=False, timeout=5, request_timeout=60, mcp={"x": {}}))
    # lifecycle 已全部支持：on_timeout=pause 到期自动暂停、auto_resume 数据面
    # 唤醒（见 test_auto_resume_wake；SDK 本地要求 auto_resume 搭配
    # on_timeout="pause" 才发请求）
    # network maskRequestHost 已支持（代理改写）；egressProxy 仍 501
    # network maskRequestHost 已支持（代理改写）；egressProxy 仍 501
    # （SDK 本地要求 egress_proxy 是 {"address": ...} dict 才发请求）
    expect_sdk_501("create(network egressProxy)", lambda: Sandbox.create(template, secure=False, timeout=5, request_timeout=60, network=SandboxNetworkOpts(egress_proxy={"address": "proxy.example.com:1080"})))

    # SDK 无法触达的协议细节（未知字段/缺 templateID），HTTP 直测
    r = client.post("/sandboxes", json={"secure": False})
    expect_status("缺 templateID 400", r, 400)
    r = client.post("/sandboxes", json={"templateID": template, "secure": False, "bogus": 1})
    expect_status("未知字段 400", r, 400)
    # 其余创建错误路径：egress 之外的网络限制 501、仅磁盘暂停 400、超 TTL 上限 400
    r = client.post("/sandboxes", json={"templateID": template, "secure": False,
                                        "network": {"rules": [{"ports": [80], "protocol": "tcp"}]}})
    expect_status("create network rules 501", r, 501)
    r = client.post("/sandboxes", json={"templateID": template, "secure": False,
                                        "network": {"allowPublicTraffic": False}})
    expect_status("create allowPublicTraffic=false 501", r, 501)
    r = client.post("/sandboxes", json={"templateID": template, "secure": False, "autoPauseMemory": False})
    expect_status("create autoPauseMemory=false 400", r, 400)
    r = client.post("/sandboxes", json={"templateID": template, "secure": False, "timeout": 86400 + 1})
    expect_status("create 超 TTL 上限 400", r, 400)

    # ---- 顶层模块：Template / Secret 走 SDK ----
    expect_sdk_501("Template.exists", lambda: Template.exists(template))
    expect_sdk_status("Secret.create 404", lambda: Secret.create("name", {"k": "v"}), 404)
    expect_sdk_status("Secret.list 404", lambda: [s for s in Secret.list().next_items()], 404)

    # /sandboxes-cold 无 SDK 方法，HTTP 直测
    expect_status("未实现 /sandboxes-cold 501", client.get("/sandboxes-cold"), 501)
    # 其余显式 501 的顶层路径（无 SDK 方法）
    expect_status("未实现 /templates 501", client.get("/templates"), 501)
    expect_status("未实现 /volumes 501", client.get("/volumes"), 501)

    # 未路由的 E2B 端点 → 404（v2 create、v2 connect、logs、集群 metrics）
    expect_status("POST /v2/sandboxes 404", client.post("/v2/sandboxes", json={"templateID": template, "secure": False}), 404)
    expect_status("POST v2 connect 404", client.post(f"/v2/sandboxes/{uuid.uuid4()}/connect", json={"timeout": 60}), 404)
    # GET /snapshots 已实现（列表见 test_checkpoint），未实现删除端点
    r = client.get("/snapshots", params={"limit": 100})
    expect_status("GET /snapshots 200", r, 200)
    check("GET /snapshots 返回数组", isinstance(r.json(), list), r.text[:80])
    expect_status("DELETE /snapshots/{id} 404", client.delete(f"/snapshots/{uuid.uuid4()}"), 404)
    expect_status("GET /sandboxes/metrics 404", client.get("/sandboxes/metrics"), 404)

    # 超限请求体：maxBodyBytes = 1MiB
    r = client.post("/sandboxes", json={"templateID": template, "secure": False,
                                        "metadata": {"pad": "x" * (1 << 20)}})
    expect_status("超限请求体 413", r, 413)

    # 未知沙箱的各操作 → 404
    bogus = str(uuid.uuid4())
    expect_status("pause 未知沙箱 404", client.post(f"/sandboxes/{bogus}/pause", json={}), 404)
    expect_status("connect 未知沙箱 404", client.post(f"/sandboxes/{bogus}/connect", json={"timeout": 60}), 404)
    expect_status("timeout 未知沙箱 404", client.post(f"/sandboxes/{bogus}/timeout", json={"timeout": 60}), 404)
    expect_status("refreshes 未知沙箱 404", client.post(f"/sandboxes/{bogus}/refreshes", json={"duration": 30}), 404)
    expect_status("update_network 未知沙箱 404", client.put(f"/sandboxes/{bogus}/network", json={}), 404)

    box = Sandbox.create(template, secure=False, timeout=300, request_timeout=180,
                         metadata={"suite": SUITE}, envs={"CONCH_E2E_VALUE": "from-create"})
    try:
        # SDK 方法层（box.xxx()）：已实现的 set_timeout/update_network/refreshes
        box.set_timeout(600)
        info = box.get_info()
        import datetime as _dt
        remaining = (info.end_at - _dt.datetime.now(_dt.timezone.utc)).total_seconds()
        check("set_timeout 重置 TTL", 590 < remaining < 605, str(remaining))
        # set_timeout 可缩短
        box.set_timeout(120)
        info = box.get_info()
        remaining = (info.end_at - _dt.datetime.now(_dt.timezone.utc)).total_seconds()
        check("set_timeout 可缩短", remaining < 121, str(remaining))
        box.set_timeout(600)

        box.update_network(SandboxNetworkUpdate(allow_out=["1.1.1.1/32"], deny_out=["0.0.0.0/0"]))
        check("update_network 204 生效", True)
        # 域名 allowOut 由 netstack 拒绝（仅 IPv4/CIDR）
        etype, etext = sdk_error(lambda: box.update_network(SandboxNetworkUpdate(allow_out=["example.com"])))
        check("update_network 域名 400", etype is not None and "400" in (etext or ""), f"{etype}: {etext}")

        r = client.post(f"/sandboxes/{box.sandbox_id}/refreshes", json={"duration": 30})
        expect_status("refreshes 只延长 (HTTP)", r, 204)
        info = box.get_info()
        remaining = (info.end_at - _dt.datetime.now(_dt.timezone.utc)).total_seconds()
        check("refreshes 后 TTL 不低于原值", remaining > 590, str(remaining))

        # refreshes 时长钳制：下限 15s、上限为沙箱总寿命（create 后 24h）。
        # SDK 没有 refreshes 方法（不调用该端点），只能裸 HTTP
        box.set_timeout(10)
        r = client.post(f"/sandboxes/{box.sandbox_id}/refreshes", json={"duration": 3})
        expect_status("refreshes 过短钳制 204", r, 204)
        info = box.get_info()
        remaining = (info.end_at - _dt.datetime.now(_dt.timezone.utc)).total_seconds()
        check("refreshes 3s 钳制为 15s", 5 < remaining <= 16, str(remaining))
        r = client.post(f"/sandboxes/{box.sandbox_id}/refreshes", json={"duration": 999999})
        expect_status("refreshes 过长钳制 204", r, 204)
        info = box.get_info()
        remaining = (info.end_at - _dt.datetime.now(_dt.timezone.utc)).total_seconds()
        check("refreshes 999999s 钳制到总寿命", 86400 - 900 < remaining <= 86400, str(remaining))

        # 运行中 connect 缺省/非正 timeout：no-op（200），截止时间不动。
        # SDK 恒发 timeout=300（`timeout or 300` 短路掉 0），这两种请求只能裸 HTTP 构造
        deadline = info.end_at
        r = client.post(f"/sandboxes/{box.sandbox_id}/connect", json={})
        expect_status("connect 空 body 200 no-op", r, 200)
        check("connect 空 body 不动 TTL", box.get_info().end_at == deadline, "")
        r = client.post(f"/sandboxes/{box.sandbox_id}/connect", json={"timeout": 0})
        expect_status("connect timeout=0 200 no-op", r, 200)
        check("connect timeout=0 不动 TTL", box.get_info().end_at == deadline, "")
        box.set_timeout(600)

        # update_network 未知字段（协议层 DisallowUnknownFields）
        r = client.put(f"/sandboxes/{box.sandbox_id}/network", json={"bogus": 1})
        expect_status("update_network 未知字段 400", r, 400)

        # egress 实际拦截行为：上面的 update_network 断言只覆盖状态码，这里验证
        # 白/黑名单真正作用于 guest 出站流量（需外网可达 223.5.5.5/8.8.8.8，否则跳过）。
        egress_box = Sandbox.create(template, secure=False, timeout=300, request_timeout=180,
                                    metadata={"suite": SUITE, "phase": "egress"})
        try:
            egress_box.files.write("/tmp/conch-e2e-egress-probe.py", EGRESS_PROBE)

            def probe(host, port):
                return egress_probe(egress_box, host, port)

            base_a, base_b = probe_open(egress_box, "223.5.5.5", 53), probe_open(egress_box, "8.8.8.8", 53)
            if not (base_a.startswith("OPEN") and base_b.startswith("OPEN")):
                skip_section("egress 拦截行为",
                             f"无策略时 guest 出站即不可达：{base_a} / {base_b}")
            else:
                # 白名单：仅放行 A，其余默认拒绝
                egress_box.update_network(SandboxNetworkUpdate(allow_out=["223.5.5.5/32"]))
                out = probe_open(egress_box, "223.5.5.5", 53)
                check("egress 白名单放行目标", out.startswith("OPEN"), out)
                out = probe("8.8.8.8", 53)
                check("egress 白名单默认拒绝其余", out.startswith("BLOCKED"), out)
                # 全量替换：换白名单后旧条目不残留
                egress_box.update_network(SandboxNetworkUpdate(allow_out=["8.8.8.8/32"]))
                out = probe_open(egress_box, "8.8.8.8", 53)
                check("egress 全量替换新目标放行", out.startswith("OPEN"), out)
                out = probe("223.5.5.5", 53)
                check("egress 全量替换旧目标被拒", out.startswith("BLOCKED"), out)
                # 黑名单：只封 A，其余放行
                egress_box.update_network(SandboxNetworkUpdate(deny_out=["223.5.5.5/32"]))
                out = probe("223.5.5.5", 53)
                check("egress 黑名单目标被拒", out.startswith("BLOCKED"), out)
                out = probe_open(egress_box, "8.8.8.8", 53)
                check("egress 黑名单不影响其余", out.startswith("OPEN"), out)
                # 同一目标同时进 allow/deny：deny 规则先生效
                egress_box.update_network(SandboxNetworkUpdate(allow_out=["223.5.5.5/32"],
                                                               deny_out=["223.5.5.5/32"]))
                out = probe("223.5.5.5", 53)
                check("egress deny 优先于 allow", out.startswith("BLOCKED"), out)
                # 空更新 = 替换为空策略 = 恢复全通
                egress_box.update_network(SandboxNetworkUpdate())
                out = probe_open(egress_box, "223.5.5.5", 53)
                check("egress 空更新恢复全通", out.startswith("OPEN"), out)
                # 先释放 egress_box：宿主内存有限，避免与 create 时测试的沙箱叠加
                try:
                    egress_box.kill()
                except Exception:
                    pass

                # create 时携带的 allowOut 同样生效（此前只测过 501 拒绝路径）
                born = Sandbox.create(template, secure=False, timeout=300, request_timeout=180,
                                      metadata={"suite": SUITE, "phase": "egress-create"},
                                      network=SandboxNetworkOpts(allow_out=["223.5.5.5/32"]))
                try:
                    born.files.write("/tmp/conch-e2e-egress-probe.py", EGRESS_PROBE)
                    out = probe_open(born, "223.5.5.5", 53)
                    check("create 白名单放行目标", out.startswith("OPEN"), out)
                    out = egress_probe(born, "8.8.8.8", 53)
                    check("create 白名单默认拒绝其余", out.startswith("BLOCKED"), out)
                finally:
                    try:
                        born.kill()
                    except Exception:
                        pass
                # allow_internet_access=false → netstack 兜底 REJECT（SDK create 不暴露该参数，HTTP 直测）
                r = client.post("/sandboxes", json={"templateID": template, "secure": False,
                                                    "timeout": 300, "allow_internet_access": False,
                                                    "metadata": {"suite": SUITE, "phase": "egress-noinet"}})
                expect_status("create allow_internet_access=false 201", r, 201)
                if r.status_code == 201:
                    offline = Sandbox.connect(r.json()["sandboxID"], timeout=300, request_timeout=180)
                    try:
                        offline.files.write("/tmp/conch-e2e-egress-probe.py", EGRESS_PROBE)
                        out = egress_probe(offline, "223.5.5.5", 53)
                        check("allow_internet_access=false 全拒", out.startswith("BLOCKED"), out)
                    finally:
                        try:
                            offline.kill()
                        except Exception:
                            pass
        finally:
            try:
                egress_box.kill()
            except Exception:
                pass

        # 保持 501 的端点
        expect_sdk_501("fork", box.fork)
        expect_sdk_501("get_metrics", box.get_metrics)

        # 详情：SDK get_info
        info = box.get_info()
        check("get_info 字段完整", all(hasattr(info, k) for k in
              ["sandbox_id", "template_id", "envd_version", "started_at",
               "end_at", "cpu_count", "memory_mb", "state", "metadata"]),
              str(sorted(vars(info).keys())))
        check("envd_version 非空", bool(getattr(info, "envd_version", "")),
              repr(getattr(info, "envd_version", "")))
        check("metadata 回读一致", info.metadata == {"suite": SUITE}, str(info.metadata))
        check("state=running", info.state == "running", str(info.state))

        # 未知沙箱：直连节点返回 JSON 404，SDK 正常映射为 False
        # （经 Gateway 时为 text/plain 404，SDK 抛 JSONDecodeError——单机无此问题）
        bogus = str(uuid.uuid4())
        killed = Sandbox.kill(bogus, request_timeout=30)
        check("kill(未知沙箱) 返回 False", killed is False, str(killed))
        # PUT /sandboxes 无 SDK 方法，HTTP 直测
        expect_status("PUT /sandboxes 404", client.put("/sandboxes"), 404)

        # 列表：SDK 分页器 + 查询
        paginator = Sandbox.list(query=SandboxQuery(metadata={"suite": SUITE}), limit=1)
        listed = []
        while paginator.has_next:
            listed.extend(s.sandbox_id for s in paginator.next_items())
        check("SDK 分页器 metadata 过滤", box.sandbox_id in listed, str(listed))
        # 非法 order：SDK 分页器迭代时本地校验（InvalidArgumentException），不发请求
        etype, _ = sdk_error(lambda: [s for s in Sandbox.list(order="invalid").next_items()])
        check("SDK list(非法 order) 本地拦截", etype == "InvalidArgumentException", etype or "no exception")
        # limit 边界 / state 过滤的裸 HTTP 语义，SDK 不暴露 limit=0/101 这种调用面
        expect_status("limit=0 400", client.get("/sandboxes", params={"limit": 0}), 400)
        expect_status("limit=101 400", client.get("/sandboxes", params={"limit": 101}), 400)
        expect_status("state=paused 过滤 200", client.get("/sandboxes", params={"state": "paused"}), 200)
        expect_status("state=running 过滤 200", client.get("/sandboxes", params={"state": "running"}), 200)
        expect_status("state 非法值 400", client.get("/sandboxes", params={"state": "creating"}), 400)

        # /v2/sandboxes 列表别名（与 /sandboxes 同一处理）
        r = client.get("/v2/sandboxes", params={"metadata": f"suite={SUITE}"})
        expect_status("GET /v2/sandboxes 200", r, 200)
        check("v2 列表含本沙箱", box.sandbox_id in [s["sandboxID"] for s in r.json()], r.text[:120])

        # order=asc 与 desc 结果相反
        ids_desc = [s["sandboxID"] for s in client.get("/sandboxes", params={"metadata": f"suite={SUITE}"}).json()]
        ids_asc = [s["sandboxID"] for s in client.get("/sandboxes", params={"metadata": f"suite={SUITE}", "order": "asc"}).json()]
        check("order=asc 逆序输出", set(ids_asc) == set(ids_desc)
              and (len(ids_asc) < 2 or ids_asc == list(reversed(ids_desc))),
              f"{ids_desc} vs {ids_asc}")
        expect_status("order 非法值 400", client.get("/sandboxes", params={"order": "invalid"}), 400)

        # X-Total-Running 响应头
        r = client.get("/sandboxes", params={"metadata": f"suite={SUITE}"})
        check("X-Total-Running 头", r.headers.get("X-Total-Running", "").isdigit()
              and int(r.headers["X-Total-Running"]) >= 1, str(r.headers.get("X-Total-Running")))

        # template 过滤（按 digest 命中 SourceTemplateID）
        r = client.get("/sandboxes", params={"template": info.template_id, "metadata": f"suite={SUITE}"})
        check("template 过滤命中", box.sandbox_id in [s["sandboxID"] for s in r.json()], r.text[:120])
        r = client.get("/sandboxes", params={"template": "does/not:exist", "metadata": f"suite={SUITE}"})
        check("template 过滤未命中", r.json() == [], r.text[:80])

        # startedAfter 过滤
        future = (_dt.datetime.now(_dt.timezone.utc) + _dt.timedelta(hours=1)).isoformat()
        past = "2020-01-01T00:00:00Z"
        r = client.get("/sandboxes", params={"startedAfter": future, "metadata": f"suite={SUITE}"})
        check("startedAfter 未来无结果", r.json() == [], r.text[:80])
        r = client.get("/sandboxes", params={"startedAfter": past, "metadata": f"suite={SUITE}"})
        check("startedAfter 过去含本沙箱", box.sandbox_id in [s["sandboxID"] for s in r.json()], r.text[:120])
        expect_status("startedAfter 非法值 400", client.get("/sandboxes", params={"startedAfter": "not-a-time"}), 400)

        # metadata 过滤：重复键 last-value-wins（E2B 解析语义）；非法编码 400
        r = client.get("/sandboxes", params={"metadata": f"suite=wrong&suite={SUITE}"})
        check("metadata 重复键 last-value-wins", box.sandbox_id in [s["sandboxID"] for s in r.json()], r.text[:120])
        expect_status("metadata 非法编码 400", client.get("/sandboxes", params={"metadata": "%zz"}), 400)

        # nextToken：非法值 400、顺序不匹配 400
        expect_status("nextToken 非法 400", client.get("/sandboxes", params={"nextToken": "!!!not-base64!!!"}), 400)
        import base64 as _b64
        token = _b64.urlsafe_b64encode(f"2024-01-01T00:00:00Z__{box.sandbox_id}".encode()).decode()
        expect_status("nextToken 顺序不匹配 400", client.get("/sandboxes", params={"order": "asc", "nextToken": token}), 400)

        # 子资源方法路由：GET pause / GET logs 未路由 404；单资源 PATCH 405
        expect_status("GET /pause 404", client.get(f"/sandboxes/{box.sandbox_id}/pause"), 404)
        expect_status("GET /sandboxes/{id}/logs 404", client.get(f"/sandboxes/{box.sandbox_id}/logs"), 404)
        expect_status("PATCH /sandboxes/{id} 405", client.patch(f"/sandboxes/{box.sandbox_id}"), 405)

        digest = info.template_id
        if digest.startswith("sha256:"):
            try:
                b2 = Sandbox.create(digest, secure=False, timeout=60, request_timeout=180)
                b2.kill()
                check("digest 模板创建", True)
            except Exception as err:
                check("digest 模板创建", False, str(err)[:120])
    finally:
        box.kill()
    expect_status("GET 已删除沙箱 404", client.get(f"/sandboxes/{box.sandbox_id}"), 404)
    expect_status("DELETE 已删除沙箱 404", client.delete(f"/sandboxes/{box.sandbox_id}"), 404)


# ---------------------------------------------------------------- envd
def test_envd(box):
    section("envd commands")
    result = box.commands.run('printf "%s" "$CONCH_E2E_VALUE"; printf err >&2')
    check("stdout/stderr/exit_code", result.stdout == "from-create"
          and result.stderr == "err" and result.exit_code == 0,
          f"{result.stdout!r} {result.stderr!r} {result.exit_code}")

    r = box.commands.run("pwd; printf %s \"$MYVAR\"", cwd="/etc", envs={"MYVAR": "v1"})
    check("cwd/envs", r.stdout == "/etc\nv1", repr(r.stdout))

    r = box.commands.run("id -u", user="root")
    check("user=root", r.exit_code == 0 and r.stdout.strip() == "0", r.stdout)

    try:
        box.commands.run("exit 7")
        check("非零退出码抛异常", False, "no exception")
    except CommandExitException:
        check("非零退出码抛异常", True)

    try:
        box.commands.run("sleep 10", timeout=1)
        check("命令超时异常", False, "no exception")
    except TimeoutException:
        check("命令超时异常", True)

    outs, errs = [], []
    r = box.commands.run("echo out1; echo err1 >&2", on_stdout=outs.append, on_stderr=errs.append)
    check("流式回调", outs == ["out1\n"] and errs == ["err1\n"], f"{outs} {errs}")

    r = box.commands.run("sleep 1; echo late", timeout=0)
    check("timeout=0 不限时", r.exit_code == 0 and r.stdout.strip() == "late", r.stdout)

    handle = box.commands.run("sleep 300", background=True, timeout=0)
    check("后台命令返回 pid", isinstance(handle.pid, int) and handle.pid > 0, str(handle.pid))
    # 进程列表（envd Process List）：后台命令可按 pid 列出
    procs = box.commands.list()
    check("commands.list 列出后台进程", any(p.pid == handle.pid for p in procs),
          str([(p.pid, p.cmd) for p in procs]))
    check("后台命令 kill", handle.kill() is True, "")

    handle = box.commands.run("cat > /tmp/stdin.txt", stdin=True, background=True, timeout=30)
    handle.send_stdin(b"interactive-payload\n")
    handle.close_stdin()
    r = handle.wait()
    check("stdin 交互命令", r.exit_code == 0
          and box.files.read("/tmp/stdin.txt") == "interactive-payload\n", str(r))

    section("envd files")
    base = "/tmp/conch-fullsdk"
    if box.files.exists(base):
        box.files.remove(base)
    check("make_dir", box.files.make_dir(base) is True)
    info = box.files.write(f"{base}/bin.dat", b"\x00\x01\x02\xff")
    check("二进制写入", info.name == "bin.dat")
    check("二进制读取", bytes(box.files.read(f"{base}/bin.dat", format="bytes")) == b"\x00\x01\x02\xff")

    box.files.write(f"{base}/text.txt", "Conch 文件传输\n" * 512)
    check("大文本 roundtrip", box.files.read(f"{base}/text.txt") == "Conch 文件传输\n" * 512)

    filename = f"{base}/e2e file %23.txt"
    box.files.write(filename, "escaped")
    names = [e.name for e in box.files.list(base, depth=2)]
    check("特殊字符文件名列出", "e2e file %23.txt" in names, str(names))

    check("exists 正/误", box.files.exists(filename) and not box.files.exists(f"{base}/nope"))

    box.files.make_dir(f"{base}/sub/deep")
    check("嵌套 make_dir", box.files.exists(f"{base}/sub/deep"))
    box.files.remove(f"{base}/sub")
    check("remove 目录", not box.files.exists(f"{base}/sub/deep"))

    buf = io.BytesIO(b"io-stream-data")
    box.files.write(f"{base}/io.txt", buf)
    check("IO 对象写入", bytes(box.files.read(f"{base}/io.txt", format="bytes")) == b"io-stream-data")

    with box.files.read(f"{base}/io.txt", format="stream") as stream:
        chunks = list(stream)
    check("流式读取", b"".join(chunks) == b"io-stream-data", str(chunks[:2]))

    # Stat：get_info 返回 EntryInfo
    stat = box.files.get_info(f"{base}/text.txt")
    check("get_info(Stat)", stat.name == "text.txt" and stat.type.value == "file"
          and stat.path == f"{base}/text.txt", str(stat))

    # Move：rename 保留内容
    box.files.write(f"{base}/rename-src.txt", "renamed-content")
    entry = box.files.rename(f"{base}/rename-src.txt", f"{base}/rename-dst.txt")
    check("rename 重命名", entry.name == "rename-dst.txt"
          and box.files.read(f"{base}/rename-dst.txt") == "renamed-content"
          and not box.files.exists(f"{base}/rename-src.txt"), str(entry))

    # 批量上传：write_files 一次写多个文件
    infos = box.files.write_files([
        {"path": f"{base}/batch/a.txt", "data": "A"},
        {"path": f"{base}/batch/b.bin", "data": b"\x00\xff"},
    ])
    check("write_files 批量上传", [i.name for i in infos] == ["a.txt", "b.bin"]
          and box.files.read(f"{base}/batch/a.txt") == "A"
          and bytes(box.files.read(f"{base}/batch/b.bin", format="bytes")) == b"\x00\xff",
          str([i.name for i in infos]))

    box.files.remove(base)
    check("remove 根目录", not box.files.exists(base))

    watch = box.files.watch_dir("/tmp")
    time.sleep(0.5)
    box.files.write("/tmp/watched-file.txt", "data")
    seen = eventually(watch.get_new_events, lambda evs: len(evs) > 0,
                      "watch_dir 事件到达", timeout=8)
    check("watch_dir CREATE 事件", any(
        getattr(e, "name", None) == "watched-file.txt" for e in seen), str(seen[:3]))
    watch.stop()

    section("envd /files 上传下载 URL")
    box.files.write("/tmp/dl.txt", "download-me")
    down = box.download_url("/tmp/dl.txt")
    r = httpx.get(down, headers=routing_headers(box, 49983))
    check("download_url 下载", r.status_code == 200 and r.text == "download-me",
          f"{r.status_code} {r.text[:40]}")
    up = box.upload_url("/tmp/ul.txt")
    r = httpx.post(up, headers={**routing_headers(box, 49983),
                                "Content-Type": "application/octet-stream"},
                   content=b"uploaded-bytes")
    check("upload_url 上传", r.status_code == 200
          and box.files.read("/tmp/ul.txt") == "uploaded-bytes", f"{r.status_code} {r.text[:60]}")

    section("envd pty")
    pty = box.pty.create(size=PtySize(rows=24, cols=80), timeout=30)
    box.pty.resize(pty.pid, size=PtySize(rows=35, cols=100))
    box.pty.send_stdin(pty.pid, b"printf 'PTY_OK\\n'; stty size; exit\n")
    output = []
    result = pty.wait(on_pty=output.append)
    data = b"".join(output)
    check("pty resize+回显", result.exit_code == 0 and b"PTY_OK" in data and b"35 100" in data,
          data[-120:])

    pty2 = box.pty.create(size=PtySize(rows=24, cols=80), timeout=30)
    check("pty kill", box.pty.kill(pty2.pid) is True, str(pty2.pid))

    check("get_info().sandbox_id 一致", box.get_info().sandbox_id == box.sandbox_id)
    check("is_running", box.is_running() is True)


# ---------------------------------------------------------------- code-interpreter
def test_code_interpreter(box):
    section("code-interpreter run_code")
    e = box.run_code("x = 1")
    check("首条执行无结果", e.error is None and e.results == [], str(e.error))
    e = box.run_code("x += 1; x")
    check("会话状态保持", bool(e.results) and e.results[0].text == "2",
          str([r.text for r in e.results]))
    check("execution_count 递增", e.execution_count >= 2, str(e.execution_count))

    e = box.run_code("import sys; print('o1'); print('e1', file=sys.stderr)")
    check("stdout 日志", e.logs.stdout == ["o1\n"], str(e.logs))
    check("stderr 日志", e.logs.stderr == ["e1\n"], str(e.logs))

    e = box.run_code("1/0")
    check("执行错误回传", e.error is not None and e.error.name == "ZeroDivisionError"
          and e.error.value == "division by zero",
          f"{e.error.name if e.error else None} {e.error.value if e.error else None}")

    e = box.run_code("{'k': [1, 2, 3]}")
    check("json 结果", e.results[0].json == {"k": [1, 2, 3]}, str(e.results[0].json))

    e = box.run_code("from IPython.display import HTML; HTML('<b>hi</b>')")
    check("html 富文本", bool(e.results) and e.results[0].html == "<b>hi</b>",
          str(e.results[0].html if e.results else None))
    e = box.run_code("from IPython.display import SVG; "
                     "SVG('<svg xmlns=\"http://www.w3.org/2000/svg\"/>')")
    check("svg 富文本", bool(e.results) and bool(e.results[0].svg),
          str(e.results[0].svg if e.results else None)[:80])

    e = box.run_code("from IPython.display import display; display('one'); display('two')")
    check("多结果", [r.text for r in e.results] == ["one", "two"],
          str([r.text for r in e.results]))

    e = box.run_code("import os; os.environ['CI_VAR']", envs={"CI_VAR": "ci-env-ok"})
    check("run_code envs", bool(e.results) and e.results[0].text == "ci-env-ok",
          str([r.text for r in e.results]))

    outs = []
    e = box.run_code("print('cb')", on_stdout=outs.append)
    check("on_stdout 回调", bool(outs) and outs[0].line == "cb\n", str(outs))

    e = box.run_code("1 + 2", language="javascript")
    check("javascript 语言", bool(e.results) and e.results[0].text == "3"
          and e.error is None, str([r.text for r in e.results]) + str(e.error))

    try:
        box.run_code("import time; time.sleep(30)", timeout=2)
        check("run_code 执行超时", False, "no exception")
    except TimeoutException:
        check("run_code 执行超时", True)

    section("code-interpreter context")
    ctx = box.create_code_context()
    check("创建 context", isinstance(ctx.id, str) and ctx.id != "", ctx.id)
    box.run_code("ctx_var = 'in-ctx'", context=ctx)
    e = box.run_code("ctx_var", context=ctx)
    check("context 内状态", bool(e.results) and e.results[0].text == "in-ctx",
          str([r.text for r in e.results]))
    e = box.run_code("'ctx_var' in dir()")
    check("context 相互隔离", bool(e.results) and e.results[0].text == "False",
          str([r.text for r in e.results]))

    box.restart_code_context(ctx)
    e = box.run_code("'ctx_var' in dir()", context=ctx)
    check("restart 清空 context", bool(e.results) and e.results[0].text == "False",
          str([r.text for r in e.results]))

    ctxs = box.list_code_contexts()
    check("list_code_contexts", ctx.id in [c.id for c in ctxs], str([c.id for c in ctxs]))
    box.remove_code_context(ctx)
    check("remove_code_context", ctx.id not in [c.id for c in box.list_code_contexts()])


# ---------------------------------------------------------------- checkpoint
def test_checkpoint(client, template):
    section("checkpoint 快照")
    box = Sandbox.create(template, secure=False, timeout=600, request_timeout=180,
                         metadata={"suite": SUITE, "phase": "checkpoint"})
    name = f"{SUITE[:26]}-ckpt:v1"
    try:
        # 指定名称捕获
        box.files.write("/home/user/checkpoint.txt", "1")
        snap = box.create_snapshot(name=name, request_timeout=180)
        check("指定名称捕获", snap.snapshot_id == name and snap.names == [name],
              f"{snap.snapshot_id} {snap.names}")

        # 同名再次捕获（更新同一模板）
        box.files.write("/home/user/checkpoint.txt", "2")
        snap2 = box.create_snapshot(name=name, request_timeout=180)
        check("同名更新", snap2.snapshot_id == name and snap2.names == [name],
              f"{snap2.snapshot_id} {snap2.names}")

        # 捕获后源沙箱继续可用
        alive = box.commands.run("printf alive").stdout == "alive"
        check("捕获后源沙箱可用", alive and box.files.read("/home/user/checkpoint.txt") == "2", "")

        # 自动命名
        anon = box.create_snapshot(request_timeout=180)
        check("自动命名捕获", anon.snapshot_id.startswith("checkpoint-") and anon.names == [],
              f"{anon.snapshot_id} {anon.names}")

        # 错误路径
        r = client.post(f"/sandboxes/{box.sandbox_id}/snapshots", json={"name": "  "})
        expect_status("空名称 400", r, 400)
        r = client.post(f"/sandboxes/{box.sandbox_id}/snapshots",
                        json={"name": "sha256:6aaeaa67ce540956e8b09051ef5add2932c73a1f05ca4b4ff2a2d28202d20b96"})
        expect_status("digest 名称 400", r, 400)
        r = client.post(f"/sandboxes/{box.sandbox_id}/snapshots", json={"bogus": 1})
        expect_status("未知字段 400", r, 400)
        r = client.post(f"/sandboxes/{uuid.uuid4()}/snapshots", json={})
        expect_status("未知沙箱 404", r, 404)

        # list_snapshots 已实现：SDK 分页拉全量，核对名称映射与过滤
        listed = _iter_snapshots(box)
        by_id = {snap.snapshot_id: snap for snap in listed}
        check("list_snapshots 含指定名快照", name in by_id and by_id[name].names == [name],
              str({k: v.names for k, v in by_id.items()})[:200])
        check("list_snapshots 自动名快照 names 为空",
              anon.snapshot_id in by_id and by_id[anon.snapshot_id].names == [],
              str(by_id.get(anon.snapshot_id)))
        r = client.get("/snapshots", params={"sandboxID": box.sandbox_id, "name": name})
        expect_status("GET /snapshots name 过滤 200", r, 200)
        filtered = r.json()
        check("name 过滤命中别名", [s["snapshotID"] for s in filtered] == [name],
              r.text[:200])
        r = client.get("/snapshots", params={"sandboxID": box.sandbox_id, "limit": 1})
        expect_status("GET /snapshots limit 200", r, 200)
        check("limit=1 单条", len(r.json()) == 1, r.text[:120])
        check("limit=1 附 nextToken", r.headers.get("X-Next-Token") not in (None, ""),
              str(r.headers.get("X-Next-Token")))
        # delete_snapshot 未实现：SDK 映射为 DELETE /templates/{id}，命中显式 501 前缀
        expect_sdk_status("delete_snapshot 501", lambda: box.delete_snapshot("nonexistent"), 501)
    finally:
        try:
            box.kill()
        except Exception:
            pass

    # 恢复验证：快照模板注册在本地节点，直接经同一 API 创建即可
    # （多节点版需探测归属节点端口，单机无此问题）
    section("checkpoint 恢复（本地模板）")
    restored = None
    try:
        restored = Sandbox.create(name, secure=False, timeout=120, request_timeout=180,
                                  metadata={"suite": SUITE, "phase": "restore"})
        check("从快照模板恢复创建", True)
        check("恢复内容一致", restored.files.read("/home/user/checkpoint.txt") == "2", "")
        check("恢复后 envd 可执行命令",
              restored.commands.run("printf ok").stdout == "ok", "")
    except Exception as err:
        check("从快照模板恢复创建", False, f"{type(err).__name__}: {str(err)[:120]}")
    finally:
        if restored is not None:
            sandbox_id = restored.sandbox_id
            try:
                restored.kill()
            except Exception:
                pass
            expect_status("恢复后 kill 404", client.get(f"/sandboxes/{sandbox_id}"), 404)


def _iter_snapshots(box):
    """经 SDK 分页器拉取全部快照（X-Next-Token 游标），返回 SnapshotInfo 列表。"""
    paginator = box.list_snapshots()
    collected = []
    while paginator.has_next:
        collected.extend(paginator.next_items())
    return collected


# ---------------------------------------------------------------- 数据面
def test_dataplane(box, client):
    # 数据面前置探测：envd /health 经 E2b 头路由应得 204；不可达则按约定跳过本节
    probe = client.get("/health", headers=routing_headers(box, 49983))
    if probe.status_code != 204:
        skip_section("数据面 sandboxproxy", f"envd /health 经路由头返回 {probe.status_code}，代理不可用")
        return

    section("数据面 sandboxproxy（节点内嵌）")
    # maskRequestHost：上游看到改写后的 Host（${PORT} 替换），原 Host 进 X-Forwarded-Host
    masked = Sandbox.create(os.environ["E2B_TEMPLATE_ID"], secure=False, timeout=300,
                            request_timeout=180, metadata={"suite": SUITE, "phase": "mask"},
                            network=SandboxNetworkOpts(mask_request_host="app-${PORT}.masked.example"))
    try:
        masked.files.write("/tmp/conch-e2e-app.py", APPLICATION)
        masked.commands.run("python3 /tmp/conch-e2e-app.py >/tmp/conch-e2e-app.log 2>&1",
                            background=True, timeout=0)
        masked_routing = routing_headers(masked, 8080)
        eventually(lambda: client.get("/app-ready", headers=masked_routing),
                   lambda resp: resp.status_code == 200, "掩码沙箱应用就绪")
        r = client.get("/host-check", headers=masked_routing)
        payload = r.json()
        check("maskRequestHost Host 改写", payload.get("host") == "app-8080.masked.example",
              str(payload))
        check("X-Forwarded-Host 保留原值", payload.get("forwarded_host") not in (None, ""),
              str(payload))
    finally:
        masked.kill()

    box.files.write("/tmp/conch-e2e-app.py", APPLICATION)
    box.commands.run("python3 /tmp/conch-e2e-app.py >/tmp/conch-e2e-app.log 2>&1",
                     background=True, timeout=0)
    routing = routing_headers(box, 8080)
    eventually(lambda: client.get("/app-ready", headers=routing),
               lambda r: r.status_code == 200, "访客 HTTP 应用就绪")

    r = client.get("/encoded%2Fpath?q=a%2Fb", headers={**routing, "X-Conch-Marker": "m1"})
    check("E2b 头路由 + 转义路径", r.status_code == 200 and r.json().get("path") == "/encoded%2Fpath?q=a%2Fb"
          and r.json().get("marker") == "m1", r.text[:80])

    r = client.get("/app-ready", headers={
        "X-Agentenv-Sandbox-Id": box.sandbox_id, "X-Agentenv-Target-Port": "8080"})
    check("X-Agentenv 头路由", r.status_code == 200, str(r.status_code))

    r = client.get("/proxy/8080/app-ready", headers={
        "X-Agentenv-Sandbox-Id": box.sandbox_id, "X-Agentenv-Target-Port": "8080"})
    check("/proxy 前缀路由", r.status_code == 200, f"{r.status_code} {r.text[:80]}")

    check("envd /health 经代理", probe.status_code == 204, str(probe.status_code))

    r = client.get("/app-ready", headers={"e2b-sandbox-port": "8080"})
    check("缺 sandbox id 400/401/404", r.status_code in (400, 401, 404), str(r.status_code))
    r = client.get("/app-ready", headers={"e2b-sandbox-id": box.sandbox_id,
                                          "e2b-sandbox-port": "0"})
    check("非法端口 400", r.status_code == 400, str(r.status_code))
    r = client.get("/app-ready", headers={"e2b-sandbox-id": str(uuid.uuid4()),
                                          "e2b-sandbox-port": "8080"})
    check("未知沙箱 404", r.status_code == 404, str(r.status_code))

    started = time.monotonic()
    first = None
    lines = []
    with client.stream("GET", "/stream", headers=routing) as response:
        for line in response.iter_lines():
            if line:
                if first is None:
                    first = time.monotonic() - started
                lines.append(line)
    check("SSE 全部行", lines == ["data: 0", "data: 1", "data: 2"], str(lines))
    check("SSE 首行及时下发", first is not None and first < 0.8, str(first))

    ws_url = API_URL.replace("https://", "wss://", 1).replace("http://", "ws://", 1) + "/ws"
    with connect(ws_url, additional_headers=routing, proxy=None) as websocket:
        for message in ["Conch WebSocket 文本", b"\x00\x01\xff"]:
            websocket.send(message)
            check(f"WS 回显 {'文本' if isinstance(message, str) else '二进制'}",
                  websocket.recv() == message)
        big = b"x" * 65536
        websocket.send(big)
        check("WS 64KB 大帧回显", websocket.recv() == big)
        pong = websocket.ping(b"pingdata")
        check("WS ping/pong", pong.wait() is True)

    # Host 域名路由（get_host/app URL）：仅当节点配置了 sandbox_proxy_domains
    # （create 响应携带 domain）时执行，否则整节跳过
    if box.sandbox_domain and box.sandbox_domain != "e2b.app":
        section("数据面 Host 域名路由（sandbox_proxy_domains 已配置）")
        host = box.get_host(8080)
        check("get_host 域名格式", re.match(r"^\d+-[0-9a-f-]{36}\.\S+$", host) is not None, host)
        r = client.get("/hello", headers={"Host": host})
        check("Host 路由 GET", r.status_code == 200 and r.json()["path"] == "/hello",
              f"{r.status_code} {r.text[:60]}")
        host_lines = []
        with client.stream("GET", "/stream", headers={"Host": host}) as response:
            for line in response.iter_lines():
                if line:
                    host_lines.append(line)
        check("Host 路由 SSE", host_lines == ["data: 0", "data: 1", "data: 2"], str(host_lines))
        r = client.get("/hello", headers={"Host": host, "e2b-sandbox-id": str(uuid.uuid4()),
                                          "e2b-sandbox-port": "1"})
        check("Host 优先于冲突路由头", r.status_code == 200 and r.json()["path"] == "/hello",
              f"{r.status_code} {r.text[:60]}")
        import base64, socket as _socket
        ws_key = base64.b64encode(os.urandom(16)).decode()
        req = (f"GET /ws HTTP/1.1\r\nHost: {host}\r\nUpgrade: websocket\r\n"
               f"Connection: Upgrade\r\nSec-WebSocket-Key: {ws_key}\r\n"
               f"Sec-WebSocket-Version: 13\r\n\r\n")
        addr = api_host_port()
        sock = _socket.create_connection(addr, timeout=10)
        sock.sendall(req.encode())
        resp = b""
        while b"\r\n\r\n" not in resp:
            resp += sock.recv(4096)
        sock.close()
        check("Host 路由 WS 握手 101", resp.startswith(b"HTTP/1.1 101"),
              resp[:40].decode(errors="replace"))
    else:
        skip_section("数据面 Host 域名路由", "节点未配置 sandbox_proxy_domains（get_host/app URL 不可用）")


# ---------------------------------------------------------------- pause/lifecycle
def test_pause_connect(client, template):
    """pause/connect/set_timeout 语义：pause 释放资源保留记录、connect 恢复同 ID。"""
    section("pause/connect 语义")
    box = Sandbox.create(template, secure=False, timeout=300, request_timeout=180,
                         metadata={"suite": SUITE, "phase": "pause"},
                         envs={"CONCH_E2E_VALUE": "from-create"})
    sandbox_id = box.sandbox_id
    try:
        box.commands.run("echo pause-marker > /home/user/marker.txt")
        # 全内存暂停的核心语义：后台心跳进程应在 pause→connect 后继续运行
        box.commands.run("while true; do date +%s >> /tmp/alive.txt; sleep 1; done",
                         background=True, timeout=0)
        pre_pause = int(eventually(lambda: last_heartbeat(box),
                                   lambda v: v.isdigit(), "心跳进程写入", timeout=15))
        paused = box.pause()
        check("pause() 返回 True", paused is True, str(paused))
        # 暂停后 get_info 显示 paused
        info = box.get_info()
        check("暂停后 state=paused", info.state == "paused", str(info.state))
        # 再次 pause 返回 False（409 → SDK False）
        repaused = box.pause()
        check("二次 pause() 返回 False", repaused is False, str(repaused))
        # memory=false（仅磁盘暂停）不支持
        r = client.post(f"/sandboxes/{sandbox_id}/pause", json={"memory": False})
        expect_status("pause memory=false 400", r, 400)
        # paused 态下 TTL/网络操作均要求运行中 → 409
        r = client.post(f"/sandboxes/{sandbox_id}/timeout", json={"timeout": 60})
        expect_status("set_timeout 对 paused 409", r, 409)
        r = client.post(f"/sandboxes/{sandbox_id}/refreshes", json={"duration": 30})
        expect_status("refreshes 对 paused 409", r, 409)
        r = client.put(f"/sandboxes/{sandbox_id}/network", json={"allowOut": []})
        expect_status("update_network 对 paused 409", r, 409)
        # 暂停沙箱不在运行计数中，但仍可列出
        r = client.get("/sandboxes", params={"state": "paused", "limit": 100})
        expect_status("paused 过滤 200", r, 200)
        check("paused 列表含已暂停沙箱", any(
            s["sandboxID"] == sandbox_id for s in r.json()), r.text[:200])
        # 未开 autoResume 的暂停沙箱：数据面请求唤醒被拒（503，而非 404）
        r = client.get("/health", headers=routing_headers(box, 49983))
        expect_status("paused 无 autoResume 数据面 503", r, 503)
    finally:
        pass  # 连接恢复在下方验证

    # connect 恢复同 ID（201 路径），文件保留
    restored = Sandbox.connect(sandbox_id, timeout=300, request_timeout=180)
    try:
        check("connect 恢复同 ID", restored.sandbox_id == sandbox_id, restored.sandbox_id)
        content = restored.files.read("/home/user/marker.txt")
        check("暂停文件系统保留", "pause-marker" in content, repr(content))
        env_out = restored.commands.run("printf %s \"$CONCH_E2E_VALUE\"").stdout
        check("恢复后 create envs 保留", env_out == "from-create", repr(env_out))
        try:
            post_alive = eventually(lambda: last_heartbeat(restored),
                                    lambda v: v.isdigit() and int(v) > pre_pause,
                                    "恢复后心跳继续写入", timeout=20)
            check("pause 全内存保留进程", int(post_alive) > pre_pause,
                  f"pre={pre_pause} post={post_alive}")
        except AssertionError as err:
            check("pause 全内存保留进程", False, str(err)[:120])
        info = restored.get_info()
        check("恢复后 state=running", info.state == "running", str(info.state))
        # 运行中 connect 仅延长 TTL（200 路径）
        restored.connect(timeout=600)
        info = restored.get_info()
        import datetime as _dt
        remaining = (info.end_at - _dt.datetime.now(_dt.timezone.utc)).total_seconds()
        check("运行中 connect 只延长", remaining > 590, str(remaining))
    finally:
        restored.kill()
    expect_status("恢复后 kill 404", client.get(f"/sandboxes/{sandbox_id}"), 404)

    # 并发 connect：多个请求都读到 paused，后到者等先到者恢复完成（joined 请求 201），
    # 不再 409 "sandbox is already running"；交错到达的（读到 CREATING）经 awaitResume 同样 201。
    section("并发 connect 恢复")
    import threading
    race_box = Sandbox.create(template, secure=False, timeout=300, request_timeout=180,
                              metadata={"suite": SUITE, "phase": "connect-race"})
    race_id = race_box.sandbox_id
    race_box.pause()
    outcomes = {}

    def _connect(tag, delay):
        time.sleep(delay)
        try:
            outcomes[tag] = Sandbox.connect(race_id, timeout=600, request_timeout=180)
        except Exception as exc:  # noqa: BLE001
            outcomes[tag] = exc

    racers = [threading.Thread(target=_connect, args=(tag, delay))
              for tag, delay in (("a", 0.0), ("b", 0.5), ("c", 1.0))]
    for th in racers:
        th.start()
    for th in racers:
        th.join(300)
    for tag in ("a", "b", "c"):
        check(f"并发 connect {tag} 成功", isinstance(outcomes.get(tag), Sandbox), repr(outcomes.get(tag)))
    winner = next((v for v in outcomes.values() if isinstance(v, Sandbox)), None)
    if winner is not None:
        try:
            check("并发 connect 同 ID", winner.sandbox_id == race_id, winner.sandbox_id)
            info = winner.get_info()
            check("并发 connect 后 running", info.state == "running", str(info.state))
            import datetime as _dt
            remaining = (info.end_at - _dt.datetime.now(_dt.timezone.utc)).total_seconds()
            # joined 请求不改期限：截止时间就是先到者那次恢复的 600s，不会叠加延长。
            check("并发 connect 期限为单次 600s", 590 < remaining <= 605, str(remaining))
            check("并发 connect 后命令可执行", winner.commands.run("echo ok").stdout.strip() == "ok", "")
        finally:
            winner.kill()

    # paused 态可直接删除（RemoveSandbox 不要求运行中）
    section("kill paused 沙箱")
    paused_box = Sandbox.create(template, secure=False, timeout=300, request_timeout=180,
                                metadata={"suite": SUITE, "phase": "kill-paused"})
    paused_box.pause()
    r = client.delete(f"/sandboxes/{paused_box.sandbox_id}")
    expect_status("DELETE paused 沙箱 204", r, 204)
    expect_status("DELETE paused 后 404", client.get(f"/sandboxes/{paused_box.sandbox_id}"), 404)


def test_timeout_action_pause(client, template):
    """lifecycle on_timeout=pause：到期自动暂停而非删除。"""
    section("on_timeout=pause 到期自动暂停")
    box = Sandbox.create(template, secure=False, timeout=5, request_timeout=180,
                         metadata={"suite": SUITE, "phase": "auto-pause"},
                         lifecycle={"on_timeout": "pause"})
    sandbox_id = box.sandbox_id
    box.commands.run("echo autopause > /home/user/auto.txt")
    state = eventually(
        lambda: box.get_info().state,
        lambda s: s == "paused", "到期自动暂停", timeout=30)
    check("到期自动暂停 state=paused", state == "paused", str(state))
    restored = Sandbox.connect(sandbox_id, timeout=120, request_timeout=180)
    try:
        check("自动暂停后 connect 恢复", restored.files.read("/home/user/auto.txt").strip() == "autopause", "")
    finally:
        restored.kill()


def test_auto_resume_wake(client, template):
    """autoResume 数据面唤醒：on_timeout=pause + auto_resume=True 的沙箱到期
    自动暂停后，数据面请求经代理直接恢复（无需 connect），授予 300s TTL 下限；
    运行中的数据面请求不再改动 TTL。"""
    section("autoResume 数据面唤醒")
    box = Sandbox.create(template, secure=False, timeout=5, request_timeout=180,
                         metadata={"suite": SUITE, "phase": "wake"},
                         lifecycle={"on_timeout": "pause", "auto_resume": True})
    box.commands.run("echo wake-marker > /home/user/wake.txt")
    # create 参数透传到 detail 的 lifecycle（onTimeout 用上游 kill/pause 词表）
    r = client.get(f"/sandboxes/{box.sandbox_id}")
    lifecycle = (r.json() or {}).get("lifecycle") or {}
    check("detail lifecycle.autoResume", lifecycle.get("autoResume") is True, r.text[:160])
    check("detail lifecycle.onTimeout=pause", lifecycle.get("onTimeout") == "pause",
          r.text[:160])
    eventually(lambda: box.get_info().state, lambda s: s == "paused",
               "到期自动暂停", timeout=30)
    # 数据面请求唤醒：恢复检查点、重发路由并转发本次请求（envd /health 204）
    r = client.get("/health", headers=routing_headers(box, 49983))
    expect_status("数据面唤醒 envd /health 204", r, 204)
    info = box.get_info()
    check("唤醒后 state=running", info.state == "running", str(info.state))
    import datetime as _dt
    remaining = (info.end_at - _dt.datetime.now(_dt.timezone.utc)).total_seconds()
    check("唤醒授予 300s TTL 下限", 290 <= remaining <= 305, str(remaining))
    check("唤醒后文件保留", "wake-marker" in box.files.read("/home/user/wake.txt"), "")
    # 运行中的数据面请求是 no-op 唤醒：截止时间不动
    info2 = box.get_info()
    check("运行中数据面请求不动 TTL", info2.end_at == info.end_at,
          f"{info.end_at} vs {info2.end_at}")
    # 并发唤醒：第一笔请求触发恢复并阻塞至完成；恢复期间（有世代、无路由）
    # 到达的请求只等路由出现，不再二次唤醒——并行请求不会批量 502
    box.pause()
    eventually(lambda: box.get_info().state, lambda s: s == "paused", "再次暂停", timeout=30)
    import threading
    results = {}

    def _wake_probe(tag):
        results[tag] = client.get("/health", headers=routing_headers(box, 49983))

    first = threading.Thread(target=_wake_probe, args=("first",))
    first.start()
    time.sleep(1.0)  # 首笔已过 Begin，后续请求落在引导窗口（或恢复已完成，同样 204）
    late = [threading.Thread(target=_wake_probe, args=(f"late{i}",)) for i in range(2)]
    for t in late:
        t.start()
    for t in [first, *late]:
        t.join()
    for tag in ["first", "late0", "late1"]:
        expect_status(f"并发唤醒 {tag} 204", results[tag], 204)
    box.kill()


def test_lifecycle(client, template):
    """单节点生命周期：无 /nodes 集群视图，回收以沙箱列表为准。

    节点不暴露指标（get_metrics 501），资源回收由 Manager 维护循环保证，
    此处以"列表中不再出现本套件沙箱"验证删除生效。
    """
    section("生命周期与回收（单节点）")
    boxes = []
    try:
        for _ in range(2):
            boxes.append(Sandbox.create(template, secure=False, timeout=300,
                                        request_timeout=180, metadata={"suite": SUITE}))
        listed = eventually(lambda: suite_sandboxes(client),
                            lambda xs: {s["sandboxID"] for s in xs} >=
                            {b.sandbox_id for b in boxes},
                            "创建后列表可见", timeout=30)
        check("两个沙箱均在列表", len(listed) >= 2,
              str([(s["sandboxID"], s["state"]) for s in listed]))
    finally:
        for box in boxes:
            try:
                box.kill()
            except Exception as err:
                print(f"  警告: kill {box.sandbox_id} 异常: {err}")
    eventually(lambda: suite_sandboxes(client), lambda xs: xs == [],
               "kill 后列表回收", timeout=120)

    section("timeout 到期自动删除")
    expiring = Sandbox.create(template, secure=False, timeout=3, request_timeout=180,
                              metadata={"suite": SUITE, "phase": "expire"})
    status = eventually(lambda: client.get(f"/sandboxes/{expiring.sandbox_id}").status_code,
                        lambda code: code == 404, "初始 timeout 删除沙箱", timeout=20)
    check("到期 404", status == 404, str(status))
    killed = expiring.kill()
    check("到期后 kill 返回 False", killed is False, str(killed))
    eventually(lambda: suite_sandboxes(client), lambda xs: xs == [],
               "到期回收后列表清空", timeout=90)

    # set_timeout(0)：SDK 必填参数透传、无矫正，直发 {"timeout": 0}；
    # deadline=now 返回 204，由回收循环立即删除
    section("set_timeout(0) 立即到期删除")
    zeroed = Sandbox.create(template, secure=False, timeout=300, request_timeout=180,
                            metadata={"suite": SUITE, "phase": "set-timeout-0"})
    zeroed.set_timeout(0)
    check("set_timeout(0) SDK 2xx 生效", True)
    status = eventually(lambda: client.get(f"/sandboxes/{zeroed.sandbox_id}").status_code,
                        lambda code: code == 404, "set_timeout(0) 删除沙箱", timeout=20)
    check("set_timeout(0) 后 404", status == 404, str(status))
    killed = zeroed.kill()
    check("set_timeout(0) 后 kill 返回 False", killed is False, str(killed))
    eventually(lambda: suite_sandboxes(client), lambda xs: xs == [],
               "set_timeout(0) 回收后列表清空", timeout=90)


# ---------------------------------------------------------------- 模板清理
def test_template_cleanup(client):
    """收尾清理：删除本套件产生的 checkpoint 残留模板。

    test_checkpoint 产生 *-ckpt:v1 与自动命名 checkpoint-*；pause 失败/
    崩溃路径可能残留 pause-*（正常 resume 后节点会自行删除）。
    E2B Node API 无模板管理端点，需本机 conch CLI（经 unix socket 访问
    conchd）；按 origin=checkpoint 过滤，image 模板（基础模板等）天然保留。
    仍被 paused 沙箱引用的模板是恢复锚点（RemoveTemplate 不检查引用），
    必须跳过。conchd 以 root 运行时 unix socket 可能需要权限，
    可设 CONCH_CLI="sudo conch"。
    """
    section("模板清理")
    cli = shlex.split(os.environ.get("CONCH_CLI", "conch"))
    if shutil.which(cli[0]) is None:
        # 回退到仓库构建产物 bin/conch（make build），相对本脚本定位
        fallback = os.path.join(os.path.dirname(__file__), "..", "..", "bin", "conch")
        if os.path.isfile(fallback):
            cli[0] = os.path.abspath(fallback)
        else:
            skip_section("模板清理", f"conch CLI 不可用（{cli[0]}，可用 CONCH_CLI 指定路径）")
            return
    config_args = []
    if os.environ.get("CONCH_CLI_CONFIG"):
        config_args = ["--config", os.environ["CONCH_CLI_CONFIG"]]

    # 仍处 paused 的沙箱的恢复锚点模板（RemoveTemplate 无引用检查，删了无法
    # connect）。pause 模板的 SOURCE_SANDBOX 即该沙箱 ID（paused 记录的
    # templateID 仍是原 image 模板，CheckpointHeadTemplateID 不在 API 输出中，
    # 故按 SOURCE_SANDBOX 匹配）。单节点直接查本节点 API。
    paused_ids = set()
    try:
        for s in client.get("/sandboxes", params={"state": "paused", "limit": 100}).json():
            if s.get("sandboxID"):
                paused_ids.add(s["sandboxID"])
    except Exception as err:
        print(f"  警告: paused 沙箱列表获取失败: {err}")

    proc = subprocess.run(cli + ["template", "ls", "--origin", "checkpoint"] + config_args,
                          capture_output=True, text=True, timeout=120)
    if proc.returncode != 0:
        check("checkpoint 残留模板清理", False,
              f"conch template ls 失败（可设 CONCH_CLI/CONCH_CLI_CONFIG）: {proc.stderr.strip()[:120]}")
        return
    entries = []
    for line in proc.stdout.splitlines()[1:]:
        columns = line.split()
        # NAME TEMPLATE_ID ORIGIN BOOT_MODE SOURCE_REF SOURCE_SANDBOX
        if len(columns) >= 6:
            entries.append((columns[0], columns[5]))

    removed, skipped, failed = [], [], []
    for name, source_sandbox in entries:
        if source_sandbox != "-" and source_sandbox in paused_ids:
            skipped.append(name)
            continue
        proc = subprocess.run(cli + ["template", "rm", name] + config_args,
                              capture_output=True, text=True, timeout=300)
        if proc.returncode == 0:
            removed.append(name)
        else:
            failed.append(f"rm {name}: {proc.stderr.strip()}")
    check("checkpoint 残留模板清理", not failed,
          f"removed={len(removed)} skipped={skipped} errors={failed}")
    if removed:
        print(f"  已删除 {len(removed)} 个: " + ", ".join(removed[:8]) +
              (" ..." if len(removed) > 8 else ""))


def main():
    for variable in ["E2B_API_KEY", "E2B_API_URL", "E2B_TEMPLATE_ID"]:
        if not os.getenv(variable):
            raise SystemExit(f"运行前需设置 {variable}")
    # 单机直连：数据面（sandboxproxy）与控制面共用 e2b.listen_addr 同一监听，
    # SDK commands/files/pty 经 E2b-Sandbox-Id/Port 头路由；未显式设置时默认同址。
    if not os.environ.get("E2B_SANDBOX_URL"):
        os.environ["E2B_SANDBOX_URL"] = API_URL
        print(f"E2B_SANDBOX_URL 未设置，默认取 E2B_API_URL: {API_URL}")

    template = os.environ["E2B_TEMPLATE_ID"]
    box = None
    try:
        with httpx.Client(base_url=API_URL, timeout=180,
                          headers={"X-API-Key": os.environ["E2B_API_KEY"]}) as client:
            initial = client.get("/sandboxes", params={"limit": 100}).json()
            if initial:
                print(f"警告: 节点已有 {len(initial)} 个沙箱，回收检查可能受干扰")

            test_control_plane(client, template)

            box = Sandbox.create(template, secure=False, timeout=900, request_timeout=180,
                                 metadata={"suite": SUITE, "phase": "envd"},
                                 envs={"CONCH_E2E_VALUE": "from-create"})
            # 数据面探测：SDK 经 E2B_SANDBOX_URL 执行一条命令，失败则跳过所有
            # 依赖 envd 数据面的用例（控制面用例不受影响）
            try:
                box.commands.run("true", request_timeout=30)
                data_ok = True
            except Exception as err:
                data_ok = False
                print(f"  沙箱数据面不可达: {type(err).__name__}: {str(err)[:120]}")

            if data_ok:
                test_envd(box)
                test_dataplane(box, client)
                box.kill()
                box = CISandbox.create(template, secure=False, timeout=900,
                                       request_timeout=180, metadata={"suite": SUITE})
                test_code_interpreter(box)
                box.kill()
                box = None
                test_checkpoint(client, template)
                test_pause_connect(client, template)
                test_timeout_action_pause(client, template)
                test_auto_resume_wake(client, template)
            else:
                box.kill()
                box = None
                for name in ["envd 数据接口", "数据面 sandboxproxy", "code-interpreter",
                             "checkpoint 快照与恢复", "pause/connect 语义",
                             "on_timeout=pause", "autoResume 数据面唤醒"]:
                    skip_section(name, "沙箱数据面不可达（E2B_SANDBOX_URL 路由失败）")

            test_lifecycle(client, template)
            test_template_cleanup(client)
    finally:
        if box is not None:
            try:
                box.kill()
            except Exception:
                pass

    print(f"\n{'=' * 50}")
    print(f"通过: {len(PASS)}  失败: {len(FAIL)}  跳过: {len(SKIPPED)}")
    for name, reason in SKIPPED:
        print(f"  SKIP {name} :: {reason}")
    for name, detail in FAIL:
        print(f"  FAIL {name} :: {detail}")
    if FAIL:
        sys.exit(1)


if __name__ == "__main__":
    try:
        main()
    except AssertionError:
        traceback.print_exc()
        sys.exit(2)
