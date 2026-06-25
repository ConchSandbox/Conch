ARG BASE_IMAGE=hub.oepkgs.net/openeuler/openeuler:24.03-lts-sp3

FROM ${BASE_IMAGE} AS envd-builder

ARG ENVD_ARCHIVE_URL=https://codeload.github.com/e2b-dev/infra/tar.gz/refs/tags/2026.22
ARG ENVD_COMMIT=56ac6105
ARG TARGETARCH
ARG GO_VERSION=1.26.3
ARG GO_DOWNLOAD_BASE=https://golang.google.cn/dl
ARG GOPROXY=https://goproxy.cn,direct

ENV GOTOOLCHAIN=local

RUN dnf install -y --setopt=install_weak_deps=False \
        ca-certificates \
        curl \
        gzip \
        tar \
    && dnf clean all \
    && rm -rf /var/cache/dnf

RUN curl --retry 5 --retry-delay 2 --retry-all-errors -fsSL \
        "$GO_DOWNLOAD_BASE/go${GO_VERSION}.linux-${TARGETARCH}.tar.gz" \
        -o /tmp/go.tar.gz \
    && tar -C /usr/local -xzf /tmp/go.tar.gz \
    && rm -f /tmp/go.tar.gz \
    && /usr/local/go/bin/go version

RUN mkdir -p /src/infra \
    && curl -fsSL "$ENVD_ARCHIVE_URL" \
        | tar -C /src/infra -xzf - --strip-components=1 \
    && cd /src/infra/packages/envd \
    && CGO_ENABLED=0 GOOS=linux GOARCH="$TARGETARCH" \
        /usr/local/go/bin/go build -trimpath -buildvcs=false -a \
        -ldflags "-X=main.commitSHA=${ENVD_COMMIT} -s -w -buildid=" \
        -o /out/envd

FROM ${BASE_IMAGE} AS code-interpreter-source

ARG CODE_INTERPRETER_ARCHIVE_URL=https://codeload.github.com/e2b-dev/code-interpreter/tar.gz/refs/tags/%40e2b%2Fcode-interpreter%402.5.0

RUN dnf install -y --setopt=install_weak_deps=False \
        ca-certificates \
        curl \
        gzip \
        tar \
    && dnf clean all \
    && rm -rf /var/cache/dnf

RUN mkdir -p /src/code-interpreter \
    && curl -fsSL "$CODE_INTERPRETER_ARCHIVE_URL" \
        | tar -C /src/code-interpreter -xzf - --strip-components=1 \
    && cd /src/code-interpreter \
    && mkdir -p \
        /out/server \
        /out/jupyter \
        /out/config/matplotlib \
        /out/ipython/profile_default/startup \
    && cp -a template/server/. /out/server/ \
    && cp template/requirements.txt /out/server/template-requirements.txt \
    && cp template/jupyter-healthcheck.sh /out/jupyter/ \
    && cp template/jupyter_server_config.py /out/jupyter/ \
    && cp template/matplotlibrc /out/config/matplotlib/.matplotlibrc \
    && cp template/ipython_kernel_config.py /out/ipython/profile_default/ \
    && cp -a template/startup_scripts/. /out/ipython/profile_default/startup/

FROM ${BASE_IMAGE}

ARG DEBUG_SSH_AUTHORIZED_KEY=""
ARG INSTALL_FULL_TEMPLATE_REQUIREMENTS=0

ENV ENVD_PORT=49983 \
    CODE_INTERPRETER_PORT=49999 \
    LANG=C.UTF-8 \
    LC_ALL=C.UTF-8 \
    PIP_INDEX_URL=https://pypi.tuna.tsinghua.edu.cn/simple \
    PIP_DISABLE_PIP_VERSION_CHECK=1 \
    NPM_CONFIG_REGISTRY=https://registry.npmmirror.com \
    PATH=/root/.server/.venv/bin:/usr/local/bin:/usr/bin:/bin:/sbin:/usr/sbin

RUN dnf install -y --setopt=install_weak_deps=False \
        ca-certificates \
        curl \
        gcc \
        gcc-c++ \
        nodejs \
        npm \
        iproute \
        jq \
        make \
        openssh-clients \
        openssh-server \
        procps-ng \
        python3 \
        python3-pip \
        shadow \
        sudo \
        which \
    && dnf clean all \
    && rm -rf /var/cache/dnf

RUN useradd --create-home --uid 1000 --shell /bin/bash user \
    && echo 'user ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/user \
    && chmod 0440 /etc/sudoers.d/user \
    && mkdir -p /root/.ssh \
    && chmod 0700 /root/.ssh \
    && if [ -n "$DEBUG_SSH_AUTHORIZED_KEY" ]; then \
        printf '%s\n' "$DEBUG_SSH_AUTHORIZED_KEY" > /root/.ssh/authorized_keys \
        && chmod 0600 /root/.ssh/authorized_keys; \
    fi \
    && mkdir -p /etc/ssh/sshd_config.d \
    && printf '%s\n' \
        'PermitRootLogin prohibit-password' \
        'PasswordAuthentication no' \
        'PermitEmptyPasswords no' \
        'PubkeyAuthentication yes' \
        > /etc/ssh/sshd_config.d/99-conch-debug.conf

COPY --from=envd-builder --chmod=0755 /out/envd /usr/bin/envd
COPY --from=code-interpreter-source /out/server/ /root/.server/
COPY --from=code-interpreter-source /out/jupyter/ /root/.jupyter/
COPY --from=code-interpreter-source /out/config/ /root/.config/
COPY --from=code-interpreter-source /out/ipython/ /root/.ipython/
COPY --chmod=0755 conch-entrypoint.sh /etc/conch/entrypoint

RUN chmod +x /root/.jupyter/jupyter-healthcheck.sh \
    && mkdir -p /etc/conch/features /var/log/conch-init /run/conch /home/user \
    && touch /etc/conch/features/envd /etc/conch/features/code-interpreter \
    && python3 -m venv /root/.server/.venv \
    && /root/.server/.venv/bin/python -m pip install --upgrade pip \
    && if [ "$INSTALL_FULL_TEMPLATE_REQUIREMENTS" = "1" ]; then \
        /root/.server/.venv/bin/pip install --no-cache-dir -r /root/.server/template-requirements.txt; \
    else \
        /root/.server/.venv/bin/pip install --no-cache-dir \
            jupyter-server==2.16.0 \
            ipykernel==6.31.0 \
            ipython==9.14.0; \
    fi \
    && /root/.server/.venv/bin/pip install --no-cache-dir -r /root/.server/requirements.txt \
    && /root/.server/.venv/bin/python -m ipykernel install --name python3 --display-name "Python 3 (ipykernel)" \
    && npm install -g ijavascript@5.2.1 \
    && ijsinstall --install=global \
    && chown -R root:root /root/.server /root/.jupyter /root/.config /root/.ipython \
    && chown -R user:user /home/user \
    && /usr/bin/envd -version \
    && /usr/bin/envd -commit

EXPOSE 22 4064 49983 49999 8888
