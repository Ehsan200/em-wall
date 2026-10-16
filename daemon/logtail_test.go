package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ehsan/em-wall/core/ipc"
)

func TestTailFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "em-wall.log")
	var b strings.Builder
	for i := range 100 {
		fmt.Fprintf(&b, "line %03d %s\n", i, map[bool]string{true: "xray", false: "dns"}[i%10 == 0])
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	lines, _, truncated, err := tailFile(path, 1<<20, 3, "")
	if err != nil || truncated {
		t.Fatalf("err=%v truncated=%v", err, truncated)
	}
	if want := []string{"line 097 dns", "line 098 dns", "line 099 dns"}; !slices.Equal(lines, want) {
		t.Fatalf("got %q want %q", lines, want)
	}

	lines, _, _, _ = tailFile(path, 1<<20, 2, "XRAY")
	if want := []string{"line 080 xray", "line 090 xray"}; !slices.Equal(lines, want) {
		t.Fatalf("grep: got %q want %q", lines, want)
	}

	// A window that starts mid-line drops the fragment, never a half line.
	lines, _, truncated, _ = tailFile(path, 20, 10, "")
	if !truncated || len(lines) != 1 || lines[0] != "line 099 dns" {
		t.Fatalf("window: got %q truncated=%v", lines, truncated)
	}
}

func TestTailLogMissing(t *testing.T) {
	d := &handlerDeps{daemonLogPath: filepath.Join(t.TempDir(), "nope.log")}
	out, err := d.tailLog(ipcTailParams("daemon"))
	if err != nil || !out.Missing {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	d.xrayLogDir = ""
	if out, _ := d.tailLog(ipcTailParams("xray-error")); !out.Missing {
		t.Fatalf("unconfigured xray dir should be missing: %+v", out)
	}
	if _, err := d.tailLog(ipcTailParams("bogus")); err == nil {
		t.Fatal("unknown source should error")
	}
}

func ipcTailParams(source string) ipc.LogsTailParams { return ipc.LogsTailParams{Source: source} }
