# Override image arguments deliberately when updating the supported toolchain or OS.
ARG GO_IMAGE=docker.io/library/golang:1.27.2-bookworm@sha256:5cf287a799e6b94384bad13d16b14904c531f51ba65792237e122ce42b392f61
ARG RUNTIME_IMAGE=docker.io/library/debian:bookworm-slim@sha256:7c7b2c966bc9ee8cedfeef67e0e279108992c77681fa595db4a9d65c06ccc587

FROM ${GO_IMAGE} AS build
ARG TARGETARCH
RUN test "$TARGETARCH" = amd64
WORKDIR /src
ENV CGO_ENABLED=0 GOTOOLCHAIN=local GOPROXY=https://proxy.golang.org GOSUMDB=sum.golang.org
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
RUN go build -mod=readonly -trimpath -buildvcs=false -o /out/fcd ./cmd/fcd

FROM ${RUNTIME_IMAGE} AS runtime
RUN apt-get update \
    && apt-get install -y --no-install-recommends git ca-certificates \
    && test -x /usr/lib/git-core/git-remote-https \
    && test -s /etc/ssl/certs/ca-certificates.crt \
    && test -s /usr/share/doc/git/copyright \
    && test -s /usr/share/doc/ca-certificates/copyright \
    && rm -rf /var/lib/apt/lists/* \
    && install -d -o 65532 -g 65532 -m 0700 /data
COPY --from=build --chmod=0555 /out/fcd /fcd
COPY LICENSE /usr/share/doc/firecontroller/copyright
USER 65532:65532
ENTRYPOINT ["/fcd"]
CMD ["--data-dir", "/data", "--listen", "127.0.0.1:8081"]
