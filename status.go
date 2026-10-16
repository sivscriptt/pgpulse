package main

import (
	"fmt"
	"time"
)

// Status follows the Nagios convention, so the exit code can feed any monitoring system.
type Status int

const (
	OK Status = iota
	Warn
	Crit
	Unknown
	Skip // check doesn't apply here (e.g. replica checks on a primary with no replicas)
)

func (s Status) String() string {
	switch s {
	case OK:
		return "OK"
	case Warn:
		return "WARN"
	case Crit:
		return "CRIT"
	case Skip:
		return "SKIP"
	}
	return "UNKNOWN"
}

func (s Status) MarshalText() ([]byte, error) { return []byte(s.String()), nil }

// ExitCode maps the worst status to 0/1/2/3. Skips count as OK.
func (s Status) ExitCode() int {
	if s == Skip {
		return 0
	}
	return int(s)
}

// Result is one line of output.
type Result struct {
	Check  string `json:"check"`
	Status Status `json:"status"`
	Value  string `json:"value"`
	Detail string `json:"detail,omitempty"`
}

// Worst returns the most serious status in rs, ignoring skips.
func Worst(rs []Result) Status {
	w := OK
	for _, r := range rs {
		if r.Status != Skip && r.Status > w {
			w = r.Status
		}
	}
	return w
}

// above grades a value where bigger is worse (lag, age, percent used).
func above(v, warn, crit float64) Status {
	switch {
	case v >= crit:
		return Crit
	case v >= warn:
		return Warn
	}
	return OK
}

// below grades a value where smaller is worse (cache hit ratio).
func below(v, warn, crit float64) Status {
	switch {
	case v <= crit:
		return Crit
	case v <= warn:
		return Warn
	}
	return OK
}

func bytesHuman(b float64) string {
	units := []string{"B", "kB", "MB", "GB", "TB"}
	i := 0
	for b >= 1024 && i < len(units)-1 {
		b /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%.0f %s", b, units[i])
	}
	return fmt.Sprintf("%.1f %s", b, units[i])
}

func durHuman(d time.Duration) string {
	switch {
	case d < time.Second:
		return fmt.Sprintf("%d ms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%.1f s", d.Seconds())
	case d < time.Hour:
		return fmt.Sprintf("%dm %02ds", int(d.Minutes()), int(d.Seconds())%60)
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh %02dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%.1f days", d.Hours()/24)
}
