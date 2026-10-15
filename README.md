<div align="center">

<img src="docs/lotus.svg" width="96" alt="Eye of Lotus">

# EYE OF LOTUS

**A read-only eye on a Linux server.**

Hardware, network, processes, systemd units, containers, and a verdict on
what is wrong — in one small binary with no dependencies and no way to
break anything.

[Русская версия](README.ru.md) · [Configuration](docs/configuration.md) · [API](docs/api.md)

</div>

---

## What it is

A single Go binary that watches one Linux machine and serves a web page
about it. It reads `/proc`, `/sys`, the systemd bus and the Docker socket.
It never writes anything except its own history database, and the HTTP API
has no endpoint that can change the state of the machine.

```
point it at a server, open the page, see what is wrong
```

## What it shows

| Section | Contents |
|---|---|
| **0 Verdict** | The worst finding right now, how many there are, uptime, collection time |
| **I Hardware** | CPU total and per core, load average, iowait, steal, temperature, RAM and swap, every real filesystem with usage, inodes and disk throughput |
| **II Network** | Internet reachability by TCP probe, DNS resolution and its latency, throughput, every interface with errors and drops, every listening socket with the process that owns it and whether it faces the network |
| **III Processes** | Process table sorted by CPU, with user, RSS, threads, state and age |
| **IV Services** | systemd units (running, failed, enabled, restarts) and Docker containers (state, health, restart loops) |
| **V Revelations** | Every finding with an explanation and a suggested fix, plus the stored history |

The issue engine watches for: filesystems and inodes filling up, memory
pressure, swap thrashing, load above the core count, disk wait, CPU steal
from a noisy hypervisor, lost internet, broken DNS, slow DNS, sensitive
ports open to the network, failed units, unhealthy or restart-looping
containers, high temperature, read-only filesystems and zombie processes.

Every threshold is published by the API, so the interface can explain a
verdict instead of asserting it.

## What it deliberately is not

- **Not an agent.** It monitors the machine it runs on. There is no
  central server, no fleet, no inbound connections from anywhere.
- **Not a control panel.** No restart, no kill, no edit. A monitoring tool
  with write access is a remote shell that happens to have nice fonts.
- **Not a Prometheus replacement.** No metrics endpoint to scrape, no
  alerting rules. It is for the person who wants one answer to "is
  anything wrong with this box".

## Design

The interface is graphite with flat one-pixel borders, one monospace
family, and no decoration that does not carry meaning:

- no gradients, no glow, no neon — every colour is a single flat value
- status is a small square: white for fine, violet for a warning, and a
  hard blink in crimson for something critical
- motion is limited to a linear width change on meters and the blink;
  `prefers-reduced-motion` turns the blink off
- an eye-and-lotus mark drawn as thin SVG lines, a five-petal divider, and
  Roman numerals on the sections

## Quick start

### Look at it without a server

```bash
go build -o eye-of-lotus ./cmd/eye-of-lotus
./eye-of-lotus -demo
# open http://127.0.0.1:9110
```

Demo mode serves a synthetic machine. No password is needed and nothing on
your host is read. After two minutes the synthetic machine starts filling a
filesystem — a warning appears a few minutes later and a critical finding
shortly after, so both levels of the status styling can be seen.

### Install on a server

```bash
# 1. Build (Go 1.26 or newer, no other toolchain needed)
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o eye-of-lotus ./cmd/eye-of-lotus

# 2. Create the service account and directories
sudo useradd --system --home /var/lib/eye-of-lotus --shell /usr/sbin/nologin eye-of-lotus
sudo install -d -o eye-of-lotus -g eye-of-lotus /var/lib/eye-of-lotus /etc/eye-of-lotus

# 3. Choose a password (this prints a hash, not the password)
./eye-of-lotus -hash-password 'a long passphrase'

# 4. Write the configuration
sudo install -o eye-of-lotus -g eye-of-lotus -m 0600 \
  deploy/config.example.json /etc/eye-of-lotus/config.json
sudoedit /etc/eye-of-lotus/config.json    # paste the hash, adjust the rest

# 5. Install and start
sudo install -m 0755 eye-of-lotus /usr/local/bin/eye-of-lotus
sudo install -m 0644 deploy/eye-of-lotus.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now eye-of-lotus
```

The panel listens on `127.0.0.1:9110` by default. Reach it through an SSH
tunnel (`ssh -L 9110:127.0.0.1:9110 server`) or a reverse proxy — both are
safer than exposing it, even with a password.

### Docker

```bash
cp .env.example .env      # set EOL_PASSWORD
docker compose up -d
```

A container sees hardware, network and processes for real (it shares the
kernel and, with `pid: host`, the process namespace). systemd state is not
available inside a container unless you deliberately mount the host's
D-Bus socket — see the comments in `docker-compose.yml`. For the complete
picture, install it natively.

## Configuration

Everything lives in one JSON file, `/etc/eye-of-lotus/config.json` by
default. Every key can be overridden by an environment variable
(`EOL_LISTEN`, `EOL_PASSWORD`, `EOL_DATABASE`, …) and by a command line
flag, in that order of increasing precedence.

```bash
./eye-of-lotus -print-config      # show the effective configuration
./eye-of-lotus -hash-password 'x' # produce a storable password hash
./eye-of-lotus -no-history        # run without a database
./eye-of-lotus -proc-root /mnt/image   # inspect a mounted system
```

The full reference is in [docs/configuration.md](docs/configuration.md).

Passwords are stored as PBKDF2-HMAC-SHA256 with 210,000 iterations. A
plain password in the file also works, but the panel complains about it in
the log at every start.

## API

Everything the interface does is a documented, read-only HTTP call, so the
data is scriptable:

```bash
curl -c jar -X POST -H 'Content-Type: application/json' \
     -d '{"password":"secret"}' http://127.0.0.1:9110/api/auth/login
curl -b jar http://127.0.0.1:9110/api/snapshot | jq '.verdict'
```

See [docs/api.md](docs/api.md) for the full list.

## How it is built

```
cmd/eye-of-lotus      entry point, sampling loop, graceful shutdown
internal/proc         /proc and /sys parsers, testable against fixtures
internal/collect      collectors, rate maths, issue engine, demo machine
internal/systemd      systemd over D-Bus, implemented in-repo
internal/dbus         the minimal D-Bus client that makes the above possible
internal/docker       Docker Engine API over the Unix socket, GET only
internal/store        SQLite history: raw, minute and hour resolutions
internal/web          HTTP, authentication, embedded single-page interface
web assets            HTML, CSS and JavaScript, embedded in the binary
```

Two decisions are worth explaining.

**Almost no dependencies.** SQLite is the only one, and it is pure Go, so
`CGO_ENABLED=0` produces a static binary. systemd is reached through a
D-Bus client written for this project rather than a library, and Docker
through a small HTTP client over the socket. A monitor should not be the
heaviest thing on the machine, and it should not break because a
dependency changed.

**No frontend build step.** The interface is plain HTML, CSS and
JavaScript, embedded with `embed`. There is no Node, no bundler and no
`node_modules`. Editing a stylesheet and rebuilding the binary is the
whole development loop.

## Development

```bash
go test ./...            # unit tests, including fixture-driven collector tests
go vet ./...
gofmt -l .
go run ./cmd/eye-of-lotus -demo -log-level debug
```

The collector tests build a fake `/proc` and `/sys` tree and assert that
the parsers and the issue engine behave: that a 93%-full memory reading
becomes a critical finding, that an exposed port 2375 is flagged, that a
process command line containing parentheses still parses. The systemd and
Docker collectors cannot be exercised without those daemons present; they
degrade into an explanatory finding instead of failing, which is what the
tests assert.

## License

MIT. See [LICENSE](LICENSE).
