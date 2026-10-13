package rules

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
)

func TestLogBatchTrimsToMax(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	var batch []LogEntry
	for i := 0; i < MaxLogEntries+50; i++ {
		batch = append(batch, LogEntry{QueryName: fmt.Sprintf("q%d.example", i), Action: "allow"})
	}
	if err := s.LogBatch(ctx, batch[:100]); err != nil {
		t.Fatal(err)
	}
	if err := s.LogBatch(ctx, batch); err != nil {
		t.Fatal(err)
	}
	got, err := s.RecentLogs(ctx, MaxLogEntries*2, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != MaxLogEntries {
		t.Fatalf("kept %d entries, want %d", len(got), MaxLogEntries)
	}
	if want := fmt.Sprintf("q%d.example", MaxLogEntries+49); got[0].QueryName != want {
		t.Fatalf("newest = %q, want %q", got[0].QueryName, want)
	}
}
