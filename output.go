package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Report is everything pgpulse found on one server.
type Report struct {
	Target  string   `json:"target"`
	Status  Status   `json:"status"`
	Results []Result `json:"results"`
}

var colors = map[Status]string{OK: "32", Warn: "33", Crit: "31", Unknown: "35", Skip: "90"}

// WriteTable prints one aligned block per server.
func WriteTable(w io.Writer, reports []Report, color bool) {
	for i, rep := range reports {
		if i > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "%s  %s\n", paint(rep.Status, color), rep.Target)
		width := 0
		for _, r := range rep.Results {
			if len(r.Check) > width {
				width = len(r.Check)
			}
		}
		for _, r := range rep.Results {
			line := fmt.Sprintf("  %s  %-*s  %-14s %s", paint(r.Status, color), width, r.Check, r.Value, r.Detail)
			fmt.Fprintln(w, strings.TrimRight(line, " "))
		}
	}
}

func paint(s Status, color bool) string {
	label := fmt.Sprintf("%-4s", s)
	if !color {
		return label
	}
	return "\033[1;" + colors[s] + "m" + label + "\033[0m"
}

// WriteJSON prints one JSON document with every report, for scripts and dashboards.
func WriteJSON(w io.Writer, reports []Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(reports)
}
