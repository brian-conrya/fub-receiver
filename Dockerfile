# Use the latest official Golang image for the build stage.
FROM golang:latest AS builder

# Set the working directory inside the container.
WORKDIR /app

# Copy go.mod and go.sum files and download dependencies.
COPY go.mod go.sum ./
RUN go mod download

# Copy the rest of the application source code.
COPY . .

# Build the Go application.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o /app/fub-receiver

# Use a minimal distroless image for the final stage.
FROM gcr.io/distroless/static-debian12

# Set the working directory in the container.
WORKDIR /root/

# Copy the binary from the builder stage.
COPY --from=builder /app/fub-receiver .

# Expose the default port for Cloud Run (8080).
EXPOSE 8080

# Run the Go binary.
CMD ["./fub-receiver"]
