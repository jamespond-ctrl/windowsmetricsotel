# windowsmetricsotel

En liten Windows-agent som kör i bakgrunden och skickar metrics via **OTLP/HTTP** till din OpenTelemetry Collector.

## Home Assistant-stöd (out of the box)

Appen kan automatiskt dyka upp i Home Assistant via **MQTT Discovery**.

När `home_assistant.enabled=true`:
- publiceras discovery-konfiguration med `retain` (enheten finns kvar i HA även när datorn är avstängd),
- availability sätts till `online/offline` (HA ser om datorn är igång),
- två knappar exponeras i HA:
  - **Power** (`shutdown`)
  - **Restart** (`restart`)
- appen lyssnar på command-topic för styrning.

> För att kunna väcka datorn när den är avstängd använder du Home Assistants `wake_on_lan`-integration med samma MAC-adress som i configfilen.

## Konfiguration med fil

Standard: `config.json` i samma mapp som `.exe`.

Du kan sätta alternativ sökväg via `APP_CONFIG_FILE`.

### Exempel `config.json`

```json
{
  "otlp": {
    "endpoint": "192.168.1.50:4318",
    "metrics_path": "/v1/metrics",
    "insecure": true,
    "export_interval": "10s"
  },
  "runtime": {
    "app_scan_interval": "3s"
  },
  "monitor": {
    "cpu": true,
    "memory": true,
    "disk": true,
    "process_count": true,
    "network_io": true,
    "power_lifecycle": true,
    "app_lifecycle": true,
    "temperatures": true
  },
  "home_assistant": {
    "enabled": true,
    "mqtt_url": "tcp://192.168.1.20:1883",
    "username": "mqtt-user",
    "password": "mqtt-pass",
    "topic": "kids_pc",
    "device_id": "kids_pc_win11",
    "name": "Kids PC",
    "mac": "AA:BB:CC:DD:EE:FF"
  }
}
```

## WOL i Home Assistant

Lägg till (exempel) i HA:

```yaml
wake_on_lan:

switch:
  - platform: wake_on_lan
    name: Kids PC Wake
    mac: "AA:BB:CC:DD:EE:FF"
    host: 192.168.1.123
```

Då känner HA igen enheten via MQTT även när den är offline, och du kan väcka den via WOL-switchen.

## Bygg

```bash
GOOS=windows GOARCH=amd64 go build -o windowsmetricsotel.exe .
```
