package main

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"log"
	"os"
)

// Daemon log cap.
//
// xray's access and error logs have been capped since they were added;
// the daemon's own log never was. It is written by launchd, which
// redirects our stdout and stderr to StandardOutPath and then leaves the
// file alone forever. On a developer machine here that file had reached
// 88 MB and a million lines with nothing to stop it, and the storm
// tcpHealth now brakes is exactly the thing that fills it fastest.
//
// We can't hand the file to a rotating writer: launchd owns the fd, and
// replacing the file underneath it would leave the daemon writing to an
// unlinked inode. So we do what the xray rotation does — truncate in
// place — except we keep the tail rather than dropping everything, since
// the newest lines are the ones anyone is reading when they go looking.
//
// The fd launchd holds is in append mode, so writes racing this rewrite
// land past the region we are copying and are lost when we truncate. That
// costs at most a few lines, once per cap crossing, and only in the log.
const (
	// daemonLogCapBytes is the size that triggers a rotation.
	daemonLogCapBytes = 64 << 20

	// daemonLogKeepBytes is how much of the tail survives one. Small
	// enough that the rewrite is a single cheap read, large enough to
	// still hold hours of context.
	daemonLogKeepBytes = 8 << 20
)

// rotateDaemonLogIfTooLarge trims path to its last keep bytes once it has
// grown past cap. A missing file is not an error — the dev daemon logs to
// a terminal and has no file to trim. Best-effort throughout: a failure
// here must never be able to take the daemon down.
func rotateDaemonLogIfTooLarge(path string, capBytes, keepBytes int64, logger *log.Logger) {
	if path == "" || capBytes <= 0 || keepBytes <= 0 || keepBytes >= capBytes {
		return
	}
	fi, err := os.Stat(path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			logger.Printf("em-walld: stat log %s: %v", path, err)
		}
		return
	}
	if fi.Size() <= capBytes {
		return
	}

	f, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		logger.Printf("em-walld: open log %s for rotation: %v", path, err)
		return
	}
	defer f.Close()

	tail := make([]byte, keepBytes)
	if _, err := f.ReadAt(tail, fi.Size()-keepBytes); err != nil && !errors.Is(err, io.EOF) {
		logger.Printf("em-walld: read log tail %s: %v", path, err)
		return
	}
	// Start at a line boundary so the first surviving line isn't a
	// fragment of whatever the cut landed in the middle of.
	if i := bytes.IndexByte(tail, '\n'); i >= 0 {
		tail = tail[i+1:]
	}

	n, err := f.WriteAt(tail, 0)
	if err != nil {
		logger.Printf("em-walld: rewrite log %s: %v", path, err)
		return
	}
	if err := f.Truncate(int64(n)); err != nil {
		logger.Printf("em-walld: truncate log %s: %v", path, err)
		return
	}
	logger.Printf("em-walld: log exceeded %d bytes — trimmed to last %d", capBytes, n)
}
