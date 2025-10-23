# OpenTelemetry Collector with Filter Remap Processor

This custom OpenTelemetry Collector includes a filter remap processor that groups spans by trace ID, applies filtering rules, and maintains parent-child relationships even when parent spans are dropped.

## Features

- **Trace Buffering**: Groups spans by trace ID and buffers them in memory
- **OTTL-based Filtering**: Apply complex filtering rules using OpenTelemetry Transformation Language
- **Hierarchy Preservation**: Maintains span relationships when filtering
- **Production Ready**: Includes health checks, metrics, and profiling endpoints

## Quick Start

### Prerequisites

- Docker (for containerized deployment)
- Docker Compose (recommended for quick start)

### Option 1: Docker Compose (Recommended)

The easiest way to get started - everything is automatically built and configured:

```bash
# Build and start all services (collector, Jaeger, Prometheus, Grafana, trace generator)
docker-compose up --build

# Stop all services
docker-compose down
```

### Option 2: Docker Only

Build and run just the collector:

```bash
# Build the Docker image (automatically downloads ocb and builds the collector)
docker build -t otelcol-custom:latest .

# Run the collector
docker run --rm \
  -v $(pwd)/examples/config.yaml:/etc/otelcol/config.yaml:ro \
  -p 4317:4317 -p 4318:4318 -p 8888:8888 -p 13133:13133 -p 55679:55679 \
  otelcol-custom:latest
```

### Option 3: Build Arguments

Customize the build with different OCB versions, target architectures, or manifests:

```bash
# Use a specific OCB version (default: 0.136.0)
docker build --build-arg OCB_VERSION=0.136.0 -t otelcol-custom:latest .

# Build for a specific architecture (default: amd64)
docker build --build-arg TARGETARCH=arm64 -t otelcol-custom:latest .

# Use a custom manifest
docker build --build-arg MANIFEST_FILE=manifest.yaml -t otelcol-custom:latest .

# Combine multiple arguments
docker build \
  --build-arg OCB_VERSION=0.136.0 \
  --build-arg TARGETARCH=arm64 \
  --build-arg MANIFEST_FILE=manifest.yaml \
  -t otelcol-custom:latest .
```

### Accessing Services

After running `docker-compose up --build`, access:

- **Jaeger UI**: http://localhost:16686 (view traces)
- **Grafana**: http://localhost:3000 (dashboards - admin/admin)
- **Prometheus**: http://localhost:9090 (metrics)
- **Collector Health**: http://localhost:13133 (health check)
- **Collector zPages**: http://localhost:55679/debug/tracez (debugging)
- **Collector Metrics**: http://localhost:8888/metrics (Prometheus metrics)

## Customizing the Collector

### Adding Additional Components

The collector is built with a minimal set of components. You can add more receivers, processors, exporters, extensions, and connectors by editing `manifest.yaml`:

```yaml
# Example: Adding more exporters
exporters:
  - gomod: github.com/open-telemetry/opentelemetry-collector-contrib/exporter/jaegerexporter v0.136.0
  - gomod: github.com/open-telemetry/opentelemetry-collector-contrib/exporter/prometheusexporter v0.136.0
  - gomod: github.com/open-telemetry/opentelemetry-collector-contrib/exporter/kafkaexporter v0.136.0

# Example: Adding more processors
processors:
  - gomod: github.com/open-telemetry/opentelemetry-collector-contrib/processor/attributesprocessor v0.136.0
  - gomod: github.com/open-telemetry/opentelemetry-collector-contrib/processor/resourceprocessor v136.0
  - gomod: github.com/open-telemetry/opentelemetry-collector-contrib/processor/tailsamplingprocessor v0.136.0

# Example: Adding more receivers
receivers:
  - gomod: github.com/open-telemetry/opentelemetry-collector-contrib/receiver/prometheusreceiver v0.136.0
  - gomod: github.com/open-telemetry/opentelemetry-collector-contrib/receiver/jaegerreceiver v0.136.0
  - gomod: github.com/open-telemetry/opentelemetry-collector-contrib/receiver/kafkareceiver v0.136.0

# Example: Adding more extensions
extensions:
  - gomod: github.com/open-telemetry/opentelemetry-collector-contrib/extension/basicauthextension v0.136.0
  - gomod: github.com/open-telemetry/opentelemetry-collector-contrib/extension/oauth2clientauthextension v0.136.0
```

After modifying `manifest.yaml`, rebuild the Docker image:

```bash
docker build -t otelcol-custom:latest .
```

**Available Components**: See the [OpenTelemetry Collector Contrib](https://github.com/open-telemetry/opentelemetry-collector-contrib) repository for all available components.

## Configuration

### Processor Configuration

```yaml
processors:
  filter_remap:
    # Maximum time to retain a trace before forwarding
    max_trace_retention: 30s
    
    # Time to wait after last span before forwarding
    last_span_timeout: 5s
    
    # Number of traces to keep in memory
    num_traces: 10000
    
    # Drop root spans if they match the filter
    drop_root_spans: true
    
    # OTTL filtering conditions
    traces:
      span:
        - 'name == "/health"'
        - 'attributes["http.route"] == "/metrics"'
      spanevent:
        - 'attributes["log.level"] == "DEBUG"'
```

### OTTL Filter Examples

```yaml
# Drop health check endpoints
- 'name == "/health" or name == "/healthz"'

# Drop spans from specific services
- 'resource.attributes["service.name"] == "prometheus-scraper"'

# Drop successful requests with specific status
- 'status.code == STATUS_CODE_OK and attributes["http.status_code"] == 200'

# Complex conditions
- 'kind == SPAN_KIND_INTERNAL and name == "cache.get"'

# For sending traces to Arize with a mix of APM spans and openinference spans, keep only the openinference spans
- 'attributes["openinference.span.kind"] == nil'
```

## Sending Traces to Arize

This collector can be configured to send traces to [Arize AI](https://arize.com/) for AI/ML observability. Arize provides powerful tools for monitoring and debugging LLM applications, including the OpenInference specification for tracing.

### Setting Up Environment Variables

1. **Copy the example environment file:**
   ```bash
   cp .env.example .env
   ```

2. **Edit `.env` and add your Arize credentials:**
   ```bash
   # Arize API Credentials
   ARIZE_SPACE_ID=your_space_id_here
   ARIZE_API_KEY=your_api_key_here
   ```

   > **Important**: The `.env` file is gitignored to prevent committing sensitive credentials. Never commit your actual API keys to version control.

3. **Get your Arize credentials:**
   - Log in to [Arize AI](https://app.arize.com/)
   - Navigate to **Settings** → **API Keys**
   - Copy your **Space ID** and **API Key**

### Configuration

The collector uses the `headers_setter` extension to inject authentication headers. Your `arize_config.yaml` should include:

```yaml
extensions:
  headers_setter:
    headers:
      - key: space_id
        value: ${ARIZE_SPACE_ID}
        action: upsert
      - key: api_key
        value: ${ARIZE_API_KEY}
        action: upsert

exporters:
  otlp:
    endpoint: "otlp.arize.com:443"
    auth:
      authenticator: headers_setter

service:
  extensions: [headers_setter]
  pipelines:
    traces:
      receivers: [otlp]
      processors: [transform, filter_remap]
      exporters: [otlp]
```

### Filtering for Arize OpenInference Spans

When you have a mix of standard APM spans and OpenInference spans (from LLM applications), you can filter to send only OpenInference spans to Arize:

```yaml
processors:
  filter_remap:
    traces:
      span:
        # Drop spans that don't have openinference.span.kind attribute
        - 'attributes["openinference.span.kind"] == nil'
  
  transform:
    error_mode: ignore
    trace_statements:
      # Set project name for Arize
      - set(resource.attributes["openinference.project.name"], "My LLM Project")
```

### Running with Environment Variables

**Option 1: Source the .env file (Linux/macOS):**
```bash
# Load environment variables
source .env

# Run the collector
./_build/otelcol-arize-custom --config=arize_config.yaml
```

**Option 2: Export variables inline:**
```bash
export $(cat .env | xargs) && ./_build/otelcol-arize-custom --config=arize_config.yaml
```

**Option 3: Docker Compose:**
```yaml
services:
  otel-collector:
    image: otelcol-custom:latest
    env_file:
      - .env
    volumes:
      - ./arize_config.yaml:/etc/otelcol/config.yaml:ro
    ports:
      - "4317:4317"
      - "4318:4318"
```

Then run:
```bash
docker-compose up
```

### Verifying the Connection

1. **Check collector logs** for successful startup:
   ```
   INFO    service/service.go:161  Starting otelcol-arize-custom...
   INFO    extensions/extensions.go:31     Starting extensions...
   ```

2. **Send test traces** to the collector (see `arize_test/create_synthetic_trace.py`)

3. **View traces in Arize:**
   - Navigate to https://app.arize.com/
   - Select your project
   - Go to the **Tracing** tab
   - Filter by your project name

### Example Pipeline Configurations

**Unfiltered Pipeline (send all traces):**
```yaml
pipelines:
  traces:
    receivers: [otlp]
    processors: [transform]
    exporters: [otlp]
```

**Filtered Pipeline (OpenInference only):**
```yaml
pipelines:
  traces/filtered:
    receivers: [otlp]
    processors: [filter_remap, transform]
    exporters: [otlp]
```

**Dual Pipeline (filter and monitor both):**
```yaml
pipelines:
  traces/unfiltered:
    receivers: [otlp]
    processors: [transform]
    exporters: [otlp]
  
  traces/filtered:
    receivers: [otlp]
    processors: [filter_remap, transform]
    exporters: [otlp]
```

### Troubleshooting Arize Connection

**Authentication errors (401):**
- Verify your `ARIZE_SPACE_ID` and `ARIZE_API_KEY` are correct
- Ensure environment variables are loaded (`echo $ARIZE_SPACE_ID`)
- Check that the `headers_setter` extension is enabled in the service section

**Connection errors (connection refused):**
- Verify you can reach `otlp.arize.com:443`
- Check your firewall/network settings
- Ensure TLS/SSL certificates are valid

**No traces appearing in Arize:**
- Verify traces are being received by the collector (check metrics at `localhost:8888/metrics`)
- Confirm spans have the required OpenInference attributes
- Check processor filters aren't dropping all spans
- Review collector logs for export errors

## Monitoring

### Metrics

The processor exposes the following metrics:

- `otelcol_processor_filter_remap_count_spans_sampled`: Spans sampled/dropped
- `otelcol_processor_filter_remap_traces_on_memory`: Current traces in memory
- `otelcol_processor_filter_remap_new_trace_id_received`: New traces received
- `otelcol_processor_filter_remap_span_decision_latency`: Decision latency histogram
- `otelcol_processor_filter_remap_trace_remap_latency`: Remapping latency histogram

### Health Checks

- Health endpoint: http://localhost:13133/health
- zPages: http://localhost:55679
- pprof: http://localhost:1777/debug/pprof/

## Performance Tuning

### Memory Management

```yaml
# Set GOMEMLIMIT to 90% of container memory limit
env:
  - name: GOMEMLIMIT
    value: "1900MiB"  # For 2GiB container
  - name: GOGC
    value: "80"       # More aggressive GC
```

### Scaling Guidelines

- **Small workload** (< 1000 traces/sec): 1-2 replicas, 1Gi memory
- **Medium workload** (1000-5000 traces/sec): 3-5 replicas, 2Gi memory  
- **Large workload** (> 5000 traces/sec): 5-10 replicas, 4Gi memory

## Development

### Project Structure

```
├── filterremapprocessor/       # Processor implementation
│   ├── config.go               # Configuration
│   ├── processor.go            # Main processor logic
│   ├── trace.go                # Trace data structures
│   └── hierarchy_node.go       # Span hierarchy management
├── Dockerfile                  # Docker build definition
├── docker-compose.yaml         # Complete stack deployment
├── manifest.yaml               # OCB manifest for building
├── grafana/                    # grafana dashboard configurations for collector metrics
```

### Running Tests

```bash
cd filterremapprocessor
go test ./...
```

### Contributing

1. Fork the repository
2. Create your feature branch
3. Run tests and benchmarks
4. Submit a pull request

## Troubleshooting

### Common Issues

1. **High Memory Usage**: Reduce `num_traces` or decrease `max_trace_retention`
2. **Traces Not Forwarded**: Check `last_span_timeout` and ensure it's not too high
3. **OTTL Errors**: Validate expressions using the config validator

### Debug Mode

Enable detailed logging:
```yaml
service:
  telemetry:
    logs:
      level: debug
```

## License

GNU Affero General Public License v3.0 (AGPL-3.0)

This project is licensed under the AGPL-3.0, one of the strongest copyleft licenses. This means:
- Any modifications or derivative works must also be licensed under AGPL-3.0
- If you run this software as a service over a network, you must make the source code available to users
- Patent licenses are granted to all users

See the [LICENSE](LICENSE) file for full details.
