# Lux server image: luxd, the lux command and the gitagent runner.
# See docs/self-hosting.md. Build from the repository root.
FROM docker.io/library/golang:1.27.1-trixie AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
ARG VERSION=0.0.0-dev
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X github.com/open-gitagent/lux/internal/version.Version=${VERSION}" \
      -o /out/ ./cmd/luxd ./cmd/lux

FROM docker.io/library/node:22.20.0-trixie-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends git ca-certificates \
 && rm -rf /var/lib/apt/lists/*
# luxd finds the runner at <bin>/../lib/lux/gitagent-runner.
WORKDIR /usr/local/lib/lux/gitagent-runner
COPY runtime/gitagent-runner/package.json runtime/gitagent-runner/package-lock.json ./
RUN npm ci --omit=dev --ignore-scripts && npm cache clean --force
COPY runtime/gitagent-runner/src ./src
COPY --from=build /out/luxd /out/lux /usr/local/bin/
RUN useradd --uid 10001 --no-log-init --shell /usr/sbin/nologin --home-dir /var/lib/lux --create-home lux
USER lux
WORKDIR /var/lib/lux
ENV LUX_MODE=server \
    LUX_LISTEN=0.0.0.0:7878 \
    LUX_DATA_DIR=/var/lib/lux/data
VOLUME /var/lib/lux
EXPOSE 7878
HEALTHCHECK --interval=15s --timeout=5s --start-period=10s --retries=5 \
  CMD ["node", "-e", "fetch('http://127.0.0.1:7878/readyz').then(r => process.exit(r.ok ? 0 : 1), () => process.exit(1))"]
ENTRYPOINT ["luxd"]
CMD ["serve"]
