# --- Stage 1: Build the Go application ---
# We use a specific version of the official Go image as our build environment.
FROM golang:1.24-alpine AS builder

# Set the working directory inside the container.
WORKDIR /app

# Copy the go.mod and go.sum files to download dependencies.
COPY go.mod go.sum ./
# Download all the dependencies defined in go.mod.
RUN go mod download

# Copy the rest of the application source code.
COPY . .

# Build the Go application.
# -o /app/main: specifies the output file name and location.
# -ldflags="-s -w": are build flags to create a smaller binary.
# CGO_ENABLED=0: disables Cgo, which is needed for a static binary.
RUN CGO_ENABLED=0 GOOS=linux go build -o /app/main -ldflags="-s -w" .

# --- Stage 2: Create the final, lightweight image ---
# We use a minimal 'scratch' image which contains nothing but our application.
# This makes the final image very small and secure.
FROM scratch

# Set the working directory.
WORKDIR /app

# Copy the built binary from the 'builder' stage.
COPY --from=builder /app/main .

# The command to run when the container starts.
ENTRYPOINT ["/app/main"]
