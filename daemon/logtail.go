package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/ehsan/em-wall/core/ipc"
)

// Text-log tail for the UI's Logs tab.
//
// The daemon log and xray's logs are plain files under /usr/local/var/log,
// tens of MB each, so a tail never reads the whole file: it reads a bounded
// window from the end and returns its last lines. With a search term the
// window is wider, since matches are sparser than lines.
const (
	logTailWindowBytes     = 512 << 10
	logTailGrepWindowBytes = 8 << 20
	logTailDefaultLines    = 500
	logTailMaxLines        = 5000
)

// logSourcePath maps a tail source to its file. "" means the source isn't
// configured on this daemon (e.g. -log-file "" or -xray-log-dir "").
func (d *handlerDeps) logSourcePath(source string) (string, error) {
	switch source {
	case ipc.LogSourceDaemon, "":
		return d.daemonLogPath, nil
	case ipc.LogSourceXrayError:
		if d.xrayLogDir == "" {
			return "", nil
		}
		return filepath.Join(d.xrayLogDir, "em-wall-xray-error.log"), nil
	case ipc.LogSourceXrayAccess:
		if d.xrayLogDir == "" {
			return "", nil
		}
		return filepath.Join(d.xrayLogDir, "em-wall-xray-access.log"), nil
	}
	return "", fmt.Errorf("unknown log source %q", source)
}

func (d *handlerDeps) tailLog(p ipc.LogsTailParams) (ipc.LogsTailDTO, error) {
	path, err := d.logSourcePath(p.Source)
	if err != nil {
		return ipc.LogsTailDTO{}, err
	}
	out := ipc.LogsTailDTO{Source: p.Source, Path: path, Lines: []string{}}
	if path == "" {
		out.Missing = true
		return out, nil
	}
	window := int64(logTailWindowBytes)
	if strings.TrimSpace(p.Grep) != "" {
		window = logTailGrepWindowBytes
	}
	lines, size, truncated, err := tailFile(path, window, p.Lines, p.Grep)
	if errors.Is(err, fs.ErrNotExist) {
		out.Missing = true
		return out, nil
	}
	if err != nil {
		return out, err
	}
	out.Lines, out.Size, out.Truncated = lines, size, truncated
	return out, nil
}

// tailFile returns up to n of the last lines in path's final window bytes,
// keeping only lines containing grep (case-insensitive) when it is set.
// truncated reports that the window didn't reach the start of the file, so
// older lines exist that weren't searched.
func tailFile(path string, window int64, n int, grep string) (lines []string, size int64, truncated bool, err error) {
	if n <= 0 {
		n = logTailDefaultLines
	}
	if n > logTailMaxLines {
		n = logTailMaxLines
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, false, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, 0, false, err
	}
	size = fi.Size()
	off := size - window
	if off < 0 {
		off = 0
	}
	buf := make([]byte, size-off)
	if _, err := f.ReadAt(buf, off); err != nil && !errors.Is(err, io.EOF) {
		return nil, size, false, err
	}
	// The window almost always starts mid-line; drop the fragment.
	if off > 0 {
		if i := bytes.IndexByte(buf, '\n'); i >= 0 {
			buf = buf[i+1:]
		} else {
			buf = nil
		}
	}
	buf = bytes.TrimRight(buf, "\n")

	q := strings.ToLower(strings.TrimSpace(grep))
	all := bytes.Split(buf, []byte{'\n'})
	lines = make([]string, 0, min(n, len(all)))
	// Walk backwards so a grep stops as soon as it has n matches.
	for i := len(all) - 1; i >= 0 && len(lines) < n; i-- {
		l := string(all[i])
		if l == "" {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(l), q) {
			continue
		}
		lines = append(lines, l)
	}
	for i, j := 0, len(lines)-1; i < j; i, j = i+1, j-1 {
		lines[i], lines[j] = lines[j], lines[i]
	}
	return lines, size, off > 0, nil
}
