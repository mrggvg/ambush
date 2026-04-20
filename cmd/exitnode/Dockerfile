FROM golang:1.26-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o exitnode ./cmd/exitnode

FROM scratch
COPY --from=builder /app/exitnode /exitnode
ENTRYPOINT ["/exitnode"]
