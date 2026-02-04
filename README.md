# Nvidia Fan Control

A lightweight Linux utility for monitoring GPU temperatures and dynamically controlling NVIDIA GPU fan speeds using NVML.

This fork adds:
- A **daemon mode** (original behavior, config-driven)
- A **CLI mode** to **status** GPUs and **set/auto** fan speeds manually
- Optional **curve mode** (`-curve`) for smooth fan transitions (with an AUTO floor)

## Requirements
- NVIDIA GPUs with NVML support
- NVIDIA drivers 520 or higher

## Build
```bash
go build -o nvidia_fan_control
```

## Installation
```bash
sudo install -m 0755 ./nvidia_fan_control /usr/local/bin/nvidia_fan_control
```

## Usage

### Show GPU temperature + fan speeds (no sudo)
```bash
nvidia_fan_control status
```

### Set fan speed manually (requires sudo)
Set GPU 0, fans 0 and 1, to 80%:
```bash
sudo nvidia_fan_control set -gpu 0 -fans "0,1" -speed 80
```

### Return fans to automatic control (requires sudo)
```bash
sudo nvidia_fan_control auto -gpu 0 -fans "0,1"
```

### Run as a daemon (requires sudo)
Run using a config file (defaults to `config.json` in the working directory if `-config` not set):
```bash
sudo nvidia_fan_control daemon -config /path/to/config.json
```

Enable smooth curve behavior:
```bash
sudo nvidia_fan_control daemon -config /path/to/config.json -curve
```

## Flags (CLI)

### `status`
No flags required.

### `set`
- `-gpu <N>`: GPU index (default: 0)
- `-fans "<list>"`: comma-separated fan indices (e.g. `"0,1"`)
- `-speed <0-100>`: fan speed percentage

Example:
```bash
sudo nvidia_fan_control set -gpu 0 -fans "0,1" -speed 100
```

### `auto`
- `-gpu <N>`: GPU index (default: 0)
- `-fans "<list>"`: comma-separated fan indices (e.g. `"0,1"`)

Example:
```bash
sudo nvidia_fan_control auto -gpu 0 -fans "0,1"
```

### `daemon`
- `-config <path>`: path to config JSON (default: `config.json`)
- `-curve`: enable curve mode (smooth fan transitions)

Example:
```bash
sudo nvidia_fan_control daemon -config /home/user/.nvidia_fan_control/config.json -curve
```

### 'Game Mode'
Call from tools like gamemoderun in the custom section
- `nvidia_fan_control gamemode on`: Tells the daemon to switch to game mode. This prevents the tool from setting the fan to AUTO, in practice this mean the fan will stay in the lowest manual set state instead of retuning to 0% or system control, for when a game is running and you want to force a higher setting as the floor.
- `nvidia_fan_control gamemode off`: Tells the daemon to return to normal operation, allowing AUTO.
- `nvidia_fan_control gamemode status`: Returns the current game mode setting.

Example gamemoderun config usage:

```[custom]
start=/usr/local/bin/nvidia_fan_control gamemode on
end=/usr/local/bin/nvidia_fan_control gamemode off
```

## Configuration

Edit the file `config.json` with the following structure:

```json
{
  "time_to_update": 5,
  "temperature_ranges": [
    { "min_temperature": 0,   "max_temperature": 40,  "fan_speed": 30,  "hysteresis": 3 },
    { "min_temperature": 40,  "max_temperature": 60,  "fan_speed": 40,  "hysteresis": 3 },
    { "min_temperature": 60,  "max_temperature": 80,  "fan_speed": 70,  "hysteresis": 3 },
    { "min_temperature": 80,  "max_temperature": 100, "fan_speed": 100, "hysteresis": 3 },
    { "min_temperature": 100, "max_temperature": 200, "fan_speed": 100, "hysteresis": 0 }
  ]
}
```

### Notes
- `time_to_update` is the poll interval in **seconds**
- `hysteresis` is a temperature deadband (°C) used to prevent rapid fan oscillation

### Curve mode (`-curve`)
In curve mode, the daemon uses your config as **anchors** and interpolates between them to smooth the fan ramp. It also supports an **AUTO floor**: below the configured floor temperature it will set fan policy back to automatic (useful on GPUs that clamp or ignore “manual 0%”).

A common pattern:

- `<40°C` => AUTO floor / idle behavior, setting a speed of 0 in the config will set the fans to AUTO
- `40°C` => 60%
- `60°C+` => 100%

```json
{
  "time_to_update": 5,
  "temperature_ranges": [
    { "min_temperature": 0,  "max_temperature": 40,  "fan_speed": 0,   "hysteresis": 3 },
    { "min_temperature": 40, "max_temperature": 60,  "fan_speed": 60,  "hysteresis": 3 },
    { "min_temperature": 60, "max_temperature": 200, "fan_speed": 100, "hysteresis": 0 }
  ]
}
```

## Service

Create the systemd unit:

```bash
sudo nano /etc/systemd/system/nvidia-fan-control.service
```

Example (adjust the config path):

```ini
[Unit]
Description=NVIDIA Fan Control Service
After=multi-user.target

[Service]
Type=simple
ExecStart=/usr/local/bin/nvidia_fan_control daemon -config /home/user/.nvidia_fan_control/config.json -curve
Restart=on-failure
RestartSec=2

[Install]
WantedBy=multi-user.target
```

Enable + start:

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now nvidia-fan-control.service
systemctl status nvidia-fan-control.service
```

### Check Logs
```bash
sudo tail -f /var/log/nvidia_fan_control.log
```
