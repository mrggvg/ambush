FROM golang:1.26-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o gateway ./cmd/gateway

FROM scratch
COPY --from=builder /app/gateway /gateway
EXPOSE 8080 1080
ENTRYPOINT ["/gateway"]
