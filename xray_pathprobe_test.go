package main

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/ehsan/em-wall/core/xray"
)

func pathSlot(keys ...string) xray.DialerSlot {
	sl := xray.DialerSlot{Master: "m1", Aliases: []string{"m2"}, Index: 0}
	for _, k := range keys {
		sl.Members = append(sl.Members, xray.DialerMember{Key: k})
	}
	return sl
}

func pingStatus(ms int64) nodeStatus {
	var st nodeStatus
	st.Alive, st.Delay = true, ms
	st.HealthPing.All = 6
	st.HealthPing.Average = ms * int64(time.Millisecond)
	return st
}

func transfer(key string, ok bool) pathResult {
	return pathResult{pathJob: pathJob{key: key}, ok: ok}
}

func chainRes(master, key string, ok bool) pathResult {
	return pathResult{pathJob: pathJob{key: key, master: master}, ok: ok}
}

func newTestProber(now *time.Time) *pathProber {
	p := newPathProber()
	p.now = func() time.Time { return *now }
	return p
}

// The failure the pool showed: a node that passes every ping but stalls a
// transfer is broken after pathStrikes witnessed rounds, and one pass
// clears it.
func TestPathProberStrikesAndClears(t *testing.T) {
	now := time.Unix(1000, 0)
	p := newTestProber(&now)
	sl := pathSlot("fin", "ger")

	if e := p.record([]pathResult{transfer("fin", false), transfer("ger", true)}); len(e) != 0 {
		t.Fatalf("one strike flipped a verdict: %v", e)
	}
	now = now.Add(time.Minute)
	e := p.record([]pathResult{transfer("fin", false), transfer("ger", true)})
	if len(e) != 1 || !e[0].bad || e[0].key != "fin" {
		t.Fatalf("edges = %+v, want fin broken", e)
	}
	if got := p.broken(sl); !slices.Equal(got, []string{"fin"}) {
		t.Fatalf("broken = %v, want [fin]", got)
	}
	now = now.Add(time.Minute)
	if e := p.record([]pathResult{transfer("fin", true)}); len(e) != 1 || e[0].bad {
		t.Fatalf("edges = %+v, want fin clean again", e)
	}
	if got := p.broken(sl); len(got) != 0 {
		t.Fatalf("broken = %v after a pass", got)
	}
}

// Nothing passing is the uplink: no member is struck.
func TestPathProberNoWitnessNoStrike(t *testing.T) {
	now := time.Unix(1000, 0)
	p := newTestProber(&now)
	for i := 0; i < 3; i++ {
		now = now.Add(time.Minute)
		p.record([]pathResult{transfer("a", false), transfer("b", false)})
	}
	if got := p.broken(pathSlot("a", "b")); len(got) != 0 {
		t.Fatalf("broken = %v on an all-fail link", got)
	}
}

// A chain verdict is per master: a node that can't carry the alias breaks
// the shared slot, and witnesses come only from the same master — a node
// failing every chain to a master whose server is down is not blamed.
func TestPathProberChainPerMaster(t *testing.T) {
	now := time.Unix(1000, 0)
	p := newTestProber(&now)
	for i := 0; i < pathStrikes; i++ {
		now = now.Add(time.Minute)
		p.record([]pathResult{
			chainRes("m2", "uk", false), chainRes("m2", "ger", true),
			chainRes("m1", "uk", true), chainRes("m1", "ger", true),
			// m3's server is down: every chain fails, nobody is struck.
			chainRes("m3", "uk", false), chainRes("m3", "ger", false),
		})
	}
	if got := p.broken(pathSlot("uk", "ger")); !slices.Equal(got, []string{"uk"}) {
		t.Fatalf("broken = %v, want [uk] (fails alias m2)", got)
	}
	other := xray.DialerSlot{Master: "m3", Members: []xray.DialerMember{{Key: "uk"}, {Key: "ger"}}}
	if got := p.broken(other); len(got) != 0 {
		t.Fatalf("broken = %v for a master whose server is down", got)
	}
}

// Each round tests the carriers plus the best-pinging members whose
// verdict is due, and every master over each of them; a fresh pass is not
// re-tested until it goes stale, a broken member waits pathBadRecheck.
func TestPathProberPlan(t *testing.T) {
	now := time.Unix(1000, 0)
	p := newTestProber(&now)
	keys := []string{"c1", "c2", "x1", "x2", "x3", "x4", "x5", "dead"}
	sl := pathSlot(keys...)
	sl.Preferred = []string{"c1", "c2"}
	byTag := map[string]nodeStatus{}
	for i, k := range keys[:7] {
		byTag[xray.SlotMemberTag(0, k)] = pingStatus(int64(100 + 10*i))
	}
	var dead nodeStatus
	dead.HealthPing.All, dead.HealthPing.Fail = 6, 6
	byTag[xray.SlotMemberTag(0, "dead")] = dead

	members := func(jobs []pathJob) []string {
		var out []string
		for _, j := range jobs {
			if j.master == "" {
				out = append(out, j.key)
			}
		}
		return out
	}
	jobs := p.plan([]xray.DialerSlot{sl}, byTag)
	if got := members(jobs); !slices.Equal(got, []string{"c1", "c2", "x1", "x2", "x3", "x4"}) {
		t.Fatalf("tested = %v, want carriers + %d best candidates", got, pathProbeCandidates)
	}
	if len(jobs) != 6*3 {
		t.Fatalf("jobs = %d, want a transfer and a chain per master for each", len(jobs))
	}
	var res []pathResult
	for _, j := range jobs {
		res = append(res, pathResult{pathJob: j, ok: true})
	}
	p.record(res)

	now = now.Add(pathProbeInterval)
	if got := members(p.plan([]xray.DialerSlot{sl}, byTag)); !slices.Equal(got, []string{"c1", "c2", "x5"}) {
		t.Fatalf("tested = %v, want carriers + the one member still unverified", got)
	}
	now = now.Add(pathGoodTTL)
	if got := members(p.plan([]xray.DialerSlot{sl}, byTag)); len(got) != 6 {
		t.Fatalf("tested = %v, want stale passes re-tested", got)
	}
}

// The shortlist never seats a broken member while anything else
// qualifies, and drops an incumbent the moment it is broken.
func TestShortlistSkipsBroken(t *testing.T) {
	sl := newSlotShortlist()
	slot := pathSlot("fast", "mid", "slow")
	byTag := map[string]nodeStatus{
		xray.SlotMemberTag(0, "fast"): pingStatus(100),
		xray.SlotMemberTag(0, "mid"):  pingStatus(400),
		xray.SlotMemberTag(0, "slow"): pingStatus(900),
	}
	sl.observe([]xray.DialerSlot{slot}, byTag)
	if got := sl.picks["m1"]; !slices.Contains(got, "fast") {
		t.Fatalf("picks = %v, want fast seated", got)
	}
	slot.Broken = []string{"fast"}
	sl.observe([]xray.DialerSlot{slot}, byTag)
	if got := sl.picks["m1"]; slices.Contains(got, "fast") || len(got) != 2 {
		t.Fatalf("picks = %v, want the two unbroken members", got)
	}
	// All broken: keep ranking rather than seat nothing.
	slot.Broken = []string{"fast", "mid", "slow"}
	sl.observe([]xray.DialerSlot{slot}, byTag)
	if got := sl.picks["m1"]; len(got) != 2 {
		t.Fatalf("picks = %v, want a shortlist even with every member broken", got)
	}
}

func TestAgileSkipsBroken(t *testing.T) {
	slot := pathSlot("fast", "mid")
	slot.Strategy = xray.StrategyAgile
	slot.Broken = []string{"fast"}
	byTag := map[string]nodeStatus{
		xray.SlotMemberTag(0, "fast"): pingStatus(100),
		xray.SlotMemberTag(0, "mid"):  pingStatus(400),
	}
	pk, ok := pickAgile(slot, byTag, agilePick{})
	if !ok || slices.Contains(pk.active, "fast") || pk.spare == "fast" {
		t.Fatalf("pick = %+v, want fast left out", pk)
	}
}

// runPathJobs speaks SOCKS5 with the job's username to the probe port and
// classifies a stalled transfer as a failure. The fake probe inbound below
// stands in for xray: it routes by username to a body that either arrives
// whole or stops after 8 KB, like the choked node.
func TestRunPathJobsTransfer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go fakeProbeInbound(ln)
	port := ln.Addr().(*net.TCPAddr).Port

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res := runPathJobs(ctx, []pathJob{{user: "good", key: "good"}, {user: "choked", key: "choked"}}, port)
	if !res[0].ok || res[1].ok {
		t.Fatalf("results = %+v, want good ok and choked failed", res)
	}
}

func fakeProbeInbound(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			defer c.Close()
			buf := make([]byte, 512)
			// Greeting: offer user/pass.
			if _, err := io.ReadFull(c, buf[:2]); err != nil {
				return
			}
			io.ReadFull(c, buf[:buf[1]])
			c.Write([]byte{5, 2})
			// RFC 1929 auth.
			io.ReadFull(c, buf[:2])
			user := make([]byte, buf[1])
			io.ReadFull(c, user)
			io.ReadFull(c, buf[:1])
			io.ReadFull(c, buf[:buf[0]])
			c.Write([]byte{1, 0})
			// CONNECT request.
			io.ReadFull(c, buf[:4])
			switch buf[3] {
			case 3:
				io.ReadFull(c, buf[:1])
				io.ReadFull(c, buf[:int(buf[0])+2])
			case 1:
				io.ReadFull(c, buf[:6])
			}
			c.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 0})
			req, err := http.ReadRequest(bufio.NewReader(c))
			if err != nil {
				return
			}
			n, _ := strconv.Atoi(req.URL.Query().Get("bytes"))
			body := make([]byte, n)
			c.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: " + strconv.Itoa(n) + "\r\n\r\n"))
			if string(user) == "choked" {
				c.Write(body[:8<<10])
				time.Sleep(10 * time.Second)
				return
			}
			c.Write(body)
		}(c)
	}
}

// A member choking its transfer is no witness for a chain: its own pass
// through a master doesn't prove the master works for the others.
func TestPathProberChokedMembersOutOfQuorum(t *testing.T) {
	now := time.Unix(1000, 0)
	p := newTestProber(&now)
	for i := 0; i < pathStrikes; i++ {
		now = now.Add(time.Minute)
		p.record([]pathResult{
			transfer("fin", false), transfer("ger", true), transfer("uk", true), transfer("usa", true),
			chainRes("m1", "fin", false), chainRes("m1", "ger", true),
			chainRes("m1", "uk", false), chainRes("m1", "usa", false),
		})
	}
	got := p.broken(pathSlot("fin", "ger", "uk", "usa"))
	if !slices.Equal(got, []string{"fin", "uk", "usa"}) {
		t.Fatalf("broken = %v, want fin (transfer) + uk, usa (chain)", got)
	}
}
