# das-noc container image: one static binary on a distroless base, non-root.
#   docker build --build-arg VERSION=0.6.0 -t das-noc:0.6.0 .
# The NOC has no dependencies outside the Go standard library, so the build
# needs no module downloads.

FROM golang:1.24 AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/noc ./cmd/noc \
 && go test ./internal/proto ./internal/fleet

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/noc /noc
USER 65532:65532
# 8080: devices, dashboards, REST, NOC page (behind the ingress)
# 9090: /metrics, /healthz, pprof (cluster-internal only)
EXPOSE 8080 9090
ENV DAS_NOC_LISTEN=:8080 \
    DAS_NOC_INTERNAL_LISTEN=:9090 \
    DAS_NOC_DATA_DIR=/data \
    DAS_NOC_LOG=json
VOLUME ["/data"]
ENTRYPOINT ["/noc"]
