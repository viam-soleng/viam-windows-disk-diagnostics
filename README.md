# Module windows-diagnostics

This module provides Windows disk diagnostics capabilities for Viam-powered machines.

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
