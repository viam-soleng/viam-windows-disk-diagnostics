# Module windows-diagnostics

This module provides Windows device telemetry sensors for Viam-powered machines: disk space, CPU usage, and memory usage.

## Model viam:windows-diagnostics:disk

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

## Model viam:windows-diagnostics:cpu

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

## Model viam:windows-diagnostics:memory

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

## Model viam:windows-diagnostics:tasklist

Reports the list of processes currently running on the machine — the equivalent of the `tasklist` command.

### Readings

| Key             | Type  | Description                                                 |
|-----------------|-------|-------------------------------------------------------------|
| `process_count` | int   | Number of processes reported (after applying `name_filter`) |
| `processes`     | array | One object per process, sorted by PID                       |

Each entry in `processes` contains `pid`, `ppid`, `name` (e.g. `firefox.exe`), `threads`, and `cpu_percent`.

`cpu_percent` is the process's share of total CPU capacity (0–100, across all logical processors, like Task Manager).

### Configuration

| Name          | Type   | Inclusion | Description                                                                                          |
|---------------|--------|-----------|------------------------------------------------------------------------------------------------------|
| `name_filter` | string | Optional  | Case-insensitive substring; only processes whose executable name contains it are reported. Empty reports all. |

```json
{
  "name_filter": "firefox"
}
```

## Model viam:windows-diagnostics:diskcleanup

Reclaims space on a Windows volume that Windows Update fills and never cleans up. It exists for the failure mode where `C:` runs out of room and `viam-server` goes into an unhealthy loop because it cannot unpack modules.

`Readings()` is always non-destructive, so this model is safe to poll and to put under data capture. Nothing is ever deleted except by an explicit `run` command or by a configured auto-cleanup threshold being crossed.

### Cleanup tasks

| Task | What it removes | Typical yield | Speed |
|------|-----------------|---------------|-------|
| `software_distribution` | Contents of `C:\Windows\SoftwareDistribution\Download`, the Windows Update payload cache. `wuauserv`, `bits`, `usosvc`, and `dosvc` are stopped first and always restarted afterwards. | 1–10 GB | Seconds |
| `temp_files` | Contents of `C:\Windows\Temp` and the service account's `%TEMP%`, older than `temp_min_age_hours`. | 100 MB – several GB | Seconds |
| `delivery_optimization` | The peer-to-peer update cache, via `Delete-DeliveryOptimizationCache` with a direct directory purge as fallback. Separate storage from `SoftwareDistribution`. | 0–5 GB | Seconds |
| `dism_component_cleanup` | Superseded component versions in WinSxS, via `Dism.exe /Online /Cleanup-Image /StartComponentCleanup`. | 2–8 GB | Minutes to tens of minutes |
| `cleanmgr` | The built-in Disk Cleanup handlers, run headlessly from a registry answer file. Opt-in. | Varies | Minutes, plus a slow reboot |

`cleanmgr /sagerun` hands its work to a separate process and returns immediately, so this task waits for that process to exit before moving on — otherwise its duration and freed-bytes figures would be measured against nothing and the next task would start while cleanmgr was still deleting. Its Update Cleanup handler is the part no wait can cover: that one stages its work and finishes during the next restart, which is why the task always reports `reboot_required`.

The tasks run in the order above regardless of how `tasks` is written, so a critically full machine recovers space within seconds before the slow passes start. A failing task does not abort the ones after it.

`cleanmgr` is not in the default task set. It overlaps the tasks above, its Windows Update handler only finishes during the next restart, and that restart is slow. Enable it when you also want the handlers nothing else covers — Windows.old from a feature update, error reports, thumbnails, the recycle bin.

### Requirements

`dism_component_cleanup` is skipped, not failed, when Windows has pending servicing operations — the `Component Based Servicing\RebootPending` or `WindowsUpdate\Auto Update\RebootRequired` registry keys, which are what DISM reports as `0x800f0806` (`CBS_E_PENDING`). The store cannot be serviced until the machine restarts, so the task reports `status: "skipped"` with `reboot_required: true` and the run continues with the other tasks. This is checked before DISM starts as well as after, so a machine in that state does not spend minutes in DISM to be told the same thing by exit code.

`dism_component_cleanup` and `cleanmgr` routinely exit with 3010 (`ERROR_SUCCESS_REBOOT_REQUIRED`) on a machine that has not been cleaned before. That is a success — the work is done and a restart finishes it — so the task reports `status: "ok"` with `reboot_required: true`, not an error.

All tasks require administrator rights. `viam-server` installed as a Windows service runs as LocalSystem and has them; a hand-launched `viam-server` in a normal shell does not, and the module logs a warning at startup when it is not elevated.

### Readings

| Key | Type | Description |
|-----|------|-------------|
| `path` | string | Volume being watched |
| `total_bytes`, `free_bytes`, `used_bytes` | uint64 | Volume capacity and usage |
| `free_percent`, `used_percent` | float | Percentage, 1 decimal |
| `reclaimable_bytes` | uint64 | Total currently held by the directory-based tasks |
| `reclaimable_by_task` | object | Bytes per task |
| `reclaimable_estimate_ready` | bool | False until the first background measurement completes |
| `reclaimable_estimate_age_seconds` | float | Age of the cached measurement |
| `cleanup_running` | bool | Whether a cleanup is in flight |
| `cleanup_current_task`, `cleanup_elapsed_seconds` | string, float | Present while running |
| `last_*` | | Summary of the last completed run: `last_trigger`, `last_started_at`, `last_finished_at`, `last_duration_seconds`, `last_freed_bytes`, `last_free_before`, `last_free_after`, `last_free_measured`, `last_tasks` (per-task status, freed bytes, items removed, `reboot_required`, and detail) |
| `elevated` | bool | Whether the module has the rights its tasks need |
| `reboot_required` | bool | Windows is waiting on a restart; servicing work queues behind it |

`reclaimable_by_task` covers the directory-based tasks only. DISM and `cleanmgr` cannot be sized without a multi-minute analysis, so use the `analyze` command for those.

`freed_bytes` is a before/after delta of the volume's free space, which is the only figure available for DISM and cleanmgr. When either sample cannot be read, `free_measured` is false and `freed_bytes` reports 0 rather than a fabricated number.

Directory sizes are measured on a background goroutine and cached for `estimate_ttl_seconds`, so `Readings()` stays cheap enough to poll at any interval.

### Configuration

| Name | Type | Inclusion | Description |
|------|------|-----------|-------------|
| `path` | string | Optional | Volume to watch. Defaults to `C:\` |
| `tasks` | array | Optional | Which tasks to run. Defaults to everything except `cleanmgr` |
| `dism_reset_base` | bool | Optional | Add `/ResetBase` to the component cleanup. Default `false` |
| `temp_min_age_hours` | float | Optional | Minimum age before a temp file is deleted. Default `24` |
| `cleanmgr_sageset_id` | int | Optional | Registry answer-file slot, 0–9999. Default `99` |
| `cleanmgr_handlers` | array | Optional | Restrict `cleanmgr` to these handler names (e.g. `"Update Cleanup"`). Empty enables every handler on the machine |
| `auto_cleanup_below_free_percent` | float | Optional | Run a cleanup when free space drops below this percentage. `0` disables |
| `auto_cleanup_below_free_bytes` | uint64 | Optional | Run a cleanup when free space drops below this many bytes. `0` disables |
| `min_cleanup_interval_hours` | float | Optional | Rate limit for automatic cleanups. Default `24`, `0` disables |
| `task_timeout_seconds` | int | Optional | Per-task timeout. Default `2700` (45 minutes) |
| `estimate_ttl_seconds` | int | Optional | How long a reclaimable-space measurement is reused. Default `600` |

`dism_reset_base` reclaims more space but makes every already-installed update permanently un-uninstallable. Leave it off unless you have decided you will never need to roll an update back.

Auto-cleanup is evaluated inside `Readings()`, so it is driven by whatever polling interval you already configured for this sensor — no separate schedule to maintain. It is off by default; set a threshold to turn it on. `min_cleanup_interval_hours` keeps a machine that is full for a reason cleanup cannot fix from running DISM on every poll; it counts from the last run of either kind, so a manual cleanup also holds off the next automatic one.

#### Example Configuration

Unattended: watch `C:\`, clean automatically when it drops under 15% free, at most once a day.

```json
{
  "path": "C:\\",
  "auto_cleanup_below_free_percent": 15,
  "min_cleanup_interval_hours": 24
}
```

Report-only, cleaned by hand via `DoCommand`:

```json
{
  "path": "C:\\"
}
```

Everything, including `cleanmgr` and `/ResetBase`. **`dism_reset_base` is irreversible** — with it on, none of the updates already installed on this machine can ever be uninstalled:

```json
{
  "path": "C:\\",
  "tasks": [
    "software_distribution",
    "temp_files",
    "delivery_optimization",
    "dism_component_cleanup",
    "cleanmgr"
  ],
  "dism_reset_base": true,
  "auto_cleanup_below_free_percent": 20
}
```

### DoCommand

| Command | Description |
|---------|-------------|
| `{"command": "run"}` | Start a cleanup. Optional `"tasks": [...]` overrides the configured task list for this run, `"wait": true` blocks until it finishes |
| `{"command": "status"}` | Report the in-flight run and the last completed one. `tasks` is what the running cleanup is actually executing, which a manual override can make differ from `configured_tasks` |
| `{"command": "estimate"}` | Re-measure reclaimable space now and return it |
| `{"command": "analyze"}` | Report the DISM component-store analysis, starting one in the background if nothing is cached. Optional `"wait": true` blocks until it finishes, `"refresh": true` forces a new analysis |

`run` is never rate-limited — asking for a cleanup by hand is itself the signal that one is wanted — and is refused only while another run is already in flight. Without `"wait": true` it returns immediately and the run continues in the background; poll `Readings()` or `status` for progress.

```json
{"command": "run", "tasks": ["software_distribution", "temp_files"], "wait": true}
```

`analyze` does not run DISM on the caller's connection. `/AnalyzeComponentStore` takes minutes on a slow machine — longer than the gRPC deadline on a `DoCommand` — so running it inline meant the caller's deadline killed DISM and the command returned an error instead of a report. Instead the first call starts the analysis and returns `{"running": true, "complete": false}`; call it again to collect the cached result, which carries `age_seconds` and `duration_seconds` alongside `output` and `cleanup_recommended`. Use `"wait": true` if your caller can hold the connection open; giving up on that wait does not stop the analysis. A result stays cached until `"refresh": true` asks for a new one.

The reverse also holds: a cleanup's `dism_component_cleanup` task waits for an in-flight analysis to finish before starting, rather than opening a second DISM session on top of it. Analysis is refused while a cleanup is running `dism_component_cleanup`, since Windows permits only one servicing operation at a time; the response carries a `reason` saying so. `status` reports `analyzing` and `last_analysis` too.

Omitting `tasks` runs the configured set. Passing an empty array is an error rather than a silent fallback to that set, so a caller that builds the list programmatically cannot turn "run nothing" into "run everything, DISM included".
