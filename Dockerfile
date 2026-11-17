# syntax=docker/dockerfile:1
FROM golang:1.26.5-bookworm AS builder

ARG GOPROXY=https://goproxy.cn,direct
ARG TARGETOS
ARG TARGETARCH
ENV GOPROXY=${GOPROXY}

WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-arm64} \
    go build -trimpath -ldflags="-s -w" -o /out/manager ./cmd/manager

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /out/manager /manager
USER 65532:65532
ENTRYPOINT ["/manager"]
