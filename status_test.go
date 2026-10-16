package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestAboveBelow(t *testing.T) {
	cases := []struct {
		got, want Status
	}{
		{above(10, 30, 300), OK},
		{above(30, 30, 300), Warn}, // thresholds are inclusive
		{above(299, 30, 300), Warn},
		{above(300, 30, 300), Crit},
		{below(99, 95, 90), OK},
		{below(95, 95, 90), Warn},
		{below(80, 95, 90), Crit},
	}
	for i, c := range cases {
		if c.got != c.want {
			t.Errorf("case %d: got %s, want %s", i, c.got, c.want)
		}
	}
}

func TestWorstIgnoresSkip(t *testing.T) {
	rs := []Result{{Status: OK}, {Status: Skip}, {Status: Warn}}
	if w := Worst(rs); w != Warn {
		t.Fatalf("got %s, want WARN", w)
	}
	if w := Worst([]Result{{Status: Skip}}); w != OK {
		t.Fatalf("all skipped: got %s, want OK", w)
	}
}

func TestExitCodes(t *testing.T) {
	want := map[Status]int{OK: 0, Warn: 1, Crit: 2, Unknown: 3, Skip: 0}
	for s, code := range want {
		if s.ExitCode() != code {
			t.Errorf("%s: got %d, want %d", s, s.ExitCode(), code)
		}
	}
}

func TestHumanUnits(t *testing.T) {
	for in, want := range map[float64]string{0: "0 B", 1023: "1023 B", 1536: "1.5 kB", 5 << 30: "5.0 GB"} {
		if got := bytesHuman(in); got != want {
			t.Errorf("bytesHuman(%v) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[time.Duration]string{
		22 * time.Millisecond:         "22 ms",
		2500 * time.Millisecond:       "2.5 s",
		6*time.Minute + 5*time.Second: "6m 05s",
		26*time.Hour + 3*time.Minute:  "26h 03m",
		72 * time.Hour:                "3.0 days",
	} {
		if got := durHuman(in); got != want {
			t.Errorf("durHuman(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestTableHasNoColorWhenPiped(t *testing.T) {
	var b bytes.Buffer
	WriteTable(&b, []Report{{Target: "postgres@db:5432/bank", Status: Warn, Results: []Result{
		{Check: "server", Status: OK, Value: "primary"},
		{Check: "oldest transaction", Status: Warn, Value: "6m 05s", Detail: "pid 42: select 1"},
	}}}, false)
	out := b.String()
	if strings.Contains(out, "\033[") {
		t.Fatal("color codes in piped output")
	}
	for _, want := range []string{"WARN  postgres@db:5432/bank", "  WARN  oldest transaction  6m 05s", "pid 42"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestJSONUsesStatusNames(t *testing.T) {
	var b bytes.Buffer
	if err := WriteJSON(&b, []Report{{Target: "x", Status: Crit, Results: []Result{{Check: "c", Status: Crit, Value: "v"}}}}); err != nil {
		t.Fatal(err)
	}
	var back []map[string]any
	if err := json.Unmarshal(b.Bytes(), &back); err != nil {
		t.Fatal(err)
	}
	if back[0]["status"] != "CRIT" {
		t.Fatalf("status = %v, want CRIT", back[0]["status"])
	}
}
