#!/bin/sh
set -eu

export HOME=/root
export PATH=/root/.server/.venv/bin:/usr/local/bin:/usr/bin:/bin:/sbin:/usr/sbin
export ENVD_DISABLE_MMDS=1
export ENVD_DISABLE_PORT_FORWARDER=1
export E2B_LOCAL="${E2B_LOCAL:-1}"

CONCH_LOG_DIR=/var/log/conch-init
CONCH_RUN_DIR=/run/conch
CONCH_LOG="$CONCH_LOG_DIR/conch-init.log"
CONCH_READY_FILE="$CONCH_RUN_DIR/services-ready"
CONCH_SERVICE_LOG="${CONCH_SERVICE_LOG:-$CONCH_LOG_DIR/service.log}"
CONCH_ENVD_LOG="$CONCH_LOG_DIR/envd.log"
CONCH_CODE_INTERPRETER_LOG="$CONCH_LOG_DIR/code-interpreter.log"

log() {
    timestamp="$(date '+%Y-%m-%d %H:%M:%S.000')"
    prefix=""
    if [ -n "${CONCH_SANDBOX_ID:-}" ]; then
        prefix="[$CONCH_SANDBOX_ID] "
    fi
    line="$timestamp [INFO] conch-entrypoint:0 ${prefix}$*"
    printf '%s\n' "$line" | tee -a "$CONCH_LOG" >&2
}

http_ready() {
    url="$1"
    expected_status="$2"
    expected_body="${3:-}"
    body_file="$(mktemp "$CONCH_RUN_DIR/ready-body.XXXXXX" 2>/dev/null || true)"
    if [ -z "$body_file" ]; then
        body_file="$CONCH_RUN_DIR/ready-body.$$"
    fi
    status="$(curl -fsS -m 0.2 -o "$body_file" -w '%{http_code}' "$url" 2>/dev/null || true)"
    if [ "$status" = "$expected_status" ]; then
        if [ -z "$expected_body" ] || grep -q "$expected_body" "$body_file" 2>/dev/null; then
            rm -f "$body_file"
            return 0
        fi
    fi
    rm -f "$body_file"
    return 1
}

core_services_ready() {
    http_ready http://127.0.0.1:49983/health 204 && \
        http_ready http://127.0.0.1:49999/health 200 OK
}

configure_hosts() {
    if [ ! -f /etc/hosts ]; then
        printf '%s\n' '127.0.0.1 localhost' '::1 ip6-localhost ip6-loopback' > /etc/hosts
        return
    fi
    grep -q '^127\.0\.0\.1[[:space:]].*localhost' /etc/hosts 2>/dev/null || \
        printf '%s\n' '127.0.0.1 localhost' >> /etc/hosts
}

start_sshd() {
    if command -v sshd >/dev/null 2>&1; then
        mkdir -p /run/sshd
        chmod 0755 /run/sshd || true
        ssh-keygen -A >>"$CONCH_SERVICE_LOG" 2>&1 || true
        if /usr/sbin/sshd -t >>"$CONCH_SERVICE_LOG" 2>&1; then
            /usr/sbin/sshd -D -e >>"$CONCH_SERVICE_LOG" 2>&1 &
        else
            log "sshd config test failed"
        fi
    fi
}

start_envd() {
    if [ -x /usr/bin/envd ]; then
        /usr/bin/envd -isnotfc -port 49983 >>"$CONCH_ENVD_LOG" 2>&1 &
    else
        log "envd binary not found"
    fi
}

start_jupyter() {
    MATPLOTLIBRC=/root/.config/matplotlib/.matplotlibrc jupyter server \
        --ip=127.0.0.1 \
        --port=8888 \
        --IdentityProvider.token="" \
        --ServerApp.allow_root=True \
        --ServerApp.allow_origin="*" \
        --ServerApp.allow_remote_access=True \
        --ServerApp.disable_check_xsrf=True \
        >>"$CONCH_SERVICE_LOG" 2>&1 &
}

start_code_interpreter() {
    while :; do
        (
            i=0
            while [ "$i" -lt 6000 ]; do
                if http_ready http://127.0.0.1:8888/api/status 200; then
                    cd /root/.server
                    exec .venv/bin/uvicorn main:app \
                        --host 0.0.0.0 \
                        --port 49999 \
                        --workers 1 \
                        --no-access-log \
                        --no-use-colors \
                        --timeout-keep-alive 640
                fi
                i=$((i + 1))
                sleep 0.05
            done
            log "jupyter not ready, code-interpreter not started"
            exit 1
        ) >>"$CONCH_CODE_INTERPRETER_LOG" 2>&1
        log "code-interpreter exited, restarting"
        sleep 1
    done &
}

publish_ready() {
    attempts="${1:-240}"
    i=0
    while [ "$i" -lt "$attempts" ]; do
        if core_services_ready; then
            : > "$CONCH_READY_FILE"
            kill -USR1 1 2>/dev/null || true
            return 0
        fi
        i=$((i + 1))
        sleep 0.05
    done
    log "rootfs services readiness timed out"
    return 1
}

start_services_ready_watcher() {
    (
        while :; do
            if core_services_ready; then
                : > "$CONCH_READY_FILE"
            else
                rm -f "$CONCH_READY_FILE"
            fi
            sleep 1
        done
    ) &
}

mkdir -p "$CONCH_LOG_DIR" "$CONCH_RUN_DIR" /tmp
touch "$CONCH_LOG" "$CONCH_ENVD_LOG" "$CONCH_CODE_INTERPRETER_LOG" "$CONCH_SERVICE_LOG"
chmod 0644 "$CONCH_LOG" "$CONCH_ENVD_LOG" "$CONCH_CODE_INTERPRETER_LOG" "$CONCH_SERVICE_LOG" || true
rm -f "$CONCH_READY_FILE"

configure_hosts
start_sshd
start_envd
start_jupyter
start_code_interpreter
publish_ready 1800 &
start_services_ready_watcher

wait
