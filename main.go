// pgpulse is a one-shot health check for PostgreSQL. It runs the checks I'd want to see
// first when paged, prints OK / WARN / CRIT per line, and exits with the worst status
// (0 OK, 1 WARN, 2 CRIT, 3 UNKNOWN), so it works by hand, in cron, or under Nagios.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
)

func main() {
	l := Limits{}
	flag.DurationVar(&l.LagWarn, "lag-warn", 30*time.Second, "replica replay lag WARN")
	flag.DurationVar(&l.LagCrit, "lag-crit", 5*time.Minute, "replica replay lag CRIT")
	slotWarn := flag.Float64("slot-warn-gb", 1, "WAL held by a slot WARN, in GB")
	slotCrit := flag.Float64("slot-crit-gb", 10, "WAL held by a slot CRIT, in GB")
	flag.DurationVar(&l.TxnWarn, "txn-warn", 5*time.Minute, "oldest open transaction WARN")
	flag.DurationVar(&l.TxnCrit, "txn-crit", 30*time.Minute, "oldest open transaction CRIT")
	flag.DurationVar(&l.IdleTxnWarn, "idle-txn-warn", time.Minute, "idle in transaction WARN")
	flag.DurationVar(&l.LockWarn, "lock-warn", 30*time.Second, "longest lock wait WARN")
	flag.DurationVar(&l.LockCrit, "lock-crit", 5*time.Minute, "longest lock wait CRIT")
	flag.Float64Var(&l.ConnWarn, "conn-warn", 80, "percent of max_connections WARN")
	flag.Float64Var(&l.ConnCrit, "conn-crit", 95, "percent of max_connections CRIT")
	flag.Float64Var(&l.CacheWarn, "cache-warn", 95, "cache hit percent WARN (at or below)")
	flag.Float64Var(&l.CacheCrit, "cache-crit", 90, "cache hit percent CRIT (at or below)")
	flag.Float64Var(&l.XidWarn, "xid-warn", 50, "percent to xid wraparound WARN")
	flag.Float64Var(&l.XidCrit, "xid-crit", 75, "percent to xid wraparound CRIT")
	flag.Float64Var(&l.DeadWarn, "dead-warn", 20, "dead row percent on the worst table WARN")
	flag.Float64Var(&l.DeadCrit, "dead-crit", 40, "dead row percent on the worst table CRIT")
	flag.StringVar(&l.BackupDir, "backup-dir", "", "folder of pg_basebackup backups to check the age of")
	flag.DurationVar(&l.BackupWarn, "backup-warn", 26*time.Hour, "newest backup age WARN")
	flag.DurationVar(&l.BackupCrit, "backup-crit", 50*time.Hour, "newest backup age CRIT")
	flag.IntVar(&l.ExpectReplicas, "expect-replicas", 0, "CRIT if a primary has fewer replicas connected")
	asJSON := flag.Bool("json", false, "print JSON instead of a table")
	timeout := flag.Duration("timeout", 10*time.Second, "time limit per server")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: pgpulse [flags] [connection string ...]\n\n"+
			"With no connection string it uses the usual PG* environment variables.\n"+
			"Give several to check a primary and its replicas in one go.\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	l.SlotWarn, l.SlotCrit = *slotWarn*(1<<30), *slotCrit*(1<<30)

	targets := flag.Args()
	if len(targets) == 0 {
		targets = []string{""}
	}

	var reports []Report
	worst := OK
	for _, dsn := range targets {
		rep := check1(dsn, l, *timeout)
		reports = append(reports, rep)
		if rep.Status != Skip && rep.Status > worst {
			worst = rep.Status
		}
	}

	if *asJSON {
		if err := WriteJSON(os.Stdout, reports); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(int(Unknown))
		}
	} else {
		WriteTable(os.Stdout, reports, isTerminal(os.Stdout))
	}
	os.Exit(worst.ExitCode())
}

func check1(dsn string, l Limits, timeout time.Duration) Report {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	db, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return Report{Target: describe(dsn, nil), Status: Crit,
			Results: []Result{{Check: "connect", Status: Crit, Value: "failed", Detail: err.Error()}}}
	}
	defer db.Close(context.Background())
	rs := RunAll(ctx, db, l)
	return Report{Target: describe(dsn, db), Status: Worst(rs), Results: rs}
}

// describe names a server without printing its password.
func describe(dsn string, db *pgx.Conn) string {
	if db != nil {
		c := db.Config()
		return fmt.Sprintf("%s@%s:%d/%s", c.User, c.Host, c.Port, c.Database)
	}
	if cfg, err := pgx.ParseConfig(dsn); err == nil {
		return fmt.Sprintf("%s@%s:%d/%s", cfg.User, cfg.Host, cfg.Port, cfg.Database)
	}
	return "unparseable connection string"
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
