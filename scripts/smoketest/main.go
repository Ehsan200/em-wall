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

	// ---- Proxies CRUD ----
	var proxy ipc.ProxyDTO
	if err := c.Call(ipc.MethodProxiesAdd, ipc.ProxiesAddParams{
		Name: "smoketest-work", Protocol: "socks5",
		Host: "127.0.0.1", Port: 1080,
		Username: "alice", Password: "s3cret",
	}, &proxy); err != nil {
		log.Fatalf("proxies.add: %v", err)
	}
	fmt.Printf("added proxy id=%d name=%s hasPassword=%v\n", proxy.ID, proxy.Name, proxy.HasPassword)

	// Rule referencing a non-existent proxy should be rejected.
	var rejected ipc.RuleDTO
	if err := c.Call(ipc.MethodRulesAdd, ipc.RulesAddParams{
		Pattern: "*.proxytest.invalid", Action: "route",
		Interface: "proxy:no-such-proxy", Enabled: true,
	}, &rejected); err == nil {
		fmt.Println("FAIL: rule with unknown proxy ref was accepted")
		os.Exit(1)
	}
	fmt.Println("rule with unknown proxy ref correctly rejected")

	// Rule referencing the new proxy should succeed.
	var pxRule ipc.RuleDTO
	if err := c.Call(ipc.MethodRulesAdd, ipc.RulesAddParams{
		Pattern: "*.proxytest.invalid", Action: "route",
		Interface: "proxy:smoketest-work", Enabled: true,
	}, &pxRule); err != nil {
		log.Fatalf("proxies-bound rule.add: %v", err)
	}
	fmt.Printf("added proxy-bound rule id=%d interface=%s\n", pxRule.ID, pxRule.Interface)

	// Deleting the proxy while it's referenced should fail.
	if err := c.Call(ipc.MethodProxiesDelete, ipc.ProxiesDeleteParams{ID: proxy.ID}, nil); err == nil {
		fmt.Println("FAIL: proxy delete should have been blocked by rule reference")
		os.Exit(1)
	}
	fmt.Println("proxy delete correctly blocked while referenced")

	// Tear down rule, then proxy.
	if err := c.Call(ipc.MethodRulesDelete, ipc.RulesDeleteParams{ID: pxRule.ID}, nil); err != nil {
		log.Fatalf("rules.delete (px-bound): %v", err)
	}
	if err := c.Call(ipc.MethodProxiesDelete, ipc.ProxiesDeleteParams{ID: proxy.ID}, nil); err != nil {
		log.Fatalf("proxies.delete: %v", err)
	}
	fmt.Println("deleted proxy")

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
