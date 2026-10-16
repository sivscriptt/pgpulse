package main

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// TestChecksAgainstServer runs every check against a real server when PGPULSE_TEST_DSN is
// set (CI starts one). No check may come back UNKNOWN, which would mean its SQL is broken.
func TestChecksAgainstServer(t *testing.T) {
	dsn := os.Getenv("PGPULSE_TEST_DSN")
	if dsn == "" {
		t.Skip("PGPULSE_TEST_DSN not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(ctx)

	l := Limits{LagWarn: time.Minute, LagCrit: time.Hour, SlotWarn: 1 << 30, SlotCrit: 10 << 30,
		TxnWarn: time.Minute, TxnCrit: time.Hour, IdleTxnWarn: time.Minute, LockWarn: time.Minute,
		LockCrit: time.Hour, ConnWarn: 80, ConnCrit: 95, CacheWarn: 0, CacheCrit: 0,
		XidWarn: 50, XidCrit: 75, DeadWarn: 20, DeadCrit: 40}
	rs := RunAll(ctx, db, l)
	if len(rs) < len(checks) {
		t.Fatalf("got %d results for %d checks", len(rs), len(checks))
	}
	for _, r := range rs {
		if r.Status == Unknown {
			t.Errorf("%s: %s", r.Check, r.Detail)
		}
	}
}
