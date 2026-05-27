package proxy

import (
	"net"
	"testing"
	"time"
)

func TestTable_RecordAndLookup(t *testing.T) {
	tb := NewTable(time.Millisecond)
	ip := net.ParseIP("1.2.3.4")
	tb.Record(ip, "example.com", []string{"work"}, 30*time.Second, 7)
	got, ok := tb.Lookup(ip)
	if !ok {
		t.Fatal("expected hit")
	}
	if got.Hostname != "example.com" || got.ProxyNames[0] != "work" || got.RuleID != 7 {
		t.Errorf("unexpected entry: %+v", got)
	}
}

func TestTable_ExpiryHidesFromLookup(t *testing.T) {
	tb := NewTable(time.Millisecond)
	ip := net.ParseIP("1.2.3.4")
	tb.Record(ip, "x.com", []string{"work"}, time.Millisecond, 1)
	time.Sleep(5 * time.Millisecond)
	if _, ok := tb.Lookup(ip); ok {
		t.Errorf("expected expired entry to miss Lookup")
	}
}

func TestTable_RemoveByRule(t *testing.T) {
	tb := NewTable(time.Second)
	tb.Record(net.ParseIP("1.1.1.1"), "a.com", []string{"work"}, time.Hour, 1)
	tb.Record(net.ParseIP("2.2.2.2"), "b.com", []string{"work"}, time.Hour, 1)
	tb.Record(net.ParseIP("3.3.3.3"), "c.com", []string{"home"}, time.Hour, 2)

	if n := tb.RemoveByRule(1); n != 2 {
		t.Errorf("RemoveByRule(1) = %d, want 2", n)
	}
	if _, ok := tb.Lookup(net.ParseIP("1.1.1.1")); ok {
		t.Errorf("expected 1.1.1.1 to be gone")
	}
	if _, ok := tb.Lookup(net.ParseIP("3.3.3.3")); !ok {
		t.Errorf("expected rule 2's entry to survive")
	}
}

func TestTable_RecordOverwrite(t *testing.T) {
	tb := NewTable(time.Second)
	ip := net.ParseIP("1.2.3.4")
	tb.Record(ip, "old.com", []string{"old"}, time.Hour, 1)
	tb.Record(ip, "new.com", []string{"new"}, time.Hour, 2)

	got, _ := tb.Lookup(ip)
	if got.Hostname != "new.com" || got.RuleID != 2 {
		t.Errorf("overwrite failed: %+v", got)
	}
	// Removing rule 1 should NOT touch the rebinded entry.
	tb.RemoveByRule(1)
	if _, ok := tb.Lookup(ip); !ok {
		t.Errorf("RemoveByRule(1) removed the rule-2 entry")
	}
}

func TestTable_SweepExpired(t *testing.T) {
	tb := NewTable(time.Millisecond)
	tb.Record(net.ParseIP("1.1.1.1"), "a", []string{"x"}, time.Millisecond, 1)
	tb.Record(net.ParseIP("2.2.2.2"), "b", []string{"x"}, time.Hour, 2)
	time.Sleep(5 * time.Millisecond)
	if n := tb.SweepExpired(); n != 1 {
		t.Errorf("SweepExpired = %d, want 1", n)
	}
	if tb.Len() != 1 {
		t.Errorf("Len after sweep = %d, want 1", tb.Len())
	}
}
