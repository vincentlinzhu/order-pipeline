# --- Stage 1: Build the Go application ---
FROM golang:1.24-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -o /app/main -ldflags="-s -w" .

# --- Stage 2: Create the final, lightweight image ---
FROM scratch

WORKDIR /app

COPY --from=builder /app/main .

ENTRYPOINT ["/app/main"]
