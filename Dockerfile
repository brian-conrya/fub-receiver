# Use a pinned official Golang image for the build stage. Pinned by digest
# for reproducibility; Dependabot keeps it current.
FROM golang:1.26.5-bookworm@sha256:e60d708a92ad26a6d61901334510d3debd23ddcba125663ecd6008d42e8ec669 AS builder

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
FROM gcr.io/distroless/static-debian13:nonroot@sha256:963fa6c544fe5ce420f1f54fb88b6fb01479f054c8056d0f74cc2c6000df5240

# Copy the binary from the builder stage.
COPY --from=builder /out/fub-receiver /fub-receiver

# Expose the default port for Cloud Run (8080).
EXPOSE 8080

# Drop root for defense in depth.
USER nonroot:nonroot

# Run the Go binary.
ENTRYPOINT ["/fub-receiver"]
