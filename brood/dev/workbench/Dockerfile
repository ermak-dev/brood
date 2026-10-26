ARG PYTHON_IMAGE
FROM docker:29.9.2-dind@sha256:9e79b879b177fbd72c34376a3dd3d91747da5db49e1f560f76aea59561471977 AS engine
FROM ${PYTHON_IMAGE}

ARG GO_VERSION
ARG GO_SHA256
ENV PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1 PIP_DISABLE_PIP_VERSION_CHECK=1

RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates curl git iproute2 iptables nftables kmod procps util-linux tini \
    && rm -rf /var/lib/apt/lists/*
COPY --from=engine /usr/local/bin/ /usr/local/bin/
COPY --from=engine /usr/local/libexec/docker/cli-plugins/ /usr/local/libexec/docker/cli-plugins/
RUN mkdir -p /tmp/workbench-go \
    && curl -fsSL -o /tmp/workbench-go/go.tar.gz "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz" \
    && printf '%s  %s\n' "${GO_SHA256}" /tmp/workbench-go/go.tar.gz | sha256sum -c - \
    && tar -xzf /tmp/workbench-go/go.tar.gz -C /usr/local \
    && rm -rf /tmp/workbench-go
ENV PATH=/usr/local/go/bin:${PATH}

COPY brood/pyproject.toml /opt/build/pyproject.toml
RUN python3 -I -c 'import tomllib; p=tomllib.load(open("/opt/build/pyproject.toml", "rb")); print("\n".join(p["project"]["optional-dependencies"]["server"] + p["project"]["optional-dependencies"]["dev"]))' > /tmp/workbench-requirements \
    && python3 -m pip download --quiet --dest /opt/wheels -r /tmp/workbench-requirements \
    && rm /tmp/workbench-requirements
RUN apt-get update && apt-get install -y --no-install-recommends e2fsprogs tmux ttyd \
    && rm -rf /var/lib/apt/lists/*
ARG CLAUDE_VERSION=2.1.310
ARG CLAUDE_SHA256=b73676430af12f97316a798b8371f81d640f90a58bdcb8f3ce56a835bb47df56
RUN curl -fsSL -o /usr/local/bin/claude \
        "https://downloads.claude.ai/claude-code-releases/${CLAUDE_VERSION}/linux-x64/claude" \
    && printf '%s  %s\n' "${CLAUDE_SHA256}" /usr/local/bin/claude | sha256sum -c - \
    && chmod 0755 /usr/local/bin/claude \
    && DISABLE_AUTOUPDATER=1 claude --version
COPY brood/ /opt/seeds/brood/
COPY substrate/ /opt/seeds/substrate/
COPY runtime/ /opt/seeds/brood/brood/dev/workbench/
COPY workbench.sh /opt/seeds/brood/.ci/workbench.sh

ENV BROOD_WORKBENCH=1 WORKBENCH_ROOT=/workspace WORKBENCH_SEEDS=/opt/seeds \
    HOME=/workspace/home TMPDIR=/workspace/tmp XDG_DATA_HOME=/workspace/data \
    DOCKER_HOST=unix:///var/run/docker.sock DOCKER_TLS_CERTDIR= \
    GOCACHE=/workspace/cache/go-build GOMODCACHE=/workspace/cache/go-mod GOPATH=/workspace/go \
    PIP_FIND_LINKS=/opt/wheels PIP_NO_INDEX=1
WORKDIR /opt/seeds/brood
ENTRYPOINT ["/usr/bin/tini", "--", "/opt/seeds/brood/bin/brood", "workbench", "start"]
CMD ["--", "bash", ".ci/workbench.sh"]
