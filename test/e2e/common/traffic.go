//go:build e2e

package common

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const monitorLog = "/tmp/egw-monitor.log"

// TrafficMonitor runs a request loop inside the client pod every 200ms, so it
// keeps measuring even while the API server is down.
type TrafficMonitor struct{}

func StartMonitor() *TrafficMonitor {
	GinkgoHelper()
	loop := fmt.Sprintf(`echo $$ > /tmp/egw-monitor.pid; while true; do `+
		`t=$(cut -d" " -f1 /proc/uptime); r=$(curl -s -m 1 http://%s/clientip 2>/dev/null); `+
		`echo "$t ${r%%%%:*}"; sleep 0.2; done > %s`, E.TargetAddr, monitorLog)
	Kubectl("exec", "client", "--", "sh", "-c", fmt.Sprintf("rm -f %s; nohup sh -c '%s' >/dev/null 2>&1 &", monitorLog, loop))
	time.Sleep(2 * time.Second)
	return &TrafficMonitor{}
}

type TrafficStats struct {
	Requests, Failed int
	// LongestGap is the longest time without a successful request with the
	// egress IP, including before the first and after the last success.
	LongestGap  time.Duration
	WrongSource map[string]int
}

func (s TrafficStats) String() string {
	return fmt.Sprintf("%d requests, %d failed, longest time without success %v, wrong source %v",
		s.Requests, s.Failed, s.LongestGap.Round(10*time.Millisecond), s.WrongSource)
}

// Stop stops the loop and returns the statistics. kubectl is retried because
// the API server may just have come back.
func (m *TrafficMonitor) Stop() TrafficStats {
	GinkgoHelper()
	var out string
	Eventually(func() error {
		var err error
		out, err = Command("kubectl", "exec", "client", "--", "sh", "-c",
			fmt.Sprintf("kill $(cat /tmp/egw-monitor.pid) 2>/dev/null; cat %s", monitorLog))
		return err
	}).WithTimeout(2 * time.Minute).Should(Succeed())
	s := TrafficStats{WrongSource: map[string]int{}}
	first, last, lastOK := -1.0, -1.0, -1.0
	gap := func(from, to float64) {
		if g := time.Duration((to - from) * float64(time.Second)); g > s.LongestGap {
			s.LongestGap = g
		}
	}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		ts, err := strconv.ParseFloat(f[0], 64)
		if err != nil {
			continue
		}
		s.Requests++
		if first < 0 {
			first = ts
		}
		last = ts
		src := ""
		if len(f) > 1 {
			src = f[1]
		}
		if src != E.EgressIP() {
			s.Failed++
			if src != "" {
				s.WrongSource[src]++
			}
			continue
		}
		if lastOK >= 0 {
			gap(lastOK, ts)
		} else {
			gap(first, ts)
		}
		lastOK = ts
	}
	if lastOK < 0 {
		gap(first, last)
	} else {
		gap(lastOK, last)
	}
	Expect(s.Requests).To(BeNumerically(">", 0), "traffic monitor produced no output: %q", out)
	return s
}

// ExpectNoInterruption fails when egress traffic was interrupted. A normal
// request cycle takes about 0.2s.
func ExpectNoInterruption(s TrafficStats) {
	GinkgoHelper()
	GinkgoWriter.Printf("traffic: %s\n", s)
	Expect(s.Failed).To(BeZero(), "egress traffic interrupted: %s", s)
	Expect(s.LongestGap).To(BeNumerically("<=", 2*time.Second), "egress traffic interrupted: %s", s)
}
