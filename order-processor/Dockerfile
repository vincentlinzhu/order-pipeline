# --- Build Stage ---
# Use the official Golang image to create a build artifact.
# This stage includes the Go toolchain and all necessary build tools.
FROM golang:1.24-alpine AS builder

# Set the working directory inside the container
WORKDIR /app

# Copy go.mod and go.sum files to download dependencies
COPY go.mod go.sum ./
RUN go mod download

# Copy the rest of the application source code
COPY . .

# Build the Go application.
# CGO_ENABLED=0 disables Cgo, creating a static binary.
# GOOS=linux specifies the target OS.
# -o /app/main creates an output file named 'main' in the /app directory.
RUN CGO_ENABLED=0 GOOS=linux go build -o /app/main .

# --- Final Stage ---
# Use a minimal base image for a small and secure final image.
# alpine:latest is a popular choice for its small size.
FROM alpine:latest

# Set the working directory
WORKDIR /app

# Copy the built binary from the 'builder' stage.
# This is the only file we need from the build stage.
COPY --from=builder /app/main .

# Set the entrypoint for the container.
# This command will be executed when the container starts.
ENTRYPOINT ["/app/main"]
