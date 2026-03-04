FROM golang:1.24-alpine AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/query-gateway ./cmd/query-gateway

FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /out/query-gateway /query-gateway
COPY configs /configs

EXPOSE 8080
ENTRYPOINT ["/query-gateway"]
