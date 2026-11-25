# magnet

[Русская версия](README.ru.md)

`magnet` finds and saves `.torrent` files from magnet links using the BitTorrent
DHT. It does not download torrent contents — it retrieves metadata only, meaning
the torrent file itself.

I occasionally had to process magnet links, sometimes in large batches. I used
to add them to the rtorrent client and then collect the resulting files. That
required a lot of manual work, so I built this utility to simplify the process.
Created with DeepSeek V4.1 Flash 1017 and the OpenCode agent.

The application is built as one binary and has two modes:

1. **One-shot mode** — `magnet fetch` looks up metadata, saves the file, and exits.
2. **Daemon mode** — `magnet daemon` accepts links through an HTTP API and
   processes them in the background. A CLI client is included (`magnet add`,
   `magnet ls`, `magnet wait`, `magnet get`, and others).

A separate lightweight `magnet-web` binary is also built. It provides a local
web interface and can either connect to a running daemon or start an embedded
engine itself — no Node.js, WebView, or CGO required.

## Features

- Metadata lookup through the DHT; trackers can be disabled (`--no-trackers`).
- Batch processing from arguments, files (`@links.txt`), and stdin (`-`).
- A daemon with a queue, multiple workers, and persistent state in bbolt.
- Automatic retries with configurable timeout and delay.
- Queue recovery after a daemon restart.
- HTTP/JSON API and `.torrent` downloads through the API.
- Local browser-based web UI: add links, list jobs, cancel, retry, delete, and
  download `.torrent` files.
- Optional API token. By default the daemon listens on `127.0.0.1` only.
- JSON output for scripting (`--json`).

## Releases

Ready-to-use archives for Linux, Windows, and macOS are published on the
[Releases](https://github.com/shurshur/magnet/releases) page. The first release
is `v0.1.0`.

Each archive contains both `magnet` and `magnet-web`, plus `LICENSE` and
`THIRD_PARTY_NOTICES.md`. Every release also includes `SHA256SUMS`:

```sh
curl -LO https://github.com/shurshur/magnet/releases/download/v0.1.0/magnet-v0.1.0-linux-amd64.tar.gz
curl -LO https://github.com/shurshur/magnet/releases/download/v0.1.0/SHA256SUMS
sha256sum -c SHA256SUMS --ignore-missing
tar -xzf magnet-v0.1.0-linux-amd64.tar.gz
sudo install -m 0755 magnet magnet-web /usr/local/bin/
```

Replace `linux-amd64` with `linux-arm64`, `darwin-amd64`, `darwin-arm64`,
`windows-amd64`, or `windows-arm64` as needed. Windows archives use `.zip`.
On macOS, verify checksums with
`shasum -a 256 -c SHA256SUMS --ignore-missing`.

Releases are built by GitHub Actions with GoReleaser. The version comes from
the git tag: `magnet --version` and `magnet-web --version` print it.

To publish a release:

1. Make sure CI is green on `master`.
2. Create and push a SemVer tag:

   ```sh
   git tag -a v0.1.0 -m "magnet v0.1.0"
   git push origin v0.1.0
   ```

3. The `Release` workflow builds Linux, Windows, and macOS archives for
   `amd64` and `arm64`, generates `SHA256SUMS`, and publishes a GitHub Release.

To verify the release build locally without publishing:

```sh
goreleaser release --snapshot --clean
```

Artifacts are written to `dist/`.

## Build

Go 1.23+ is required.

```sh
make build          # bin/magnet and bin/magnet-web
# or build them separately
go build -o magnet ./cmd/magnet
go build -o magnet-web ./cmd/magnet-web
```

Install into `/usr/local/bin`:

```sh
sudo make install
# or
sudo install -m 0755 bin/magnet bin/magnet-web /usr/local/bin/
```

Checks:

```sh
make vet
make test
make race
```

### Cross-compilation for Linux, Windows, and macOS

```sh
make build-all       # all platforms
make build-linux     # Linux amd64 + arm64
make build-windows   # Windows amd64 + arm64
make build-darwin    # macOS amd64 + arm64
```

Artifacts are written to `dist/` together with `SHA256SUMS`:

```text
dist/magnet-dev-linux-amd64
dist/magnet-web-dev-linux-amd64
dist/magnet-dev-linux-arm64
dist/magnet-web-dev-linux-arm64
dist/magnet-dev-windows-amd64.exe
dist/magnet-web-dev-windows-amd64.exe
dist/magnet-dev-windows-arm64.exe
dist/magnet-web-dev-windows-arm64.exe
dist/magnet-dev-darwin-amd64
dist/magnet-web-dev-darwin-amd64
dist/magnet-dev-darwin-arm64
dist/magnet-web-dev-darwin-arm64
dist/LICENSE
dist/THIRD_PARTY_NOTICES.md
dist/SHA256SUMS
```

The version defaults to `git describe`; it can be set explicitly:

```sh
make build-all VERSION=v1.0.0
```

Cross-compilation uses `CGO_ENABLED=0`, so no C compiler or platform
toolchains are required. In this mode the torrent client uses the pure-Go uTP
implementation; DHT, TCP, and metadata retrieval work fully.

`make build-all` produces flat binaries for local use. Release archives for
distribution are built separately by GoReleaser as described above.

## Mode 1: one-shot download

```sh
magnet fetch 'magnet:?xt=urn:btih:43519d14444904f6015e1a0d92018068fb0a49fa'
```

The path to the saved file is printed to stdout; progress and errors go to
stderr.

```sh
# Save into downloads/; the file name comes from the torrent metadata
magnet fetch -o downloads/ 'magnet:?xt=urn:btih:...'

# Explicit file name (the path ends with .torrent)
magnet fetch -o downloads/ubuntu.torrent 'magnet:?xt=urn:btih:...'

# Multiple links, a link file, and stdin
magnet fetch -o downloads/ \
  'magnet:?xt=urn:btih:...' \
  @links.txt \
  - < more-links.txt

# DHT only, no trackers
magnet fetch --no-trackers -o downloads/ 'magnet:?xt=urn:btih:...'

# Script-friendly output
magnet fetch --json -o downloads/ 'magnet:?xt=urn:btih:...'
```

`fetch` options:

| Option | Default | Description |
|---|---|---|
| `-o, --out` | `.` | Directory or `.torrent` path. A path without `.torrent` is treated as a directory. |
| `-t, --timeout` | `15m` | Per-link metadata search timeout. `0` = unlimited. |
| `--piece-layers-timeout` | `30s` | How long to wait for v2/hybrid piece layers. `0` = one check without waiting. |
| `--allow-incomplete-v2` | `false` | Save v2 without piece layers with `incomplete` status. |
| `--strict` | `false` | Treat `incomplete` as an error (exit code `1`). |
| `-f, --force` | `false` | Overwrite an existing `.torrent`. |
| `--port` | `0` | UDP/TCP port for DHT and peer connections; `0` = random. |
| `--no-trackers` | `false` | Do not use trackers; DHT only. |
| `--json` | `false` | Print results as a JSON array. |
| `-q, --quiet` | `false` | Do not print progress. |
| `--debug` | `false` | Verbose torrent client log. |

Exit codes: `0` = success, `1` = at least one link failed, `2` = invalid
arguments, `130` = interrupted by Ctrl+C.

## Mode 2: daemon and CLI client

### Starting the daemon

```sh
magnet daemon
```

Defaults:

- HTTP API: `127.0.0.1:8787`;
- state, job database, and torrent files: `~/.local/state/magnet`
  (or `$XDG_STATE_HOME/magnet`);
- 4 concurrent searches;
- one attempt lasts up to 15 minutes, then retries after 30 seconds; the number
  of attempts is unlimited.

Example with explicit options:

```sh
magnet daemon \
  --listen 127.0.0.1:8787 \
  --data-dir /var/lib/magnet \
  --workers 8 \
  --attempt-timeout 20m \
  --retry-delay 1m \
  --max-attempts 0
```

Daemon options:

| Option | Default | Description |
|---|---|---|
| `--listen` | `127.0.0.1:8787` | HTTP API listen address. |
| `--data-dir` | `$MAGNET_DATA_DIR` or `$XDG_STATE_HOME/magnet` | State, database, torrent files, and cache directory. |
| `--api-token` | `$MAGNET_API_TOKEN` | API token. If set, required for all `/v1/*` requests. |
| `--workers` | `4` | Number of concurrent searches. |
| `--attempt-timeout` | `15m` | Timeout for one attempt; `0` = unlimited. |
| `--retry-delay` | `30s` | Delay before retrying. |
| `--max-attempts` | `0` | Maximum attempts per link; `0` = unlimited. |
| `--piece-layers-timeout` | `30s` | How long to wait for v2/hybrid piece layers. `0` = one check. |
| `--allow-incomplete-v2` | `false` | Save v2 without piece layers with `incomplete` status. |
| `--port` | `0` | UDP/TCP port for DHT and peer connections. |
| `--no-trackers` | `false` | Search through the DHT only. |
| `--log-level` | `info` | `debug`, `info`, `warn`, `error`. |
| `--log-format` | `text` | `text` or `json`. |

The following are created inside `--data-dir`:

- `jobs.db` — bbolt job database;
- `torrents/<info-hash>.torrent` — retrieved torrent files;
- `cache/` — torrent client working directory.

### CLI client

Client commands use the same binary. The daemon address is set with
`--server`/`-s` or `MAGNET_SERVER`; the token with `--token` or
`MAGNET_API_TOKEN`. Options may appear before or after magnet links and job IDs.

```sh
# Queue links
magnet add 'magnet:?xt=urn:btih:...'
magnet add @links.txt
cat links.txt | magnet add -

# List jobs
magnet ls
magnet ls --state searching
magnet ls --all

# Job details
magnet show <id>

# Wait for completion and print the file path
magnet wait <id>
magnet wait --timeout 6h <id1> <id2>
magnet wait --strict <id>          # treat incomplete as an error

# Download completed torrent files (including incomplete ones)
magnet get -o downloads/ <id>
magnet get --strict -o downloads/ <id>

# Manage jobs
magnet cancel <id>
magnet retry <id>                  # also restarts incomplete jobs
magnet rm <id>                     # remove from the list, keep the file
magnet rm --purge <id>             # remove the job and the file

# Daemon status
magnet stats
magnet ping

# Restore piece layers in an existing v2/hybrid .torrent
magnet repair saved.torrent
magnet repair --timeout 2m -o fixed.torrent saved.torrent
```

The same commands are available explicitly:

```sh
magnet client add ...
magnet client ls
magnet client get ...
```

Most client commands support `--json` for machine processing. During bulk
import, `magnet add` handles invalid links individually and does not block the
rest; the exit code is `1` if at least one link was rejected.

### Job states

| State | Meaning |
|---|---|
| `queued` | Waiting for a worker or a retry delay. |
| `searching` | Metadata lookup is in progress. |
| `found` | `.torrent` was found and saved. |
| `incomplete` | Metadata was saved, but v2 piece layers are missing. The file can still be downloaded. |
| `failed` | All attempts were exhausted (`--max-attempts`). |
| `canceled` | The job was canceled by the user. |

On error or timeout, the job is returned to the queue; the reason is shown in
`error`. After a daemon restart, `searching` jobs are queued again and found jobs
are preserved. If a saved torrent file disappears, the job is automatically
queued for another lookup.

### v2, hybrid torrents, and piece layers

- v1 torrents are saved as before.
- A hybrid torrent is saved completely if its piece layers were retrieved;
  otherwise it is saved with `incomplete` status. Such a file remains usable by
  v1 clients and is available for download.
- A v2 torrent without piece layers is treated as an error by default and uses
  the normal retry policy. With `--allow-incomplete-v2`, it is saved as
  `incomplete` for analysis or for clients that can obtain layers from peers
  themselves.
- `magnet repair` loads an existing `.torrent`, requests missing piece layers
  from v2 peers, and writes the repaired file. Torrent contents are not
  downloaded.
- If the same hybrid torrent is added through separate v1 and v2 links, the jobs
  are merged after metadata is obtained. Both IDs remain aliases, and both links
  are preserved in history.
- When a link is added, the daemon also looks for an already saved `.torrent` by
  any known v1/v2 hash and reuses it.

## Web UI

`magnet-web` is a separate lightweight binary with a local web interface. It
does not require Node.js, WebView, CGO, or native GUI libraries: HTML/CSS/JS is
embedded in the executable.

```sh
./bin/magnet-web
```

By default the web UI:

1. checks whether a daemon is running at `http://127.0.0.1:8787`;
2. connects to it if found;
3. otherwise starts the embedded engine using the same data directory as
   `magnet daemon`;
4. starts the local web interface on a random loopback port and opens the
   browser.

The interface URL is printed to stdout. If the browser cannot be opened, open
the URL manually.

```sh
# Always use the embedded engine
./bin/magnet-web --embedded

# Connect to an existing daemon
./bin/magnet-web --server http://127.0.0.1:8787

# Daemon with a token
./bin/magnet-web --server http://127.0.0.1:8787 --api-token secret

# Do not open a browser; choose the port explicitly
./bin/magnet-web --no-browser --listen 127.0.0.1:9090

# Embedded engine with a separate data directory
./bin/magnet-web --embedded --data-dir ~/.local/state/magnet-web
```

Options:

| Option | Default | Description |
|---|---|---|
| `--server URL` | auto-detect | Connect to a daemon instead of the embedded engine. |
| `--api-token` | `$MAGNET_API_TOKEN` | Token for an external daemon. |
| `--embedded` | `false` | Always start the embedded engine. |
| `--listen` | `127.0.0.1:0` | Local web UI listen address. |
| `--data-dir` | `$MAGNET_DATA_DIR` / XDG | Data directory for the embedded engine. |
| `--workers` | `4` | Number of concurrent searches. |
| `--attempt-timeout` | `15m` | Timeout for one attempt; `0` = unlimited. |
| `--retry-delay` | `30s` | Delay before retrying. |
| `--max-attempts` | `0` | Maximum attempts; `0` = unlimited. |
| `--piece-layers-timeout` | `30s` | How long to wait for v2/hybrid piece layers. |
| `--allow-incomplete-v2` | `false` | Save v2 without piece layers with `incomplete` status. |
| `--port` | `0` | UDP/TCP port for DHT and peer connections. |
| `--no-trackers` | `false` | Search through the DHT only. |
| `--no-browser` | `false` | Do not open the browser automatically. |
| `--log-level` | `info` | `debug`, `info`, `warn`, `error`. |

The interface supports:

- adding one or more magnet links;
- filtering jobs by state;
- viewing the name, info hash, version, attempts, size, and error text;
- filtering incomplete jobs and seeing a missing-piece-layers warning;
- canceling searches, retrying jobs (including `incomplete`), and deleting jobs;
- downloading the resulting `.torrent`, including incomplete files;
- optionally deleting the saved torrent file together with the job.

The web UI is available in English and Russian. It selects the language
automatically from the browser settings and also provides a manual language
selector.

`.torrent` downloads go through the browser, so the browser chooses the save
location (usually the downloads folder). Use `magnet get` or the daemon API when
an exact path is required.

The embedded engine and the daemon use the same bbolt database, so they cannot
work concurrently with the same `--data-dir`. If the daemon runs on a
non-default port, pass it with `--server`.

The options `--data-dir`, `--workers`, `--attempt-timeout`, `--retry-delay`,
`--max-attempts`, `--piece-layers-timeout`, `--allow-incomplete-v2`, `--port`,
and `--no-trackers` apply only to the embedded engine; when connecting to a
daemon, its settings are used.

`magnet-web` has no authentication of its own and listens on `127.0.0.1` only by
default. For remote access, use an SSH tunnel or a reverse proxy with
authentication; binding to a non-loopback address produces a warning. Mutating
requests require a service header and are same-origin checked, which protects
against simple browser CSRF.

## HTTP API

Default base URL: `http://127.0.0.1:8787`.

When `--api-token` is set, pass it as:

```http
Authorization: Bearer <token>
```

or:

```http
X-API-Token: <token>
```

`GET /healthz` is always available without a token.

| Method and path | Description |
|---|---|
| `GET /healthz` | Liveness check. |
| `GET /v1/stats` | Job counters and uptime. |
| `POST /v1/jobs` | Add one or more links. |
| `GET /v1/jobs` | List jobs: `state` (`queued`, `searching`, `found`, `incomplete`, `failed`, `canceled`), `limit` (1..1000), `offset`. |
| `GET /v1/jobs/{id}` | One job. |
| `GET /v1/jobs/{id}/torrent` | Download the resulting `.torrent`. |
| `POST /v1/jobs/{id}/cancel` | Cancel a job. |
| `POST /v1/jobs/{id}/retry` | Retry a `failed`/`canceled` job. |
| `DELETE /v1/jobs/{id}` | Delete a job; `?purge=true` also deletes the file. |

### API examples

Add one link:

```sh
curl -sS -X POST http://127.0.0.1:8787/v1/jobs \
  -H 'Content-Type: application/json' \
  -d '{"magnet":"magnet:?xt=urn:btih:43519d14444904f6015e1a0d92018068fb0a49fa"}'
```

Add a batch:

```sh
curl -sS -X POST http://127.0.0.1:8787/v1/jobs \
  -H 'Content-Type: application/json' \
  -d '{"magnets":[
        "magnet:?xt=urn:btih:43519d14444904f6015e1a0d92018068fb0a49fa",
        "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567"
      ]}'
```

The batch is validated as a whole: if at least one link is invalid, `400` is
returned and no links are added. The `magnet add` CLI client validates links
locally and sends only valid ones to the API, so one typo does not block a bulk
import.

Add response:

```json
{
  "jobs": [
    {
      "id": "43519d14444904f6015e1a0d92018068fb0a49fa",
      "magnet": "magnet:?xt=urn:btih:...",
      "info_hash": "43519d14444904f6015e1a0d92018068fb0a49fa",
      "info_hash_v1": "43519d14444904f6015e1a0d92018068fb0a49fa",
      "info_hash_v2": "",
      "version": "v1",
      "state": "queued",
      "attempts": 0,
      "created_at": "2026-01-01T12:00:00Z",
      "updated_at": "2026-01-01T12:00:00Z"
    }
  ],
  "created": 1,
  "existing": 0
}
```

List:

```sh
curl -sS 'http://127.0.0.1:8787/v1/jobs?state=found&limit=50'
curl -sS 'http://127.0.0.1:8787/v1/jobs?state=incomplete&limit=50'
```

Download a torrent:

```sh
curl -sSJO http://127.0.0.1:8787/v1/jobs/<id>/torrent
```

API errors use a single format:

```json
{"error":{"code":"not_found","message":"job not found"}}
```

## Systemd

Example unit file `/etc/systemd/system/magnet.service`:

```ini
[Unit]
Description=magnet DHT metadata daemon
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=magnet
Group=magnet
StateDirectory=magnet
ExecStart=/usr/local/bin/magnet daemon \
    --listen 127.0.0.1:8787 \
    --data-dir /var/lib/magnet
Restart=on-failure
RestartSec=5s
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true

[Install]
WantedBy=multi-user.target
```

```sh
sudo useradd --system --home /var/lib/magnet --shell /usr/sbin/nologin magnet
sudo systemctl daemon-reload
sudo systemctl enable --now magnet
sudo -u magnet /usr/local/bin/magnet stats
```

If the daemon listens on a non-loopback address, always set `--api-token` or put
the token in `Environment=MAGNET_API_TOKEN=...` / `EnvironmentFile=`.

The API does not terminate TLS. For network access, use an SSH tunnel, VPN, or
an HTTPS reverse proxy; otherwise the token and magnet links are transmitted in
plain text.

## How it works

1. A magnet link is parsed and its v1 (`btih`) and/or v2 (`btmh`) info hashes
   are extracted.
2. The metadata fetcher adds the link to an anacrolix/torrent client.
3. The client discovers peers through the DHT (and trackers, unless disabled),
   performs an extended handshake, and obtains the info dictionary.
4. For v2/hybrid torrents the client additionally requests piece layers from v2
   peers. If necessary, a saved file can later be completed with `magnet repair`.
5. The info dictionary, announce list, and complete piece layers are encoded
   into a bencoded `.torrent`.
6. One-shot mode saves the file and exits. The daemon saves it to
   `torrents/<info-hash>.torrent`, updates the job in bbolt, and releases the
   worker.

The daemon does not request file pieces and does not participate in seeding, so
only torrent metadata is written to disk.

Limitations:

- v2-only torrents without piece layers are not saved by default; use
  `--allow-incomplete-v2` when an intermediate result is needed;
- `magnet repair` can restore only layers that are available from v2 peers;
  recomputing layers from already downloaded content can be done with the
  [torrent](https://github.com/shurshur/torrent) utility;
- DHT requires outbound UDP and preferably inbound UDP/TCP on the selected port;
- if a torrent has no available peers, metadata cannot be obtained — the job
  will be retried according to the configured policy until canceled;
- private torrents without DHT/trackers cannot be found.

## License

The project is distributed under the **MIT** license. See [LICENSE](LICENSE) for
the full text.

Third-party dependencies retain their own licenses. The list of modules,
versions, copyright notices, and license texts is in
[THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md). In particular,
`anacrolix/torrent`, `anacrolix/dht`, and several auxiliary `anacrolix` modules
are distributed under MPL-2.0; their source code was not modified, and source
links and versions are listed in the notices.
