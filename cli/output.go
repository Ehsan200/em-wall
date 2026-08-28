package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"text/tabwriter"
)

// emitJSON writes v as indented JSON. Used for every command when
// --json is set; the shapes are the DTOs from core/ipc verbatim, so
// scripts can rely on the same field names the daemon publishes.
func (a *app) emitJSON(v any) error {
	enc := json.NewEncoder(a.out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// table prints a column-aligned table. Column order is part of the CLI's
// contract — append new columns, don't reorder existing ones.
func (a *app) table(header []string, rows [][]string) {
	tw := tabwriter.NewWriter(a.out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, strings.Join(header, "\t"))
	for _, r := range rows {
		fmt.Fprintln(tw, strings.Join(r, "\t"))
	}
	tw.Flush()
}

// dash renders empty strings as "-" so blank cells stay visible in a table.
func dash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
