package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Limits holds every threshold. Defaults are in main.go and each one has a flag.
type Limits struct {
	LagWarn, LagCrit       time.Duration // replica replay lag
	SlotWarn, SlotCrit     float64       // bytes of WAL held by a slot
	TxnWarn, TxnCrit       time.Duration // oldest open transaction
	IdleTxnWarn            time.Duration // idle in transaction
	LockWarn, LockCrit     time.Duration // longest wait on a lock
	ConnWarn, ConnCrit     float64       // percent of max_connections
	CacheWarn, CacheCrit   float64       // cache hit percent
	XidWarn, XidCrit       float64       // percent of the way to wraparound
	DeadWarn, DeadCrit     float64       // dead tuple percent on the worst table
	BackupWarn, BackupCrit time.Duration // age of newest base backup
	BackupDir              string
	ExpectReplicas         int
}

type check func(ctx context.Context, db *pgx.Conn, l Limits) []Result

// Order is the order of the report.
var checks = []check{
	checkServer, checkConnections, checkReplication, checkSlots, checkArchiver, checkBackup,
	checkTransactions, checkLocks, checkCache, checkWraparound, checkDeadTuples, checkInvalidIndexes,
}

// RunAll runs every check. A check that errors becomes an UNKNOWN line instead of
// stopping the report, because a broken check shouldn't hide the others.
func RunAll(ctx context.Context, db *pgx.Conn, l Limits) []Result {
	var out []Result
	for _, c := range checks {
		out = append(out, c(ctx, db, l)...)
	}
	return out
}

func failed(name string, err error) []Result {
	return []Result{{Check: name, Status: Unknown, Value: "error", Detail: err.Error()}}
}

func inRecovery(ctx context.Context, db *pgx.Conn) (bool, error) {
	var r bool
	err := db.QueryRow(ctx, "select pg_is_in_recovery()").Scan(&r)
	return r, err
}

func checkServer(ctx context.Context, db *pgx.Conn, _ Limits) []Result {
	var ver string
	var up time.Duration
	var standby bool
	err := db.QueryRow(ctx, `select current_setting('server_version'),
		now() - pg_postmaster_start_time(), pg_is_in_recovery()`).Scan(&ver, &up, &standby)
	if err != nil {
		return failed("server", err)
	}
	role := "primary"
	if standby {
		role = "standby"
	}
	return []Result{{Check: "server", Status: OK, Value: role, Detail: fmt.Sprintf("PostgreSQL %s, up %s", ver, durHuman(up))}}
}

func checkConnections(ctx context.Context, db *pgx.Conn, l Limits) []Result {
	var used, max int
	err := db.QueryRow(ctx, `select (select count(*) from pg_stat_activity where backend_type = 'client backend'),
		current_setting('max_connections')::int`).Scan(&used, &max)
	if err != nil {
		return failed("connections", err)
	}
	pct := 100 * float64(used) / float64(max)
	return []Result{{Check: "connections", Status: above(pct, l.ConnWarn, l.ConnCrit),
		Value: fmt.Sprintf("%.0f%%", pct), Detail: fmt.Sprintf("%d of %d", used, max)}}
}

func checkReplication(ctx context.Context, db *pgx.Conn, l Limits) []Result {
	standby, err := inRecovery(ctx, db)
	if err != nil {
		return failed("replication", err)
	}
	if standby {
		return standbyLag(ctx, db, l)
	}

	rows, err := db.Query(ctx, `select coalesce(application_name, client_addr::text, 'unnamed'), state, sync_state,
		coalesce(extract(epoch from replay_lag), 0), coalesce(pg_wal_lsn_diff(pg_current_wal_lsn(), replay_lsn), 0)
		from pg_stat_replication order by 1`)
	if err != nil {
		return failed("replication", err)
	}
	defer rows.Close()
	var out []Result
	for rows.Next() {
		var name, state, sync string
		var lagSecs, behind float64
		if err := rows.Scan(&name, &state, &sync, &lagSecs, &behind); err != nil {
			return failed("replication", err)
		}
		lag := time.Duration(lagSecs * float64(time.Second))
		s := above(lagSecs, l.LagWarn.Seconds(), l.LagCrit.Seconds())
		if state != "streaming" {
			s = Crit
		}
		out = append(out, Result{Check: "replica " + name, Status: s, Value: durHuman(lag),
			Detail: fmt.Sprintf("%s, %s, %s behind", state, sync, bytesHuman(behind))})
	}
	if err := rows.Err(); err != nil {
		return failed("replication", err)
	}
	if len(out) < l.ExpectReplicas {
		out = append(out, Result{Check: "replicas", Status: Crit, Value: fmt.Sprintf("%d of %d", len(out), l.ExpectReplicas),
			Detail: "fewer replicas connected than expected"})
	} else if len(out) == 0 {
		out = append(out, Result{Check: "replicas", Status: Skip, Value: "none", Detail: "no replicas connected"})
	}
	return out
}

// standbyLag measures how far a standby is behind. Replay delay alone lies on a quiet
// primary (no commits means the last replayed commit gets old), so it is only graded
// when there is also WAL received but not yet replayed.
func standbyLag(ctx context.Context, db *pgx.Conn, l Limits) []Result {
	var receiving bool
	var behind float64
	var delay *time.Duration
	err := db.QueryRow(ctx, `select exists (select 1 from pg_stat_wal_receiver where status = 'streaming'),
		greatest(coalesce(pg_wal_lsn_diff(pg_last_wal_receive_lsn(), pg_last_wal_replay_lsn()), 0), 0),
		now() - pg_last_xact_replay_timestamp()`).Scan(&receiving, &behind, &delay)
	if err != nil {
		return failed("replication", err)
	}
	if !receiving {
		return []Result{{Check: "wal receiver", Status: Crit, Value: "not streaming", Detail: "standby has lost its primary"}}
	}
	r := Result{Check: "replay", Status: OK, Value: bytesHuman(behind) + " behind"}
	if delay != nil {
		r.Detail = "last replayed commit " + durHuman(*delay) + " ago"
		if behind > 0 {
			r.Status = above(delay.Seconds(), l.LagWarn.Seconds(), l.LagCrit.Seconds())
		}
	}
	return []Result{{Check: "wal receiver", Status: OK, Value: "streaming"}, r}
}

func checkSlots(ctx context.Context, db *pgx.Conn, l Limits) []Result {
	rows, err := db.Query(ctx, `select slot_name, active,
		coalesce(pg_wal_lsn_diff(case when pg_is_in_recovery() then pg_last_wal_receive_lsn() else pg_current_wal_lsn() end, restart_lsn), 0)
		from pg_replication_slots order by 1`)
	if err != nil {
		return failed("slots", err)
	}
	defer rows.Close()
	var out []Result
	for rows.Next() {
		var name string
		var active bool
		var held float64
		if err := rows.Scan(&name, &active, &held); err != nil {
			return failed("slots", err)
		}
		s := above(held, l.SlotWarn, l.SlotCrit)
		detail := "active"
		if !active {
			detail = "INACTIVE: holding WAL until someone drops it"
			if s == OK {
				s = Warn
			}
		}
		out = append(out, Result{Check: "slot " + name, Status: s, Value: bytesHuman(held) + " held", Detail: detail})
	}
	if err := rows.Err(); err != nil {
		return failed("slots", err)
	}
	if len(out) == 0 {
		out = append(out, Result{Check: "slots", Status: Skip, Value: "none"})
	}
	return out
}

func checkArchiver(ctx context.Context, db *pgx.Conn, _ Limits) []Result {
	var mode string
	var archived, failedCount int64
	var lastOK, lastFail *time.Time
	var lastWal *string
	err := db.QueryRow(ctx, `select current_setting('archive_mode'), archived_count, failed_count,
		last_archived_time, last_failed_time, last_archived_wal from pg_stat_archiver`).
		Scan(&mode, &archived, &failedCount, &lastOK, &lastFail, &lastWal)
	if err != nil {
		return failed("archiver", err)
	}
	if mode == "off" {
		return []Result{{Check: "wal archiving", Status: Warn, Value: "off", Detail: "no point in time recovery possible"}}
	}
	if standby, err := inRecovery(ctx, db); err == nil && standby && mode != "always" {
		return []Result{{Check: "wal archiving", Status: Skip, Value: "-", Detail: "the primary archives (archive_mode = on)"}}
	}
	if lastFail != nil && (lastOK == nil || lastFail.After(*lastOK)) {
		return []Result{{Check: "wal archiving", Status: Crit, Value: "failing",
			Detail: fmt.Sprintf("last failure %s ago, %d failures total; WAL is piling up in pg_wal", durHuman(time.Since(*lastFail)), failedCount)}}
	}
	detail := "nothing archived yet"
	if lastOK != nil && lastWal != nil {
		detail = fmt.Sprintf("last %s, %s ago", *lastWal, durHuman(time.Since(*lastOK)))
	}
	return []Result{{Check: "wal archiving", Status: OK, Value: fmt.Sprintf("%d archived", archived), Detail: detail}}
}

// checkBackup looks for the newest pg_basebackup in --backup-dir (any folder with a
// backup_manifest). The database itself doesn't know when it was last backed up.
func checkBackup(_ context.Context, _ *pgx.Conn, l Limits) []Result {
	if l.BackupDir == "" {
		return []Result{{Check: "last backup", Status: Skip, Value: "-", Detail: "pass --backup-dir to check"}}
	}
	manifests, err := filepath.Glob(filepath.Join(l.BackupDir, "*", "backup_manifest"))
	if err != nil {
		return failed("last backup", err)
	}
	var newest time.Time
	var name string
	for _, m := range manifests {
		if fi, err := os.Stat(m); err == nil && fi.ModTime().After(newest) {
			newest, name = fi.ModTime(), filepath.Base(filepath.Dir(m))
		}
	}
	if name == "" {
		return []Result{{Check: "last backup", Status: Crit, Value: "none", Detail: "no backup_manifest under " + l.BackupDir}}
	}
	age := time.Since(newest)
	return []Result{{Check: "last backup", Status: above(age.Seconds(), l.BackupWarn.Seconds(), l.BackupCrit.Seconds()),
		Value: durHuman(age) + " ago", Detail: name}}
}

func checkTransactions(ctx context.Context, db *pgx.Conn, l Limits) []Result {
	var oldest, idle *time.Duration
	var oldestPid, idleCount int
	var oldestQuery string
	err := db.QueryRow(ctx, `
		with x as (
		  select pid, state, now() - xact_start as age, query from pg_stat_activity
		  where xact_start is not null and backend_type = 'client backend' and pid <> pg_backend_pid())
		select (select age from x order by age desc limit 1),
		       coalesce((select pid from x order by age desc limit 1), 0),
		       coalesce((select left(regexp_replace(query, '\s+', ' ', 'g'), 60) from x order by age desc limit 1), ''),
		       (select max(age) from x where state like 'idle in transaction%'),
		       (select count(*) from x where state like 'idle in transaction%')`).
		Scan(&oldest, &oldestPid, &oldestQuery, &idle, &idleCount)
	if err != nil {
		return failed("transactions", err)
	}
	out := []Result{{Check: "oldest transaction", Status: OK, Value: "none open"}}
	if oldest != nil {
		out[0] = Result{Check: "oldest transaction", Status: above(oldest.Seconds(), l.TxnWarn.Seconds(), l.TxnCrit.Seconds()),
			Value: durHuman(*oldest), Detail: fmt.Sprintf("pid %d: %s", oldestPid, oldestQuery)}
	}
	r := Result{Check: "idle in transaction", Status: OK, Value: "0"}
	if idleCount > 0 && idle != nil {
		r = Result{Check: "idle in transaction", Status: above(idle.Seconds(), l.IdleTxnWarn.Seconds(), l.TxnCrit.Seconds()),
			Value: fmt.Sprint(idleCount), Detail: "longest " + durHuman(*idle) + "; holds locks and blocks vacuum"}
	}
	return append(out, r)
}

func checkLocks(ctx context.Context, db *pgx.Conn, l Limits) []Result {
	var waiting int
	var longest *time.Duration
	var blockers []int32
	err := db.QueryRow(ctx, `
		with w as (
		  select pid, now() - coalesce(state_change, query_start) as waited, pg_blocking_pids(pid) as by
		  from pg_stat_activity where cardinality(pg_blocking_pids(pid)) > 0)
		select count(*), max(waited),
		       coalesce((select array_agg(distinct b) from w, unnest(w.by) b
		                 where b not in (select pid from w)), '{}')
		from w`).Scan(&waiting, &longest, &blockers)
	if err != nil {
		return failed("locks", err)
	}
	if waiting == 0 {
		return []Result{{Check: "blocked sessions", Status: OK, Value: "0"}}
	}
	return []Result{{Check: "blocked sessions", Status: above(longest.Seconds(), l.LockWarn.Seconds(), l.LockCrit.Seconds()),
		Value: fmt.Sprint(waiting), Detail: fmt.Sprintf("longest wait %s, root blocker pid %s", durHuman(*longest), joinInts(blockers))}}
}

func joinInts(xs []int32) string {
	s := make([]string, len(xs))
	for i, x := range xs {
		s[i] = fmt.Sprint(x)
	}
	return strings.Join(s, ", ")
}

func checkCache(ctx context.Context, db *pgx.Conn, l Limits) []Result {
	var hit, read float64
	err := db.QueryRow(ctx, `select coalesce(sum(blks_hit), 0), coalesce(sum(blks_read), 0) from pg_stat_database`).Scan(&hit, &read)
	if err != nil {
		return failed("cache hit", err)
	}
	if hit+read < 10000 {
		return []Result{{Check: "cache hit", Status: Skip, Value: "-", Detail: "not enough traffic since stats reset"}}
	}
	pct := 100 * hit / (hit + read)
	return []Result{{Check: "cache hit", Status: below(pct, l.CacheWarn, l.CacheCrit), Value: fmt.Sprintf("%.1f%%", pct),
		Detail: "all databases, since stats reset"}}
}

// checkWraparound: transaction ids run out at about 2 billion. Autovacuum freezes old rows
// to stop that, and when it can't keep up the server eventually refuses writes.
func checkWraparound(ctx context.Context, db *pgx.Conn, l Limits) []Result {
	var name string
	var age float64
	err := db.QueryRow(ctx, `select datname, age(datfrozenxid) from pg_database order by 2 desc limit 1`).Scan(&name, &age)
	if err != nil {
		return failed("xid wraparound", err)
	}
	pct := 100 * age / 2_000_000_000
	return []Result{{Check: "xid wraparound", Status: above(pct, l.XidWarn, l.XidCrit), Value: fmt.Sprintf("%.1f%%", pct),
		Detail: fmt.Sprintf("oldest is %s at %.0f xids", name, age)}}
}

// checkDeadTuples is a cheap bloat signal for the current database: the table with the
// highest share of dead rows, among tables big enough to matter.
func checkDeadTuples(ctx context.Context, db *pgx.Conn, l Limits) []Result {
	var tbl string
	var pct float64
	var lastVac *time.Duration
	err := db.QueryRow(ctx, `select schemaname || '.' || relname, 100.0 * n_dead_tup / (n_live_tup + n_dead_tup),
		now() - greatest(last_vacuum, last_autovacuum)
		from pg_stat_user_tables where n_live_tup + n_dead_tup > 10000
		order by 2 desc limit 1`).Scan(&tbl, &pct, &lastVac)
	if err == pgx.ErrNoRows {
		return []Result{{Check: "dead rows", Status: Skip, Value: "-", Detail: "no table over 10,000 rows"}}
	}
	if err != nil {
		return failed("dead rows", err)
	}
	vac := "never vacuumed"
	if lastVac != nil {
		vac = "vacuumed " + durHuman(*lastVac) + " ago"
	}
	return []Result{{Check: "dead rows", Status: above(pct, l.DeadWarn, l.DeadCrit), Value: fmt.Sprintf("%.1f%%", pct),
		Detail: fmt.Sprintf("worst is %s, %s", tbl, vac)}}
}

// checkInvalidIndexes catches a CREATE INDEX CONCURRENTLY that failed halfway. The index
// still slows down every write but is never used for reads.
func checkInvalidIndexes(ctx context.Context, db *pgx.Conn, _ Limits) []Result {
	var n int
	var names string
	err := db.QueryRow(ctx, `select count(*), coalesce(string_agg(indexrelid::regclass::text, ', '), '')
		from pg_index where not indisvalid`).Scan(&n, &names)
	if err != nil {
		return failed("invalid indexes", err)
	}
	if n == 0 {
		return []Result{{Check: "invalid indexes", Status: OK, Value: "0"}}
	}
	return []Result{{Check: "invalid indexes", Status: Warn, Value: fmt.Sprint(n), Detail: names + " (drop and rebuild)"}}
}
