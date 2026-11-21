ARG PYTHON_IMAGE=python:3.12-slim@sha256:aa943ce411423c7582288f18707ff66eddcbaeb510502d747a72aeb34f30d159
FROM oven/bun:1.3.14-slim@sha256:d56a2534ffd262e92c12fd3249d3924d296d97086da773f821d7d0477435ea04 AS panel
WORKDIR /app
COPY web/package.json web/bun.lock ./
RUN bun install --frozen-lockfile
COPY web/ ./
RUN VERSION=v1.26.7+brood bun run build

FROM ${PYTHON_IMAGE} AS build
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/*
COPY brood/dev/pins.json /tmp/pins.json
COPY .ci/go-download.sh /tmp/go-download.sh
RUN python3 -I -c 'import json; p=json.load(open("/tmp/pins.json")); print(p["GO_VERSION"]); print(p["GO_SHA256"])' > /tmp/go-pins \
    && version="$(sed -n '1p' /tmp/go-pins)" \
    && checksum="$(sed -n '2p' /tmp/go-pins)" \
    && mkdir /tmp/toolchain \
    && bash /tmp/go-download.sh "$version" "$checksum" /tmp/go.tar.gz \
    && printf '%s  %s\n' "$checksum" /tmp/go.tar.gz | sha256sum -c - \
    && tar -xzf /tmp/go.tar.gz -C /tmp/toolchain
ENV PATH=/tmp/toolchain/go/bin:${PATH} CGO_ENABLED=0 GOTOOLCHAIN=local GOWORK=off
WORKDIR /src/go
COPY go/go.mod go/go.sum ./
RUN go mod download
COPY go/ ./
COPY --from=panel /app/dist/index.html ./brood/server/static/index.html
ARG VERSION=dev
RUN mkdir /out && go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/ ./cmd/brood ./cmd/bro ./cmd/brood-guest

# Результат сборки экспортируется в файлы; образ на машину не выкладывается.
FROM scratch AS binaries
COPY --from=build /out/ /
