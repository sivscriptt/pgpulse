# pgpulse

A one-shot health check for PostgreSQL. Point it at a server, or a primary and its
replicas, and it prints the things I'd want to see first if I got paged. Each line is
OK, WARN or CRIT, and the exit code is the worst one (0 OK, 1 WARN, 2 CRIT, 3 UNKNOWN).
So it works by hand, from cron, or as a Nagios check.

Written in Go, one binary, no agent on the server.

## What it checks

| Check | WARN / CRIT by default |
|---|---|
| Replica lag, and whether every replica is streaming | 30 s / 5 min, CRIT if fewer than `--expect-replicas` |
| Replication slots, and how much WAL each one holds | 1 GB / 10 GB, WARN if a slot is inactive |
| WAL archiving | CRIT if the last attempt failed |
| Age of the newest base backup (`--backup-dir`) | 26 h / 50 h |
| Oldest open transaction | 5 min / 30 min |
| Sessions idle in a transaction | 1 min |
| Sessions waiting on a lock, and who is blocking them | 30 s / 5 min |
| Connections used | 80% / 95% |
| Cache hit ratio | 95% / 90% |
| Transaction ID wraparound | 50% / 75% |
| Dead rows on the worst table | 20% / 40% |
| Invalid indexes left by a failed CREATE INDEX CONCURRENTLY | any |

Every threshold has a flag. Run `pgpulse -h` for the list.

## Example

I tested it against the primary and replica from
[pg-ops-lab](https://github.com/sivscriptt/pg-ops-lab). First a healthy run, then I
broke things on purpose. I left a transaction open, queued an update behind it, created
a slot nobody uses, and set `archive_command` to `false`:

    $ pgpulse --expect-replicas 1 --txn-warn 10s --lock-warn 10s "host=... port=55432"
    CRIT  postgres@...:55432/bank
      OK    server                      primary        PostgreSQL 17.10, up 1m 18s
      OK    connections                 3%             3 of 100
      OK    replica node_b              9 ms           streaming, async, 0 B behind
      OK    slot node_b                 0 B held       active
      WARN  slot old_reporting_replica  48.0 MB held   INACTIVE: holding WAL until someone drops it
      CRIT  wal archiving               failing        last failure 12.2 s ago, 6 failures total
      WARN  oldest transaction          17.5 s         pid 49551: update core.accounts set ...
      WARN  idle in transaction         1              longest 17.5 s; holds locks and blocks vacuum
      WARN  blocked sessions            1              longest wait 16.4 s, root blocker pid 49551
      OK    cache hit                   99.0%
      OK    xid wraparound              0.0%
      OK    invalid indexes             0
    exit 2

The blocked sessions line names the root blocker, not just the session that is
waiting. That's the pid you'd actually go and talk to, or terminate.

Then I stopped the replica:

      CRIT  replicas                    0 of 1         fewer replicas connected than expected

## Usage

    go install github.com/sivscriptt/pgpulse@latest

    pgpulse                                   # uses PGHOST, PGUSER and so on
    pgpulse "host=db1 user=monitor" "host=db2 user=monitor"
    pgpulse --backup-dir /backups --expect-replicas 2 "host=db1"
    pgpulse --json "host=db1" | jq '.[0].status'

A monitoring user only needs the `pg_monitor` role:

    create role monitor login password '...' in role pg_monitor;

## Things I had to think about

- **Replica lag lies on a quiet primary.** "Time since the last replayed commit" keeps
  growing when nobody is writing, even though the replica is fully caught up. So on a
  standby, pgpulse only grades that delay when there's also WAL received but not yet
  replayed.
- **The database doesn't know when it was last backed up.** pg_basebackup leaves
  nothing in the catalog, so the backup check reads the `backup_manifest` timestamps in
  a folder you point it at.
- **One broken check shouldn't hide the rest.** If a query fails, that line becomes
  UNKNOWN and the other checks still run.
- **Cache hit is noisy after a restart.** A replica I had just restarted showed 93%,
  which is a WARN. That's true, the cache is cold, but it'll recover by itself.

## Tests

`go test ./...` runs the unit tests. With `PGPULSE_TEST_DSN` set, it also runs every
check against a real server and fails if any of them comes back UNKNOWN. CI does that
on Postgres 15, 16 and 17.
