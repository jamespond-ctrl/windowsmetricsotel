package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

type mockProcessSource struct {
	snapshots [][]processInfo
	idx       int
}

func (m *mockProcessSource) List(context.Context) ([]processInfo, error) {
	if len(m.snapshots) == 0 {
		return nil, nil
	}
	if m.idx >= len(m.snapshots) {
		return m.snapshots[len(m.snapshots)-1], nil
	}
	s := m.snapshots[m.idx]
	m.idx++
	return s, nil
}

type mockSink struct {
	starts []processInfo
	stops  []processInfo
}

func (m *mockSink) OnStart(p processInfo) { m.starts = append(m.starts, p) }
func (m *mockSink) OnStop(p processInfo)  { m.stops = append(m.stops, p) }

func TestBootstrapProcessSnapshotFetchesDataFromSource(t *testing.T) {
	state := &runtimeState{runningApps: map[int32]string{}}
	src := &mockProcessSource{snapshots: [][]processInfo{{{PID: 10, Name: "explorer.exe"}, {PID: 11, Name: "chrome.exe"}}}}
	if err := bootstrapProcessSnapshot(context.Background(), state, src); err != nil {
		t.Fatalf("bootstrapProcessSnapshot error: %v", err)
	}
	want := map[int32]string{10: "explorer.exe", 11: "chrome.exe"}
	if !reflect.DeepEqual(state.runningApps, want) {
		t.Fatalf("runningApps mismatch\nwant: %#v\ngot:  %#v", want, state.runningApps)
	}
}

func TestUpdateProcessSnapshotFetchesAndSendsLifecycleEvents(t *testing.T) {
	state := &runtimeState{runningApps: map[int32]string{}}
	src := &mockProcessSource{snapshots: [][]processInfo{
		{{PID: 10, Name: "explorer.exe"}, {PID: 11, Name: "steam.exe"}},
		{{PID: 10, Name: "explorer.exe"}, {PID: 12, Name: "code.exe"}},
	}}
	sink := &mockSink{}
	if err := bootstrapProcessSnapshot(context.Background(), state, src); err != nil {
		t.Fatalf("bootstrapProcessSnapshot error: %v", err)
	}
	if err := updateProcessSnapshot(context.Background(), state, src, sink); err != nil {
		t.Fatalf("updateProcessSnapshot error: %v", err)
	}
	if state.processStarts.Load() != 1 || state.processStops.Load() != 1 {
		t.Fatalf("unexpected counters: starts=%d stops=%d", state.processStarts.Load(), state.processStops.Load())
	}
	if len(sink.starts) != 1 || sink.starts[0].Name != "code.exe" || len(sink.stops) != 1 || sink.stops[0].Name != "steam.exe" {
		t.Fatalf("unexpected sink events: starts=%#v stops=%#v", sink.starts, sink.stops)
	}
}

func TestSnapshotToMapNormalizesEmptyNames(t *testing.T) {
	got := snapshotToMap([]processInfo{{PID: 1, Name: ""}, {PID: 2, Name: "cmd.exe"}})
	want := map[int32]string{1: "unknown", 2: "cmd.exe"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshotToMap mismatch\nwant: %#v\ngot:  %#v", want, got)
	}
}

func TestLoadConfigUsesDefaultsWhenFileMissing(t *testing.T) {
	cfg, err := loadConfig(filepath.Join(t.TempDir(), "does-not-exist.json"))
	if err != nil {
		t.Fatalf("loadConfig error: %v", err)
	}
	if cfg.OTLP.Endpoint != "192.168.1.10:4318" || !cfg.Monitor.AppLifecycle {
		t.Fatalf("unexpected defaults: %#v", cfg)
	}
	if cfg.HomeAssistant.Topic != "windowsmetricsotel" {
		t.Fatalf("unexpected HA default topic: %s", cfg.HomeAssistant.Topic)
	}
}

func TestLoadConfigAppliesOverridesAndKeepsUnspecifiedDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	json := `{
  "otlp": {"endpoint": "10.0.0.5:4318"},
  "home_assistant": {"enabled": true, "mqtt_url": "tcp://10.0.0.2:1883", "topic": "pc/kid", "device_id": "kid_pc", "name": "Kid PC", "mac": "AA:BB:CC:DD:EE:FF"},
  "monitor": {"cpu": true, "memory": false, "disk": false, "process_count": true, "network_io": false, "power_lifecycle": true, "app_lifecycle": false, "temperatures": false}
}`
	if err := os.WriteFile(path, []byte(json), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig error: %v", err)
	}
	if cfg.OTLP.MetricsPath != "/v1/metrics" || cfg.HomeAssistant.DeviceID != "kid_pc" || cfg.HomeAssistant.Name != "Kid PC" {
		t.Fatalf("unexpected merged config: %#v", cfg)
	}
}

func TestHandleControlCommandRejectsUnknown(t *testing.T) {
	if err := handleControlCommand(context.Background(), "blabla"); err == nil {
		t.Fatal("expected unknown command error")
	}
}
