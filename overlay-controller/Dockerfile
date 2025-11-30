FROM golang:1.21-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o overlay-controller main.go

FROM gcr.io/distroless/base-debian12
WORKDIR /app
COPY --from=builder /app/overlay-controller /app/overlay-controller
ENV OVERLAY_STATIC_DIR=/app/overlay
EXPOSE 8080
CMD ["/app/overlay-controller"]
