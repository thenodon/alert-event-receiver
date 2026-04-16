package telemetry

import (
	"context"
	"fmt"
	"os"
	"strings"

	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/log/global"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.27.0"
)

// InitOTel initialises the OTel log SDK and returns a shutdown function.
//
// The exporter protocol is selected by OTEL_EXPORTER_OTLP_PROTOCOL:
//   - "grpc" (default)          → otlploggrpc
//   - "http/protobuf" or "http" → otlploghttp
//
// All other OTel configuration (endpoint, headers, TLS, timeout) is read
// from the standard OTel environment variables by the exporters automatically:
//
//	OTEL_EXPORTER_OTLP_ENDPOINT
//	OTEL_EXPORTER_OTLP_HEADERS
//	OTEL_EXPORTER_OTLP_TIMEOUT
//	OTEL_RESOURCE_ATTRIBUTES
func InitOTel(ctx context.Context, serviceName string) (shutdown func(context.Context) error, err error) {
	protocol := os.Getenv("OTEL_EXPORTER_OTLP_PROTOCOL")
	if protocol == "" {
		protocol = "grpc"
	}

	headers, err := parseOTLPHeadersFromEnv()
	if err != nil {
		return nil, err
	}

	var exporter sdklog.Exporter
	switch protocol {
	case "http/protobuf", "http":
		if len(headers) > 0 {
			exporter, err = otlploghttp.New(ctx, otlploghttp.WithHeaders(headers))
		} else {
			exporter, err = otlploghttp.New(ctx)
		}
	case "grpc":
		if len(headers) > 0 {
			exporter, err = otlploggrpc.New(ctx, otlploggrpc.WithHeaders(headers))
		} else {
			exporter, err = otlploggrpc.New(ctx)
		}
	default:
		return nil, fmt.Errorf("unsupported OTEL_EXPORTER_OTLP_PROTOCOL: %q (want grpc or http/protobuf)", protocol)
	}
	if err != nil {
		return nil, fmt.Errorf("create OTLP log exporter: %w", err)
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(semconv.ServiceName(serviceName)),
		resource.WithFromEnv(),  // OTEL_RESOURCE_ATTRIBUTES
		resource.WithProcess(),
		resource.WithOS(),
	)
	if err != nil {
		return nil, fmt.Errorf("create OTel resource: %w", err)
	}

	provider := sdklog.NewLoggerProvider(
		sdklog.WithResource(res),
		sdklog.WithProcessor(sdklog.NewBatchProcessor(exporter)),
	)
	global.SetLoggerProvider(provider)

	return provider.Shutdown, nil
}

func parseOTLPHeadersFromEnv() (map[string]string, error) {
	raw := os.Getenv("OTEL_EXPORTER_OTLP_LOGS_HEADERS")
	if raw == "" {
		raw = os.Getenv("OTEL_EXPORTER_OTLP_HEADERS")
	}
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	parsed, err := parseHeaderMap(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid OTLP headers in environment: %w", err)
	}
	return parsed, nil
}

// parseHeaderMap parses a header list in k=v,k2=v2 form.
// It is intentionally lenient: tokens without '=' are treated as a comma
// continuation of the previous header value, which supports values like
// VL-Stream-Fields=a,b,c in environment variables.
func parseHeaderMap(raw string) (map[string]string, error) {
	result := make(map[string]string)
	parts := strings.Split(raw, ",")
	lastKey := ""

	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if i := strings.Index(part, "="); i > 0 {
			k := strings.TrimSpace(part[:i])
			v := strings.TrimSpace(part[i+1:])
			if k == "" {
				return nil, fmt.Errorf("empty header key in segment %q", part)
			}
			result[k] = v
			lastKey = k
			continue
		}

		if lastKey == "" {
			return nil, fmt.Errorf("invalid header segment %q", part)
		}
		result[lastKey] = result[lastKey] + "," + part
	}

	return result, nil
}

// NewOTelEmitter returns an Emitter that writes lifecycle events as OTel log records.
func NewOTelEmitter(instrumentationName, instrumentationVersion string) *OTelEmitter {
	return &OTelEmitter{
		logger: global.GetLoggerProvider().Logger(
			instrumentationName,
			otellog.WithInstrumentationVersion(instrumentationVersion),
		),
	}
}

// OTelEmitter emits LifecycleEvents as OTel LogRecords.
type OTelEmitter struct {
	logger otellog.Logger
}


