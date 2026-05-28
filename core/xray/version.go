package xray

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"time"
)

// Version runs `<binary> version` and returns the first line of
// output, which xray formats as e.g.
//
//	Xray 26.3.27 (Xray, Penetrates Everything.) ...
//
// Returns "" + err if the binary can't be exec'd. Caller decides
// what to do with the version string (display in UI, log, refuse to
// start if too old, …).
func Version(binary string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var buf bytes.Buffer
	cmd := exec.CommandContext(ctx, binary, "version")
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		return "", err
	}
	line, _, _ := strings.Cut(buf.String(), "\n")
	return strings.TrimSpace(line), nil
}
