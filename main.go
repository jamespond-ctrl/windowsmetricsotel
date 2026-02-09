package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/mem"
	gnet "github.com/shirou/gopsutil/v4/net"
	"github.com/shirou/gopsutil/v4/process"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	metricapi "go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

type Config struct {
	OTLP          OTLPConfig          `json:"otlp"`
	Runtime       RuntimeConfig       `json:"runtime"`
	Monitor       MonitorConfig       `json:"monitor"`
	HomeAssistant HomeAssistantConfig `json:"home_assistant"`
}

type OTLPConfig struct {
	Endpoint       string `json:"endpoint"`
	MetricsPath    string `json:"metrics_path"`
	Insecure       bool   `json:"insecure"`
	ExportInterval string `json:"export_interval"`
}

type RuntimeConfig struct {
	AppScanInterval string `json:"app_scan_interval"`
}

type MonitorConfig struct {
	CPU            bool `json:"cpu"`
	Memory         bool `json:"memory"`
	Disk           bool `json:"disk"`
	ProcessCount   bool `json:"process_count"`
	NetworkIO      bool `json:"network_io"`
	PowerLifecycle bool `json:"power_lifecycle"`
	AppLifecycle   bool `json:"app_lifecycle"`
	Temperatures   bool `json:"temperatures"`
}

type HomeAssistantConfig struct {
	Enabled  bool   `json:"enabled"`
	MQTTURL  string `json:"mqtt_url"`
	Username string `json:"username"`
	Password string `json:"password"`
	Topic    string `json:"topic"`
	DeviceID string `json:"device_id"`
	Name     string `json:"name"`
	MAC      string `json:"mac"`
}

func defaultConfig() Config {
	return Config{
		OTLP:          OTLPConfig{Endpoint: "192.168.1.10:4318", MetricsPath: "/v1/metrics", Insecure: true, ExportInterval: "15s"},
		Runtime:       RuntimeConfig{AppScanInterval: "5s"},
		Monitor:       MonitorConfig{CPU: true, Memory: true, Disk: true, ProcessCount: true, NetworkIO: true, PowerLifecycle: true, AppLifecycle: true, Temperatures: true},
		HomeAssistant: HomeAssistantConfig{Enabled: false, MQTTURL: "tcp://127.0.0.1:1883", Topic: "windowsmetricsotel", DeviceID: "windowsmetricsotel_pc", Name: "Windows PC", MAC: ""},
	}
}

type runtimeState struct {
	startupEvents  atomic.Int64
	shutdownEvents atomic.Int64
	processStarts  atomic.Int64
	processStops   atomic.Int64
	mu             sync.RWMutex
	runningApps    map[int32]string
}

type processInfo struct {
	PID  int32
	Name string
}
type processSource interface {
	List(context.Context) ([]processInfo, error)
}
type lifecycleSink interface {
	OnStart(processInfo)
	OnStop(processInfo)
}
type nopLifecycleSink struct{}

func (nopLifecycleSink) OnStart(processInfo) {}
func (nopLifecycleSink) OnStop(processInfo)  {}

type gopsutilProcessSource struct{}

func (gopsutilProcessSource) List(ctx context.Context) ([]processInfo, error) {
	procs, err := process.ProcessesWithContext(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]processInfo, 0, len(procs))
	for _, p := range procs {
		name, err := p.NameWithContext(ctx)
		if err != nil || name == "" {
			name = "unknown"
		}
		result = append(result, processInfo{PID: p.Pid, Name: name})
	}
	return result, nil
}

func main() {
	ctx := context.Background()
	cfg, err := loadConfig(configPath())
	if err != nil {
		log.Fatalf("kunde inte läsa config: %v", err)
	}

	shutdownOTel, err := setupOTel(ctx, cfg)
	if err != nil {
		log.Fatalf("kunde inte starta OpenTelemetry: %v", err)
	}
	defer shutdownOTel(context.Background())

	state := &runtimeState{runningApps: map[int32]string{}}
	state.startupEvents.Store(1)
	meter := otel.Meter("windowsmetricsotel")
	if err := registerMetrics(meter, state, cfg); err != nil {
		log.Fatalf("kunde inte registrera metrics: %v", err)
	}

	if cfg.Monitor.AppLifecycle {
		go trackProcessLifecycle(ctx, state, parseDuration(cfg.Runtime.AppScanInterval, 5*time.Second), gopsutilProcessSource{}, nopLifecycleSink{})
	}

	var haClient mqtt.Client
	if cfg.HomeAssistant.Enabled {
		haClient, err = setupHomeAssistant(ctx, cfg)
		if err != nil {
			log.Printf("home assistant init misslyckades: %v", err)
		} else {
			defer haClient.Disconnect(1000)
		}
	}

	log.Printf("windowsmetricsotel kör. export endpoint: %s", cfg.OTLP.Endpoint)
	waitForSignal(state)
}

func setupOTel(ctx context.Context, cfg Config) (func(context.Context) error, error) {
	opts := []otlpmetrichttp.Option{otlpmetrichttp.WithEndpoint(cfg.OTLP.Endpoint), otlpmetrichttp.WithURLPath(cfg.OTLP.MetricsPath), otlpmetrichttp.WithTimeout(10 * time.Second)}
	if cfg.OTLP.Insecure {
		opts = append(opts, otlpmetrichttp.WithInsecure())
	}
	exp, err := otlpmetrichttp.New(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("skapa exporter: %w", err)
	}
	hostname, _ := os.Hostname()
	res, err := resource.New(ctx, resource.WithAttributes(semconv.ServiceName("windowsmetricsotel"), semconv.HostName(hostname)))
	if err != nil {
		return nil, fmt.Errorf("skapa resource: %w", err)
	}
	provider := metric.NewMeterProvider(metric.WithResource(res), metric.WithReader(metric.NewPeriodicReader(exp, metric.WithInterval(parseDuration(cfg.OTLP.ExportInterval, 15*time.Second)))))
	otel.SetMeterProvider(provider)
	return provider.Shutdown, nil
}

func registerMetrics(meter metricapi.Meter, state *runtimeState, cfg Config) error {
	if cfg.Monitor.CPU {
		_, err := meter.Float64ObservableGauge("system.cpu.usage.percent", metricapi.WithFloat64Callback(func(ctx context.Context, o metricapi.Float64Observer) error {
			values, err := cpu.PercentWithContext(ctx, 0, false)
			if err == nil && len(values) > 0 {
				o.Observe(values[0])
			}
			return nil
		}))
		if err != nil {
			return err
		}
	}
	if cfg.Monitor.Memory {
		_, err := meter.Int64ObservableGauge("system.memory.used", metricapi.WithUnit("By"), metricapi.WithInt64Callback(func(ctx context.Context, o metricapi.Int64Observer) error {
			vm, err := mem.VirtualMemoryWithContext(ctx)
			if err == nil {
				o.Observe(int64(vm.Used))
			}
			return nil
		}))
		if err != nil {
			return err
		}
		_, err = meter.Int64ObservableGauge("system.memory.available", metricapi.WithUnit("By"), metricapi.WithInt64Callback(func(ctx context.Context, o metricapi.Int64Observer) error {
			vm, err := mem.VirtualMemoryWithContext(ctx)
			if err == nil {
				o.Observe(int64(vm.Available))
			}
			return nil
		}))
		if err != nil {
			return err
		}
	}
	if cfg.Monitor.Disk {
		_, err := meter.Float64ObservableGauge("system.disk.usage.percent", metricapi.WithFloat64Callback(func(ctx context.Context, o metricapi.Float64Observer) error {
			du, err := disk.UsageWithContext(ctx, `C:\`)
			if err == nil {
				o.Observe(du.UsedPercent, metricapi.WithAttributes(attribute.String("disk.mount", `C:\`)))
			}
			return nil
		}))
		if err != nil {
			return err
		}
	}
	if cfg.Monitor.ProcessCount {
		_, err := meter.Int64ObservableGauge("system.process.count", metricapi.WithInt64Callback(func(ctx context.Context, o metricapi.Int64Observer) error {
			procs, err := process.ProcessesWithContext(ctx)
			if err == nil {
				o.Observe(int64(len(procs)))
			}
			return nil
		}))
		if err != nil {
			return err
		}
	}
	if cfg.Monitor.NetworkIO {
		_, err := meter.Int64ObservableGauge("system.network.io.bytes", metricapi.WithUnit("By"), metricapi.WithInt64Callback(func(ctx context.Context, o metricapi.Int64Observer) error {
			stats, err := gnet.IOCountersWithContext(ctx, true)
			if err == nil {
				for _, s := range stats {
					o.Observe(int64(s.BytesSent), metricapi.WithAttributes(attribute.String("network.interface.name", s.Name), attribute.String("direction", "sent")))
					o.Observe(int64(s.BytesRecv), metricapi.WithAttributes(attribute.String("network.interface.name", s.Name), attribute.String("direction", "recv")))
				}
			}
			return nil
		}))
		if err != nil {
			return err
		}
	}
	if cfg.Monitor.PowerLifecycle {
		_, err = meter.Int64ObservableGauge("system.power.state", metricapi.WithUnit("1"), metricapi.WithInt64Callback(func(ctx context.Context, o metricapi.Int64Observer) error { o.Observe(1); return nil }))
		if err != nil {
			return err
		}
		_, err = meter.Int64ObservableGauge("system.boot.time_unix", metricapi.WithUnit("s"), metricapi.WithInt64Callback(func(ctx context.Context, o metricapi.Int64Observer) error {
			boot, err := host.BootTimeWithContext(ctx)
			if err == nil {
				o.Observe(int64(boot))
			}
			return nil
		}))
		if err != nil {
			return err
		}
		_, err = meter.Int64ObservableGauge("system.uptime", metricapi.WithUnit("s"), metricapi.WithInt64Callback(func(ctx context.Context, o metricapi.Int64Observer) error {
			uptime, err := host.UptimeWithContext(ctx)
			if err == nil {
				o.Observe(int64(uptime))
			}
			return nil
		}))
		if err != nil {
			return err
		}
		_, err = meter.Int64ObservableGauge("system.lifecycle.events", metricapi.WithUnit("1"), metricapi.WithInt64Callback(func(ctx context.Context, o metricapi.Int64Observer) error {
			o.Observe(state.startupEvents.Load(), metricapi.WithAttributes(attribute.String("event", "startup")))
			o.Observe(state.shutdownEvents.Load(), metricapi.WithAttributes(attribute.String("event", "shutdown")))
			return nil
		}))
		if err != nil {
			return err
		}
	}
	if cfg.Monitor.AppLifecycle {
		_, err := meter.Int64ObservableGauge("system.app.lifecycle.events", metricapi.WithUnit("1"), metricapi.WithInt64Callback(func(ctx context.Context, o metricapi.Int64Observer) error {
			o.Observe(state.processStarts.Load(), metricapi.WithAttributes(attribute.String("event", "start")))
			o.Observe(state.processStops.Load(), metricapi.WithAttributes(attribute.String("event", "stop")))
			return nil
		}))
		if err != nil {
			return err
		}
		_, err = meter.Int64ObservableGauge("system.app.running", metricapi.WithUnit("1"), metricapi.WithInt64Callback(func(ctx context.Context, o metricapi.Int64Observer) error {
			state.mu.RLock()
			defer state.mu.RUnlock()
			for _, name := range state.runningApps {
				o.Observe(1, metricapi.WithAttributes(attribute.String("app.name", name)))
			}
			return nil
		}))
		if err != nil {
			return err
		}
	}
	if cfg.Monitor.Temperatures {
		_, err := meter.Float64ObservableGauge("system.temperature.celsius", metricapi.WithUnit("Cel"), metricapi.WithFloat64Callback(func(ctx context.Context, o metricapi.Float64Observer) error {
			temps, err := host.SensorsTemperaturesWithContext(ctx)
			if err != nil {
				return nil
			}
			var cpuTemp, gpuTemp float64
			var cpuFound, gpuFound bool
			for _, t := range temps {
				if t.Temperature == 0 {
					continue
				}
				key := strings.ToLower(t.SensorKey)
				o.Observe(t.Temperature, metricapi.WithAttributes(attribute.String("sensor", t.SensorKey)))
				if strings.Contains(key, "cpu") {
					cpuTemp += t.Temperature
					cpuFound = true
				}
				if strings.Contains(key, "gpu") {
					gpuTemp += t.Temperature
					gpuFound = true
				}
			}
			if cpuFound {
				o.Observe(cpuTemp, metricapi.WithAttributes(attribute.String("sensor", "cpu.aggregate")))
			}
			if gpuFound {
				o.Observe(gpuTemp, metricapi.WithAttributes(attribute.String("sensor", "gpu.aggregate")))
			}
			return nil
		}))
		if err != nil {
			return err
		}
	}
	return nil
}

func setupHomeAssistant(ctx context.Context, cfg Config) (mqtt.Client, error) {
	if cfg.HomeAssistant.Topic == "" {
		return nil, fmt.Errorf("home_assistant.topic saknas")
	}
	if cfg.HomeAssistant.MQTTURL == "" {
		return nil, fmt.Errorf("home_assistant.mqtt_url saknas")
	}
	if cfg.HomeAssistant.DeviceID == "" {
		return nil, fmt.Errorf("home_assistant.device_id saknas")
	}

	base := cfg.HomeAssistant.Topic
	availabilityTopic := base + "/availability"
	commandTopic := base + "/command"

	opts := mqtt.NewClientOptions().AddBroker(cfg.HomeAssistant.MQTTURL).SetClientID(cfg.HomeAssistant.DeviceID + "_agent")
	if cfg.HomeAssistant.Username != "" {
		opts.SetUsername(cfg.HomeAssistant.Username)
		opts.SetPassword(cfg.HomeAssistant.Password)
	}
	opts.SetWill(availabilityTopic, "offline", 1, true)

	client := mqtt.NewClient(opts)
	if token := client.Connect(); token.Wait() && token.Error() != nil {
		return nil, token.Error()
	}

	publish := func(topic string, payload any) error {
		b, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		t := client.Publish(topic, 1, true, b)
		t.Wait()
		return t.Error()
	}

	device := map[string]any{"identifiers": []string{cfg.HomeAssistant.DeviceID}, "name": cfg.HomeAssistant.Name, "manufacturer": "windowsmetricsotel", "model": "windows-agent"}
	if cfg.HomeAssistant.MAC != "" {
		device["connections"] = [][]string{{"mac", strings.ToLower(cfg.HomeAssistant.MAC)}}
	}

	if err := publish("homeassistant/button/"+cfg.HomeAssistant.DeviceID+"/power_control/config", map[string]any{
		"name":    cfg.HomeAssistant.Name + " Power",
		"uniq_id": cfg.HomeAssistant.DeviceID + "_power",
		"cmd_t":   commandTopic,
		"pl_prs":  "shutdown",
		"avty_t":  availabilityTopic,
		"dev":     device,
		"icon":    "mdi:power",
	}); err != nil {
		return nil, err
	}

	if err := publish("homeassistant/button/"+cfg.HomeAssistant.DeviceID+"/restart_control/config", map[string]any{
		"name":    cfg.HomeAssistant.Name + " Restart",
		"uniq_id": cfg.HomeAssistant.DeviceID + "_restart",
		"cmd_t":   commandTopic,
		"pl_prs":  "restart",
		"avty_t":  availabilityTopic,
		"dev":     device,
		"icon":    "mdi:restart",
	}); err != nil {
		return nil, err
	}

	if err := publish("homeassistant/sensor/"+cfg.HomeAssistant.DeviceID+"/state/config", map[string]any{
		"name":    cfg.HomeAssistant.Name + " State",
		"uniq_id": cfg.HomeAssistant.DeviceID + "_state",
		"stat_t":  availabilityTopic,
		"dev_cla": "connectivity",
		"pl_on":   "online",
		"pl_off":  "offline",
		"avty_t":  availabilityTopic,
		"dev":     device,
	}); err != nil {
		return nil, err
	}

	if token := client.Publish(availabilityTopic, 1, true, "online"); token.Wait() && token.Error() != nil {
		return nil, token.Error()
	}

	if token := client.Subscribe(commandTopic, 1, func(_ mqtt.Client, m mqtt.Message) {
		cmd := strings.TrimSpace(strings.ToLower(string(m.Payload())))
		if err := handleControlCommand(ctx, cmd); err != nil {
			log.Printf("home assistant command '%s' misslyckades: %v", cmd, err)
		}
	}); token.Wait() && token.Error() != nil {
		return nil, token.Error()
	}

	log.Printf("Home Assistant MQTT discovery aktiv: topic=%s", base)
	return client, nil
}

func handleControlCommand(ctx context.Context, cmd string) error {
	switch cmd {
	case "shutdown":
		return exec.CommandContext(ctx, "shutdown", "/s", "/t", "0").Start()
	case "restart":
		return exec.CommandContext(ctx, "shutdown", "/r", "/t", "0").Start()
	case "sleep":
		return exec.CommandContext(ctx, "rundll32.exe", "powrprof.dll,SetSuspendState", "0,1,0").Start()
	default:
		return fmt.Errorf("okänt kommando: %s", cmd)
	}
}

func trackProcessLifecycle(ctx context.Context, state *runtimeState, interval time.Duration, source processSource, sink lifecycleSink) {
	if err := bootstrapProcessSnapshot(ctx, state, source); err != nil {
		log.Printf("kunde inte läsa initial processlista: %v", err)
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := updateProcessSnapshot(ctx, state, source, sink); err != nil {
			log.Printf("kunde inte läsa processlista: %v", err)
		}
		<-ticker.C
	}
}

func bootstrapProcessSnapshot(ctx context.Context, state *runtimeState, source processSource) error {
	snapshot, err := source.List(ctx)
	if err != nil {
		return err
	}
	state.mu.Lock()
	state.runningApps = snapshotToMap(snapshot)
	state.mu.Unlock()
	return nil
}

func updateProcessSnapshot(ctx context.Context, state *runtimeState, source processSource, sink lifecycleSink) error {
	if sink == nil {
		sink = nopLifecycleSink{}
	}
	snapshot, err := source.List(ctx)
	if err != nil {
		return err
	}
	current := snapshotToMap(snapshot)
	state.mu.Lock()
	defer state.mu.Unlock()
	for pid, name := range current {
		if _, ok := state.runningApps[pid]; !ok {
			state.processStarts.Add(1)
			sink.OnStart(processInfo{PID: pid, Name: name})
		}
	}
	for pid, name := range state.runningApps {
		if _, ok := current[pid]; !ok {
			state.processStops.Add(1)
			sink.OnStop(processInfo{PID: pid, Name: name})
		}
	}
	state.runningApps = current
	return nil
}

func snapshotToMap(snapshot []processInfo) map[int32]string {
	out := make(map[int32]string, len(snapshot))
	for _, p := range snapshot {
		n := p.Name
		if n == "" {
			n = "unknown"
		}
		out[p.PID] = n
	}
	return out
}

func configPath() string {
	if v := os.Getenv("APP_CONFIG_FILE"); v != "" {
		return v
	}
	return "config.json"
}

func loadConfig(path string) (Config, error) {
	cfg := defaultConfig()
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cfg, nil
		}
		return Config{}, err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return cfg, nil
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	applyConfigDefaults(&cfg)
	return cfg, nil
}

func applyConfigDefaults(cfg *Config) {
	def := defaultConfig()
	if cfg.OTLP.Endpoint == "" {
		cfg.OTLP.Endpoint = def.OTLP.Endpoint
	}
	if cfg.OTLP.MetricsPath == "" {
		cfg.OTLP.MetricsPath = def.OTLP.MetricsPath
	}
	if cfg.OTLP.ExportInterval == "" {
		cfg.OTLP.ExportInterval = def.OTLP.ExportInterval
	}
	if cfg.Runtime.AppScanInterval == "" {
		cfg.Runtime.AppScanInterval = def.Runtime.AppScanInterval
	}
	if cfg.HomeAssistant.MQTTURL == "" {
		cfg.HomeAssistant.MQTTURL = def.HomeAssistant.MQTTURL
	}
	if cfg.HomeAssistant.Topic == "" {
		cfg.HomeAssistant.Topic = def.HomeAssistant.Topic
	}
	if cfg.HomeAssistant.DeviceID == "" {
		cfg.HomeAssistant.DeviceID = def.HomeAssistant.DeviceID
	}
	if cfg.HomeAssistant.Name == "" {
		cfg.HomeAssistant.Name = def.HomeAssistant.Name
	}
}

func parseDuration(value string, fallback time.Duration) time.Duration {
	if value == "" {
		return fallback
	}
	d, err := time.ParseDuration(value)
	if err != nil {
		return fallback
	}
	return d
}

func waitForSignal(state *runtimeState) {
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)
	<-c
	state.shutdownEvents.Add(1)
}
