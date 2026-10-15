# API

Everything the interface does is a documented HTTP call, so the panel is
scriptable. Every endpoint is **read-only**: there is no call that starts,
stops or changes anything, on this machine or on another.

Two endpoints are reachable without a session (`/api/health` and
`/api/auth/session`); everything else needs the session cookie that
`POST /api/auth/login` sets.

## Authentication

```bash
# Log in and keep the cookie.
curl -c jar -X POST -H 'Content-Type: application/json' \
     -d '{"password":"secret"}' \
     http://127.0.0.1:9110/api/auth/login

# Use it.
curl -b jar http://127.0.0.1:9110/api/snapshot
```

`POST` endpoints require the `Origin` header to match the host, which is
what stops a page on another site from driving your browser. Sessions are
`HttpOnly` and `SameSite=Strict`.

Failed logins are throttled: after 8 attempts from one address within 15
minutes, the endpoint answers `429` with a `retry_in_seconds` field and a
`Retry-After` header.

---

### `GET /api/health`

No session required. It reveals nothing about the machine: a supervisor
needs to know whether the process is alive, not what it monitors.

```json
{ "status": "ok", "version": "v1.0.0", "uptime": 3600, "demo": false }
```

### `POST /api/auth/login`

```json
{ "password": "secret" }
```

Answers with the session description below and sets the cookie.
`401` for a wrong password, `429` when throttled, `403` when the `Origin`
header does not match.

### `POST /api/auth/logout`

Expires the cookie. Answers `{"authenticated": false}`.

### `GET /api/auth/session`

```json
{
  "authenticated": true,
  "demo": false,
  "base_path": "",
  "version": "v1.0.0",
  "theme": "lotus-dark",
  "poll_ms": 2000
}
```

`poll_ms` is how often the interface should refresh the snapshot;
`password_is_plain` appears when the configuration holds a readable
password rather than a hash.

### `GET /api/snapshot`

The whole picture. This is the endpoint the interface lives on.

```json
{
  "time": 1792093860000,
  "uptime": 3325912.4,
  "temp_c": 47.5,
  "host": {
    "hostname": "web-1", "os": "Debian GNU/Linux 13 (trixie)",
    "kernel": "6.12.9-amd64", "arch": "amd64",
    "virtualization": "kvm", "cpus": 4
  },
  "cpu": {
    "usage": 19.87, "cores": [12.1, 44.0, 19.7, 3.7],
    "load1": 0.83, "load5": 0.61, "load15": 0.44,
    "iowait": 3.92, "steal": 0.4,
    "model": "AMD Ryzen 5 5600X 6-Core Processor", "mhz": 3.71
  },
  "memory": {
    "total": 17179869184, "used": 8372362362, "available": 8807506832,
    "free": 764706816, "cached": 2576980377, "buffers": 325058560,
    "swap_total": 4294967296, "swap_used": 637104160,
    "used_pct": 48.74, "swap_pct": 14.83
  },
  "disks": [
    {
      "mount": "/", "device": "/dev/nvme0n1p2", "fstype": "ext4",
      "total": 502463922176, "used": 207160188928, "free": 295303733248,
      "used_pct": 41.24, "inodes_pct": 14.4,
      "read_bps": 197120, "write_bps": 84224, "read_iops": 0.5, "write_iops": 0.4,
      "read_only": false, "device_base": "nvme0n1p2"
    }
  ],
  "net": {
    "online": true, "checked_via": "tcp:1.1.1.1:53", "latency_ms": 12.97,
    "dns_ok": true, "dns_ms": 14, "dns_name": "one.one.one.one",
    "rx_bps": 13926400, "tx_bps": 2621440,
    "rx_total": 3408486046105, "tx_total": 1011550697553,
    "ifaces": [
      {
        "name": "eth0", "up": true, "mac": "52:54:00:8f:1c:2a",
        "rx_bps": 13926400, "tx_bps": 2621440,
        "rx_total": 3408486046105, "tx_total": 1011550697553,
        "rx_errs": 0, "tx_errs": 0, "rx_drops": 3, "tx_drops": 0,
        "addrs": ["192.168.1.42/24"]
      }
    ],
    "listening": [
      {
        "proto": "tcp", "addr": "0.0.0.0", "port": 443,
        "process": "nginx", "pid": 1204, "exposed": true
      }
    ]
  },
  "procs": [
    {
      "pid": 2210, "ppid": 1, "user": "postgres", "name": "postgres",
      "cmdline": "/usr/lib/postgresql/16/bin/postgres -D /var/lib/postgresql",
      "state": "sleeping", "cpu": 8.7, "mem_pct": 22.6,
      "rss": 3883794432, "threads": 31,
      "started": 1791960000000, "uptime": 133800
    }
  ],
  "total_procs": 214,
  "services": {
    "systemd": {
      "available": true, "version": "257.1",
      "total": 13, "running": 10, "failed": 1,
      "units": [
        {
          "kind": "systemd", "name": "nginx.service",
          "description": "A high performance web server",
          "load_state": "loaded", "active_state": "active", "sub_state": "running",
          "status": "running", "enabled": "enabled"
        }
      ]
    },
    "docker": {
      "available": true, "version": "27.4.1",
      "total": 5, "running": 3, "unhealthy": 0, "images": 14,
      "items": [
        {
          "kind": "docker", "name": "web", "description": "nginx:1.27-alpine Up 4 days (healthy)",
          "active_state": "running", "sub_state": "Up 4 days (healthy)",
          "status": "running", "health": "healthy",
          "image": "nginx:1.27-alpine", "started": 1791636000000, "restarts": 2
        }
      ]
    }
  },
  "issues": [
    {
      "id": "disk.high", "severity": "warn",
      "title": "Filesystem filling up", "subject": "/var/backups",
      "detail": "/var/backups is 89.4% full: 220.1 GiB free of 2.0 TiB",
      "hint": "Plan the cleanup now rather than at 100%.",
      "value": 89.4, "threshold": 88
    }
  ],
  "verdict": "warn",
  "collect_ms": 2.8
}
```

Notes:

- `verdict` is the worst severity in `issues`: `ok`, `info`, `warn` or `crit`.
- `cpu.usage` is a percentage across all cores, and `cpu.cores` holds one
  entry per logical processor.
- A process's `cpu` is its share of the whole machine, the same convention
  `top` uses, so four busy threads on a four core box read as 100%.
- `exposed` on a listening socket means it is bound to a wildcard address
  and is therefore reachable from the network, not just from localhost.
- Rate fields (`rx_bps`, `read_bps`, ...) are bytes per second and are zero on
  the very first sample, because a rate needs two readings.
- `systemd.error` and `docker.error` explain why a section is unavailable
  rather than leaving it mysteriously empty.

### `GET /api/history?range=1h&points=360`

`range` is one of `15m`, `1h`, `6h`, `24h`, `7d`, `30d`. `points` is the
budget (30 to 2000, default 360).

```json
{
  "range": "1h",
  "from": 1792090264316,
  "to": 1792093864316,
  "resolution": "raw",
  "points": [
    { "time": 1792093860000, "cpu_pct": 19.87, "mem_pct": 48.74, "swap_pct": 14.83,
      "disk_pct": 41.24, "load1": 0.83, "rx_bps": 13926400, "tx_bps": 2621440,
      "procs": 214, "issues": 1, "crit": 0, "online": true, "net_ms": 12.97,
      "temp_c": 47.5, "disk_read_bps": 197120, "disk_write_bps": 84224 }
  ]
}
```

`resolution` says what the numbers are: `raw` for individual samples,
`minute` or `hour` for averages. Individual samples are always preferred
when they fit in the point budget; when the aggregate tables are still
empty - the first minutes after a restart - the answer comes from the raw
rows rather than being empty.

### `GET /api/events?limit=50`

Findings recorded when they first appeared, so a problem that was fixed an
hour ago is still visible. Only warnings and critical findings are
recorded, and only when they change: a steady problem is not repeated.

```json
{
  "events": [
    { "time": 1792093855561, "severity": "crit", "id": "docker.restarting",
      "title": "Container restart loop: worker",
      "detail": "ghcr.io/example/worker:2.4.1: the container keeps restarting" }
  ]
}
```

### `GET /api/meta`

Identity, uptime, the threshold table and what the database holds.

```json
{
  "version": "v1.0.0", "demo": false,
  "started": 1792093853561, "now": 1792093864321, "base_path": "",
  "thresholds": { "disk_warn": 88, "disk_crit": 95, "mem_warn": 90, "mem_crit": 96 },
  "history": {
    "path": "/var/lib/eye-of-lotus/history.db", "size": 131072,
    "raw": 481, "minute": 96, "hour": 0, "events": 3,
    "oldest": 1792093860000, "newest": 1792093860000, "healthy": true
  }
}
```

`history` is absent when no database is configured.
