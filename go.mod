module windowsmetricsotel

go 1.22

require (
	github.com/eclipse/paho.mqtt.golang v1.5.0
	github.com/shirou/gopsutil/v4 v4.24.8
	go.opentelemetry.io/otel v1.28.0
	go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp v1.28.0
	go.opentelemetry.io/otel/sdk/metric v1.28.0
)
