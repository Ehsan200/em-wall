// scripts/smoketest exercises the daemon end-to-end without the UI:
// add a block rule, list, query DNS, delete the rule.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/miekg/dns"

	"github.com/ehsan/em-wall/core/ipc"
)

func main() {
	sock := flag.String("socket", "/tmp/em-wall-test.sock", "ipc socket")
	dnsAddr := flag.String("dns", "127.0.0.1:15353", "dns proxy address")
	flag.Parse()

	c, err := ipc.Dial(*sock)
	if err != nil {
		log.Fatalf("dial: %v", err)
	}
	defer c.Close()

	var status ipc.StatusResult
	if err := c.Call(ipc.MethodStatus, nil, &status); err != nil {
		log.Fatalf("status: %v", err)
	}
	fmt.Printf("daemon: v%s up=%s rules=%d listen=%s\n",
		status.Version, status.Uptime, status.RuleCount, status.ListenAddr)

	var added ipc.RuleDTO
	if err := c.Call(ipc.MethodRulesAdd, ipc.RulesAddParams{
		Pattern: "*.smoketest.invalid", Action: "block", Enabled: true,
	}, &added); err != nil {
		log.Fatalf("rules.add: %v", err)
	}
	fmt.Printf("added rule id=%d pattern=%s\n", added.ID, added.Pattern)

	rcode := dnsQuery(*dnsAddr, "x.smoketest.invalid")
	fmt.Printf("dns query x.smoketest.invalid → %s (want NXDOMAIN)\n", rcode)
	if rcode != "NXDOMAIN" {
		fmt.Println("FAIL: blocked rule did not NXDOMAIN")
		os.Exit(1)
	}

	rcode2 := dnsQuery(*dnsAddr, "other.smoketest.invalid")
	fmt.Printf("dns query other.smoketest.invalid → %s (want NXDOMAIN, wildcard match)\n", rcode2)

	if err := c.Call(ipc.MethodRulesDelete, ipc.RulesDeleteParams{ID: added.ID}, nil); err != nil {
		log.Fatalf("rules.delete: %v", err)
	}
	fmt.Println("deleted rule")
	fmt.Println("PASS")
}

func dnsQuery(addr, name string) string {
	c := &dns.Client{Net: "udp", Timeout: 2 * time.Second}
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), dns.TypeA)
	resp, _, err := c.Exchange(m, addr)
	if err != nil {
		return fmt.Sprintf("ERROR(%v)", err)
	}
	return dns.RcodeToString[resp.Rcode]
}
