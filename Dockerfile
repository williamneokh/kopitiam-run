# Stage 1: Build the Go binary in a temporary container
FROM golang:1.21-alpine AS builder

WORKDIR /app

# Copy go.mod and go.sum to download dependencies first, for caching
COPY go.mod go.sum ./
RUN go mod download

# Copy the rest of the application source code
COPY . .

# Build the Go app into a single, static binary.
# CGO_ENABLED=0 is important for creating a static binary without C dependencies.
# -ldflags="-s -w" strips debug symbols to make the binary smaller.
RUN CGO_ENABLED=0 GOOS=linux go build -a -ldflags="-s -w" -o kopitiam-run .

# Stage 2: Create the final, small production image
FROM alpine:latest

# Create a /data directory. This is where we will mount our persistent volume
# for the SQLite database file.
RUN mkdir /data

WORKDIR /root/

# Copy the pre-built binary from the 'builder' stage
COPY --from=builder /app/kopitiam-run .

# Copy the HTML templates
COPY --from=builder /app/templates ./templates

# Tell the world that the container listens on port 8080
EXPOSE 8080

# The command to run when the container starts
CMD ["./kopitiam-run"]
