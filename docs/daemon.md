# Mounting through a daemon, with logging (Linux)

On a managed machine, a FUSE mount doesn't hold store credentials of its
own. It names a **drive** and gets everything else from a server, through
a local daemon. Reads, writes, deletes and listings on the mount are
logged to that server.

```mermaid
flowchart LR
  M1["icbfs mount<br/>-log-socket -drive team"] -- Unix socket --> D["icbfs-logd"]
  C["icbfs-cli -daemon"] -- Unix socket --> D
  D -- one websocket,<br/>shared secret --> S["server<br/>(icbfs-mockserver, or a host application)"]
  S --> L[("event log")]
  S --> B[("bucket")]
  M1 --> B
```

- **The daemon** (`icbfs-logd`) holds the machine's one websocket to the
  server, and relays requests both ways. A request that is still
  unanswered when the connection drops is sent again after it reconnects.
- **A mount** asks the server for the drive. The server answers with the
  store's credentials, the filesystem and directory the drive is, and
  whether this machine may only read it. The mount then mounts that
  directory as its root, read-only if the server says so. A drive the
  server doesn't know can't be mounted.
- **icbfs-cli** with `-daemon` sends its command through the daemon
  instead of using credentials. The daemon sets who is asking from the
  socket's peer credentials (`host/uid:N` on Linux), whatever the client
  claims. The server records who asked, refuses a command it doesn't
  allow (`-allow-commands`), and runs it; the mock server checks nothing
  per account.

## Running it

```
icbfs-mockserver -secret S -log events.jsonl \
    -drives docs=docs,team=docs:/team:ro \
    -endpoint http://minio:9000 -bucket icbfs -access-key … -secret-key …
icbfs-logd -socket /run/icbfs-logd.sock -server ws://server:8090/ws/daemon -secret S
icbfs mount -log-socket /run/icbfs-logd.sock -drive team /mnt/team
icbfs-cli names create -daemon /run/icbfs-logd.sock -drive team -name before-cleanup
```

`icbfs-mockserver` is a stand-in for a real server: one shared secret, one
set of store credentials for every drive, and a JSON-lines log. It takes
the store the way the mounts do (`-store`, and the S3 or `-azure-*` flags;
see [mount.md](mount.md)), and hands it to mounts as a store
configuration: `{"kind": "s3", "s3": {...}}` or `{"kind": "azure",
"azure": {...}}`.

| flag | meaning |
|---|---|
| `-drives` | the registry, comma-separated: `id=filesystem[:/root][:ro]` |
| `-secret` | the secret daemons must present |
| `-log` | the JSON-lines file events are appended to (`icbfs-events.jsonl`) |
| `-allow-commands` | which side commands daemons may run, comma-separated with no spaces (`"names create,trash list"`); empty allows all |
| `-store-endpoint` | the store's endpoint (S3) or service URL (Azure) as the server reaches it, if that differs from what mounts use |
| `-addr` | listen address (`:8090`) |

`icbfs-logd` takes:

| flag | meaning |
|---|---|
| `-socket` | the Unix socket mounts and `icbfs-cli` connect to (`/run/icbfs-logd.sock`) |
| `-server` | the server's websocket URL (`ws://127.0.0.1:8090/ws/daemon`) |
| `-secret` | the secret the server expects |

Both read the secret from `ICBFS_LOGD_SECRET` if `-secret` isn't given.

A drive marked `:ro` is mounted read-only, and `trash restore` is refused
on it; `names create` and `names delete` still work. The `fs` commands
act on any filesystem in the bucket, not only the registry's drives, so
`-allow-commands` is what keeps `fs prune` from a daemon.

## What is logged

| op | when |
|---|---|
| `read` | a file is opened for reading |
| `write` | a file that was written is closed, with its size |
| `delete` | an unlink or rmdir |
| `ls` | a directory is listed |
| `command` | a side command ran, with who ran it, and the error if it failed |
| `dropped` | with `-log-async`, how many events were dropped while the daemon was unreachable |

Each event carries an ID, the time, the host, the drive, the filesystem,
the path (from the drive's root), and the caller's uid, gid and pid:

```json
{"id":"924f…","time":"2026-10-14T08:21:11.637Z","host":"ubuntu","drive":"team","fs":"docs",
 "op":"write","path":"/d/a","bytes":6,"uid":1000,"gid":1000,"pid":2503387}
```

**Waiting for the log.** By default, each logged operation waits until the
server has written its event to disk. With `-log-async`, events are queued
instead: an operation never waits, and events are dropped (and counted)
while the daemon is unreachable. The server ignores an event ID it has
already logged, so a resend after a reconnect is not logged twice.

The protocol is JSON frames (`internal/fslog`): one per line on the Unix
socket, and one per message on the websocket. Every request has an ID,
which its reply echoes.

Windows mounts have no daemon.
