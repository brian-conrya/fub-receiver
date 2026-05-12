# Use a pinned official Golang image for the build stage. Pinned by digest
# for reproducibility; Dependabot keeps it current.
FROM golang:1.26.3-bookworm@sha256:252599aeb51ad60b83e4d8821802068127c528c707cb7dd7afd93be057c6011c AS builder

# Set the working directory inside the container.
WORKDIR /src

# Copy go.mod and go.sum files and download dependencies.
COPY go.mod go.sum ./
RUN go mod download

# Copy the rest of the application source code.
COPY . .

# Build the Go application. -trimpath strips local paths from the binary.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" -o /out/fub-receiver

# Use a minimal distroless image for the final stage. The :nonroot variant
# runs as UID 65532, also pinned by digest.
FROM gcr.io/distroless/static-debian13:nonroot@sha256:e3f945647ffb95b5839c07038d64f9811adf17308b9121d8a2b87b6a22a80a39

# Copy the binary from the builder stage.
COPY --from=builder /out/fub-receiver /fub-receiver

# Expose the default port for Cloud Run (8080).
EXPOSE 8080

# Drop root for defense in depth.
USER nonroot:nonroot

# Run the Go binary.
ENTRYPOINT ["/fub-receiver"]
