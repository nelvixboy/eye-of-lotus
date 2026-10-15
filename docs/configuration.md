# Configuration

Eye of Lotus reads one JSON file, `/etc/eye-of-lotus/config.json` unless
`-config` says otherwise. Every key can also be set through an environment
variable (`EOL_<KEY>`), and a few of the common ones through a command line
flag. The order of precedence is:

```
defaults  <  configuration file  <  environment  <  command line flags
```

Print what the process actually resolved:

```bash
eye-of-lotus -print-config        # passwords and the session secret are hidden
```

## Reference

### Server

| Key | Type | Default | Meaning |
|---|---|---|---|
| `listen` | string | `127.0.0.1:9110` | Address to bind. Loopback by default: reach the panel through an SSH tunnel or a reverse proxy rather than publishing it. |
| `base_path` | string | `""` | URL prefix when served under a subpath, for example `/lotus`. Must start with a slash. |
| `password` | string | — | The single passphrase. A PBKDF2 hash is strongly preferred; a plain value also works but the panel warns about it at every start. Required unless `demo` is set. |
| `session_secret` | string | generated | Signs session cookies. Generated on first run and written back to the file so that a restart does not log everyone out. |
| `session_ttl` | duration | `12h` | How long a login lasts. |
| `trust_proxy` | bool | `false` | Believe `X-Forwarded-For`. Enable **only** when a reverse proxy you control sets it; otherwise a client can forge the header and bypass the login throttle. |
| `log_level` | string | `info` | `debug`, `info`, `warn` or `error`. |
| `demo` | bool | `false` | Serve a synthetic machine instead of this host. No password is required and nothing on the host is read. |

### History

| Key | Type | Default | Meaning |
|---|---|---|---|
| `database` | string | `/var/lib/eye-of-lotus/history.db` | SQLite file. An empty value disables history: the panel keeps only the last few minutes in memory. |
| `sample_interval` | duration | `2s` | How often the machine is sampled. |
| `retain_raw` | duration | `24h` | How long individual samples are kept. |
| `retain_minute` | duration | `7d` | How long one minute averages are kept. |
| `retain_hour` | duration | `90d` | How long one hour averages are kept. |

Durations are Go duration strings (`2s`, `90m`, `24h`) plus a day suffix
(`7d`, `90d`). A plain number is read as seconds.

Aggregation runs at most once a minute inside the sampler: raw rows are
folded into minute rows, minute rows into hour rows, and each level is
pruned once its retention window has passed. Nothing needs a cron job.

### What is collected

| Key | Type | Default | Meaning |
|---|---|---|---|
| `disks` | list of strings | `[]` | Mount points to report. Empty means every real filesystem; pseudo filesystems such as `proc`, `sysfs` and `cgroup2` are always skipped. |
| `ifaces` | list of strings | `[]` | Interfaces to report. Empty means every interface that has seen traffic; loopback is excluded unless named here. |
| `top_procs` | int | `12` | How many processes the snapshot carries, sorted by CPU then memory. The machine's total process count is always reported. |
| `probe_interval` | duration | `15s` | How often the internet and DNS probes run. They open real sockets, so they do not run on every sample. |
| `probe_targets` | list of strings | `1.1.1.1:53`, `8.8.8.8:53`, `9.9.9.9:53` | TCP endpoints tried in order. A TCP connect is used rather than ICMP because it needs no privileges and works behind NAT. |
| `dns_name` | string | `one.one.one.one` | Name resolved during the DNS check. |
| `proc_root` | string | `""` (the real `/`) | Read `/proc` and `/sys` from another root. Useful for inspecting a mounted disk image, and used by the test suite. |

### Services

| Key | Type | Default | Meaning |
|---|---|---|---|
| `systemd` | bool | `true` | Read unit state from systemd over D-Bus. |
| `systemd_bus` | string | `""` (the system bus) | Alternative D-Bus address. |
| `systemd_detail` | bool | `true` | Fetch per-unit properties (enabled state, restart counter). This is one D-Bus round trip per unit, cached for 60 seconds. Set it to `false` on a machine with hundreds of units if you only care about state. |
| `docker` | bool | `true` | Read containers from the Docker daemon. |
| `docker_socket` | string | `/var/run/docker.sock` | Alternative socket path. |

Both are optional and both degrade honestly: when systemd or Docker is not
reachable, the section says so and explains why, instead of showing an
empty list.

## Environment variables

| Variable | Key |
|---|---|
| `EOL_CONFIG` | path to the configuration file |
| `EOL_LISTEN` | `listen` |
| `EOL_BASE_PATH` | `base_path` |
| `EOL_PASSWORD` | `password` |
| `EOL_DATABASE` | `database` |
| `EOL_LOG_LEVEL` | `log_level` |
| `EOL_DNS_NAME` | `dns_name` |
| `EOL_PROC_ROOT` | `proc_root` |
| `EOL_DOCKER_SOCKET` | `docker_socket` |
| `EOL_SYSTEMD_BUS` | `systemd_bus` |
| `EOL_SAMPLE_INTERVAL` | `sample_interval` |
| `EOL_STORE_INTERVAL` | `store_interval` |
| `EOL_RETAIN_RAW` | `retain_raw` |
| `EOL_RETAIN_MINUTE` | `retain_minute` |
| `EOL_RETAIN_HOUR` | `retain_hour` |
| `EOL_SESSION_TTL` | `session_ttl` |
| `EOL_PROBE_INTERVAL` | `probe_interval` |
| `EOL_TOP_PROCS` | `top_procs` |
| `EOL_SYSTEMD` | `systemd` (truthy: `1`, `true`, `yes`, `on`) |
| `EOL_DOCKER` | `docker` |
| `EOL_DEMO` | `demo` |
| `EOL_TRUST_PROXY` | `trust_proxy` |

Environment variables are what the container image uses, because editing a
file inside a read-only container is not an option.

## Command line flags

```
-config PATH        configuration file to read
-listen ADDR        address to bind
-base-path PATH     URL prefix behind a reverse proxy
-db PATH            SQLite history file, empty to disable history
-no-history         disable history even if the configuration enables it
-password PASS      override the configured password
-demo               serve a synthetic machine
-proc-root PATH     read /proc and /sys from another root
-log-level LEVEL    debug, info, warn or error
-print-config       print the effective configuration and exit
-hash-password P    print a storable hash for P and exit
-healthcheck        probe a running panel over HTTP and exit
-version            print the version and exit
```

## Passwords

```bash
eye-of-lotus -hash-password 'correct horse battery staple'
```

```
Password hash for "correct horse battery staple":

  "password": "pbkdf2-sha256$210000$...$..."

Put it in /etc/eye-of-lotus/config.json.
```

The hash is PBKDF2-HMAC-SHA256 with 210,000 iterations and a 16 byte
random salt. The iteration count is stored with the hash, so it can be
raised later without invalidating existing passwords. The implementation
uses only the standard library, so the panel keeps its single dependency.

A plain password in the configuration file works, and is compared in
constant time, but the panel logs a warning about it at every start.

## Thresholds

The issue engine compares readings against a fixed table. The API
publishes it (`GET /api/meta`), and the interface explains every finding
with the number and the threshold behind it.

| Check | Warning | Critical |
|---|---|---|
| Filesystem used | 88% | 95% |
| Inodes used | 88% | 95% |
| RAM used | 90% | 96% |
| Swap used | 40% | 80% |
| Load average / cores | 1.5x | 3x |
| CPU iowait | 25% | 50% |
| Probe latency | 400 ms | — |
| DNS lookup | 500 ms | — |
| Temperature | 80 °C | 95 °C |

Some findings have no threshold: no internet, failing DNS, failed units,
unhealthy or restart-looping containers, a read-only filesystem, zombie
processes, CPU steal above 10%, a whole filesystem out of inodes, and
sensitive ports (2375, 6379, 27017, 9200, 11211, 5432, 3306, 23, 21)
listening on every interface.
