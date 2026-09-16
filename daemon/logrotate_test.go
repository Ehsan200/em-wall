package main

import (
	"bytes"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRotateDaemonLogTrimsToTail(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "em-wall.log")

	var lines []string
	for i := 0; i < 2000; i++ {
		lines = append(lines, strings.Repeat("x", 99))
	}
	body := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	logger := log.New(io.Discard, "", 0)
	rotateDaemonLogIfTooLarge(path, 100_000, 20_000, logger)

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) > 20_000 {
		t.Fatalf("file is %d bytes after rotation, want at most the 20000 kept", len(got))
	}
	if len(got) == 0 {
		t.Fatal("rotation emptied the file; the tail is the part worth keeping")
	}
	// The tail survived, the head didn't, and the first surviving line is a
	// whole line rather than the fragment the cut landed in.
	if !bytes.HasSuffix(got, []byte(strings.Repeat("x", 99)+"\n")) {
		t.Fatal("newest line did not survive")
	}
	for _, line := range strings.Split(strings.TrimSuffix(string(got), "\n"), "\n") {
		if len(line) != 99 {
			t.Fatalf("surviving line is %d bytes, want a whole 99-byte line", len(line))
		}
	}
}

func TestRotateDaemonLogLeavesSmallFileAlone(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "em-wall.log")
	body := "one\ntwo\nthree\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	rotateDaemonLogIfTooLarge(path, 100_000, 20_000, log.New(io.Discard, "", 0))

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Fatalf("file under the cap was modified: %q", got)
	}
}

func TestRotateDaemonLogIgnoresMissingFile(t *testing.T) {
	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)

	// The dev daemon logs to a terminal and has no file to trim. That is
	// the normal case there, not a condition worth a line in the log.
	rotateDaemonLogIfTooLarge(filepath.Join(t.TempDir(), "absent.log"), 100_000, 20_000, logger)
	rotateDaemonLogIfTooLarge("", 100_000, 20_000, logger)

	if buf.Len() != 0 {
		t.Fatalf("logged for a file that does not exist: %q", buf.String())
	}
}
