import os
import socket

from conch import Sandbox

policy = {
    "default_action": "deny",
    "rules": [
        {
            "action": "allow",
            "protocol": "https",
            "host": "google.com",
            "port": 443,
        },
        {
            "action": "allow",
            "protocol": "https",
            "host": "github.io",
            "port": 443,
        },
        {
            "action": "allow",
            "protocol": "https",
            "host": "*.github.io",
            "port": 443,
        },
        {
            "action": "deny",
            "protocol": "https",
            "host": "bing.com",
            "port": 443,
        },
    ],
}


def request(sandbox, name, url, expected=None):
    result = sandbox.commands.run(
        cmd="curl",
        args=[
            "-4",
            "-sS",
            "--noproxy", "*",
            "--connect-timeout", "5",
            "--max-time", "15",
            "-o", f"/tmp/{name}.body",
            "-w", "%{http_code}",
            url,
        ],
        timeout=20,
    )

    print(
        name,
        "status =", result.stdout,
        "stderr =", result.stderr.strip(),
    )
    if expected is not None:
        status = result.stdout.strip()
        if expected == "deny":
            matched = status == "403"
        else:
            matched = status.isdigit() and 200 <= int(status) < 400
        print("expected =", expected, "result =", "MATCH" if matched else "UNEXPECTED")


sbx = Sandbox.create(
    template_name=os.environ["TEMPLATE_NAME"],
    sandbox_id="external-egress-test",
    network=policy,
)
try:
    request(sbx, "google", "https://google.com/")
    request(sbx, "github_pages_root", "https://github.io/")
    request(sbx, "github_pages_wildcard", "https://spotify.github.io/")
    request(sbx, "bing", "https://bing.com/", "deny")
finally:
    sbx.delete()

# Run on the conchd host so this lookup uses its resolver. Envoy's cached or
# later DNS result can still differ; inspect its authorization log if a case disagrees.
host = os.environ.get("EGRESS_TEST_HOST", "example.com")
ips = sorted({
    address[4][0]
    for address in socket.getaddrinfo(host, 80, socket.AF_INET, socket.SOCK_STREAM)
})
if not ips:
    raise RuntimeError(f"No IPv4 addresses found for {host}")
print("HTTP policy test host =", host, "current IPv4 addresses =", ips)
print("These cases require reachable HTTP service and matching Envoy DNS results.")
print("Confirm denials in conchd authorization logs; HTTP 403 alone does not identify its source.")

allow_domain = {"action": "allow", "protocol": "http", "host": host}
deny_domain = {"action": "deny", "protocol": "http", "host": host}
cases = [
    (
        "allow-domain-deny-ip", "deny",
        {"rules": [allow_domain], "denyOut": ips},
    ),
    (
        "deny-domain-allow-ip", "deny",
        {"rules": [deny_domain], "allowOut": ips},
    ),
    (
        "allow-domain-other-ip", "allow",
        {"rules": [allow_domain], "allowOut": ["192.0.2.1"]},
    ),
    (
        "allow-other-domain-exact-ip", "allow",
        {
            "rules": [{"action": "allow", "protocol": "http", "host": "unrelated.invalid"}],
            "allowOut": ips,
        },
    ),
]

for name, expected, network in cases:
    network["default_action"] = "deny"
    sandbox = Sandbox.create(
        template_name=os.environ["TEMPLATE_NAME"],
        sandbox_id=f"egress-{name}",
        network=network,
    )
    try:
        print("\ncase =", name, "policy =", network)
        request(sandbox, name, f"http://{host}/", expected)
    finally:
        sandbox.delete()
