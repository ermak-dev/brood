ARG PYTHON_IMAGE=python:3.12@sha256:0d0db5b17f11b1b7d1aafa74ceadaa64ba0944b267ff6fc22008a197acd6b9ad
FROM oven/bun:1.3.14-slim@sha256:d56a2534ffd262e92c12fd3249d3924d296d97086da773f821d7d0477435ea04 AS panel
WORKDIR /app
COPY web/package.json web/bun.lock ./
RUN bun install --frozen-lockfile
COPY web/ ./
RUN VERSION=v1.26.7+brood bun run build

FROM ${PYTHON_IMAGE} AS build
COPY brood/dev/pins.json /tmp/pins.json
RUN python3 -I -c 'import json; p=json.load(open("/tmp/pins.json")); print(p["GO_VERSION"]); print(p["GO_SHA256"])' > /tmp/go-pins \
    && version="$(sed -n '1p' /tmp/go-pins)" \
    && checksum="$(sed -n '2p' /tmp/go-pins)" \
    && mkdir /tmp/toolchain \
    && curl -fsSL "https://go.dev/dl/go${version}.linux-amd64.tar.gz" -o /tmp/go.tar.gz \
    && printf '%s  %s\n' "$checksum" /tmp/go.tar.gz | sha256sum -c - \
    && tar -xzf /tmp/go.tar.gz -C /tmp/toolchain
ENV PATH=/tmp/toolchain/go/bin:${PATH} CGO_ENABLED=0 GOTOOLCHAIN=local GOWORK=off
WORKDIR /src/go
COPY go/go.mod go/go.sum ./
RUN go mod download
COPY go/ ./
COPY --from=panel /app/dist/index.html ./brood/server/static/index.html
ARG VERSION=dev
RUN go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/brood ./cmd/brood \
    && go build -trimpath -ldflags="-s -w" -o /out/bro ./cmd/bro \
    && mkdir /out/data && chmod 0700 /out/data && chown 65532:65532 /out/data

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/brood /brood
COPY --from=build /out/bro /bro
COPY --from=build --chown=65532:65532 /out/data /data
USER 65532:65532
WORKDIR /data
ENV BROOD_DB_PATH=/data/brood.db BROOD_ADDR=:8443
EXPOSE 8443
ENTRYPOINT ["/brood"]
CMD ["serve"]
