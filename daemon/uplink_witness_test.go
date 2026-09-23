package main

import (
	"testing"
	"time"
)

func TestUplinkWitness(t *testing.T) {
	now := time.Unix(1000, 0)
	w := newUplinkWitness()
	w.now = func() time.Time { return now }

	if w.otherAlive("a") {
		t.Fatalf("nothing seen yet: uplink not proven")
	}
	w.saw("a")
	if w.otherAlive("a") {
		t.Fatalf("a's own success can't vouch for a")
	}
	if !w.otherAlive("b") {
		t.Fatalf("a carried data just now: b's silence is b's fault")
	}
	now = now.Add(uplinkWitnessWindow + time.Second)
	if w.otherAlive("b") {
		t.Fatalf("stale evidence must not count")
	}
	var nilW *uplinkWitness
	if !nilW.otherAlive("x") {
		t.Fatalf("nil witness must keep always-blame behaviour")
	}
}
