# Module windows-diagnostics

This module provides Windows device telemetry sensors for Viam-powered machines: disk space, CPU usage, and memory usage.

## Model bill:windows-diagnostics:disk

This Windows diagnostics model for Viam enables monitoring of disk space on a specified volume.

### Configuration

The following attribute template can be used to configure this model:

```json
{
  "path": "<string>"
}
```

#### Attributes

The following attributes are available for this model:

| Name   | Type   | Inclusion | Description                                              |
|--------|--------|-----------|----------------------------------------------------------|
| `path` | string | Required  | The path to the disk or volume to diagnose (e.g. `C:\\`) |

#### Example Configuration

```json
{
  "path": "C:\\"
}
```

## Model bill:windows-diagnostics:cpu

Reports system-wide CPU usage computed as the delta between successive calls to `GetSystemTimes`.

> **Note:** The first reading always returns `used_percent: 0` because there is no prior sample to diff against. Subsequent readings will reflect actual usage.

### Readings

| Key            | Type  | Description                         |
|----------------|-------|-------------------------------------|
| `used_percent` | float | CPU utilization (0–100), 1 decimal  |
| `idle_percent` | float | CPU idle time (0–100), 1 decimal    |

### Configuration

No attributes required.

```json
{}
```

## Model bill:windows-diagnostics:memory

Reports physical memory usage via `GlobalMemoryStatusEx`.

### Readings

| Key               | Type   | Description                              |
|-------------------|--------|------------------------------------------|
| `total_bytes`     | uint64 | Total installed physical memory          |
| `available_bytes` | uint64 | Memory available to the current process  |
| `used_bytes`      | uint64 | Memory in use (`total - available`)      |
| `used_percent`    | float  | Percentage used (0–100), 1 decimal       |

### Configuration

No attributes required.

```json
{}
```
