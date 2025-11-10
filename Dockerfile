# ==============================================================================
# Multi-stage build for custom OpenTelemetry Collector with trace hierarchy processor
# ==============================================================================
#
# This Dockerfile builds a custom OpenTelemetry Collector that includes:
# - The trace hierarchy processor (filter_remap processor)
# - Standard OTEL receivers, processors, and exporters
#
# Usage:
#   docker build -t otelcol-custom:latest .
#   docker run -v $(pwd)/examples/config.yaml:/etc/otelcol/config.yaml otelcol-custom:latest
#
# Build arguments:
#   OCB_VERSION - OpenTelemetry Collector Builder version (default: 0.136.0)
#   MANIFEST_FILE - Manifest file to use for building (default: manifest.yaml)
#   TARGETARCH - Target architecture for the build (default: amd64, options: amd64, arm64)
#
# ==============================================================================

FROM golang:1.24-alpine AS builder

# Build arguments
ARG OCB_VERSION=0.136.0
ARG MANIFEST_FILE=manifest.yaml
ARG TARGETARCH=amd64

# Install build dependencies
RUN apk add --no-cache git make curl ca-certificates

# Set working directory
WORKDIR /build

# Copy the entire project
COPY . .

RUN echo $(uname -m)

RUN curl --proto '=https' --tlsv1.2 -fL -o ocb \
  https://github.com/open-telemetry/opentelemetry-collector-releases/releases/download/cmd%2Fbuilder%2Fv${OCB_VERSION}/ocb_${OCB_VERSION}_linux_${TARGETARCH} && \
  chmod +x ocb && \
  ./ocb version

# Build the custom collector using the manifest file
RUN echo "Building collector with manifest: ${MANIFEST_FILE}" && \
    ./ocb --config ${MANIFEST_FILE} && \
    ls -lh _build/

# ==============================================================================
# Runtime stage - minimal image with only the collector binary
# ==============================================================================
FROM alpine:3.19

LABEL maintainer="Luke Moehlenbrock <lucas@arize.com>"
LABEL description="Custom OpenTelemetry Collector with Trace Hierarchy Processor"

# Install runtime dependencies
# - ca-certificates: Required for TLS/HTTPS connections
# - wget: Required for health checks
RUN apk --no-cache add ca-certificates wget

# Create non-root user for security
RUN addgroup -g 10001 -S otel && \
    adduser -u 10001 -S otel -G otel

# Copy the collector binary from builder stage
COPY --from=builder /build/_build/otelcol-arize-custom /otelcol-custom

# Set ownership to non-root user
RUN chown otel:otel /otelcol-custom && \
    chmod +x /otelcol-custom

# Create config directory with proper permissions
RUN mkdir -p /etc/otelcol && chown -R otel:otel /etc/otelcol

# Switch to non-root user for security
USER otel

# Expose OpenTelemetry collector ports
# See: https://opentelemetry.io/docs/collector/deployment/

# OTLP gRPC receiver
EXPOSE 4317
# OTLP HTTP receiver
EXPOSE 4318
# Prometheus metrics endpoint
EXPOSE 8888
# Health check extension
EXPOSE 13133

# Health check to ensure the collector is running
# Requires health_check extension to be enabled in config.yaml
HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 \
  CMD wget --no-verbose --tries=1 --spider http://localhost:13133/ || exit 1

# Set the entrypoint to the collector binary
ENTRYPOINT ["/otelcol-custom"]

# Default command arguments - config file can be overridden via volume mount
# Mount your config at: /etc/otelcol/config.yaml
CMD ["--config", "/etc/otelcol/config.yaml"]
