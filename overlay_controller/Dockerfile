FROM golang:1.21-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG TARGETOS
ARG TARGETARCH
# Build for the requested platform; fall back to the host platform when not explicitly set.
RUN set -euo pipefail \
    && build_os=${TARGETOS:-$(go env GOOS)} \
    && build_arch=${TARGETARCH:-$(go env GOARCH)} \
    && CGO_ENABLED=0 GOOS="$build_os" GOARCH="$build_arch" go build -o overlay_controller main.go

FROM gcr.io/distroless/base-debian12

WORKDIR /app

COPY --from=builder /app/overlay_controller /app/overlay_controller

ENV OVERLAY_STATIC_DIR=/app/overlay

EXPOSE 8080

CMD ["/app/overlay_controller"]
