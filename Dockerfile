# Use a pinned official Golang image for the build stage. Pinned by digest
# for reproducibility; Dependabot keeps it current.
FROM golang:1.27.2-bookworm@sha256:5cf287a799e6b94384bad13d16b14904c531f51ba65792237e122ce42b392f61 AS builder

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
FROM gcr.io/distroless/static-debian13:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3

# Copy the binary from the builder stage.
COPY --from=builder /out/fub-receiver /fub-receiver

# Expose the default port for Cloud Run (8080).
EXPOSE 8080

# Drop root for defense in depth.
USER nonroot:nonroot

# Run the Go binary.
ENTRYPOINT ["/fub-receiver"]
