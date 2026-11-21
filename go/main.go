// git-forge: a standalone GitHub-compatible forge in one static binary.
//
// Under the make.fun harness (MAKEFUN_ENV_URL set) it binds an ephemeral port, POSTs it to
// $MAKEFUN_ENV_URL/register, serves the suite and polls $MAKEFUN_ENV_URL/done until the suite is
// finished, then exits 0. Without MAKEFUN_ENV_URL it serves on $PORT (or an ephemeral port).

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
)

const defaultLogin = "adaiversen"

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "[forge] "+format+"\n", args...)
}

func main() {
	// The store is small and short-lived: collect rarely.
	debug.SetGCPercent(400)
	capProcs()
	login := os.Getenv("MAKEFUN_FORGE_LOGIN")
	if login == "" {
		login = os.Getenv("FORGE_LOGIN")
	}
	if login == "" {
		login = defaultLogin
	}
	srv := &Server{forge: NewForge(login), token: os.Getenv("MAKEFUN_SHOP_TOKEN")}
	envURL := strings.TrimRight(os.Getenv("MAKEFUN_ENV_URL"), "/")
	addr := ":0"
	if envURL == "" && os.Getenv("PORT") != "" {
		addr = ":" + os.Getenv("PORT")
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		logf("listen: %v", err)
		os.Exit(1)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				if ne, ok := err.(net.Error); ok && ne.Timeout() {
					continue
				}
				return
			}
			go serveConn(conn, srv.Handle)
		}
	}()
	if envURL == "" {
		fmt.Printf("[forge] serving on http://127.0.0.1:%d as %s\n", port, login)
		select {}
	}
	fmt.Printf("[forge] listening on port %d as %s\n", port, login)
	client := &http.Client{Timeout: 30 * time.Second}
	body, _ := json.Marshal(map[string]int{"port": port})
	for attempt := 0; ; attempt++ {
		res, err := client.Post(envURL+"/register", "application/json", bytes.NewReader(body))
		if err == nil {
			io.Copy(io.Discard, res.Body)
			res.Body.Close()
			if res.StatusCode == 200 || res.StatusCode == 409 {
				break
			}
			if res.StatusCode == 400 || res.StatusCode == 405 {
				logf("register answered HTTP %d", res.StatusCode)
				os.Exit(1)
			}
		}
		time.Sleep(time.Duration(min(1000, 50<<min(attempt, 5))) * time.Millisecond)
	}
	fmt.Println("[forge] registered; serving until the env says done")
	for attempt := 0; ; attempt++ {
		res, err := client.Get(envURL + "/done")
		if err == nil {
			var out struct {
				Done bool `json:"done"`
			}
			if res.StatusCode == 200 {
				json.NewDecoder(res.Body).Decode(&out)
			}
			io.Copy(io.Discard, res.Body)
			res.Body.Close()
			if out.Done {
				break
			}
			attempt = 0
		}
		time.Sleep(time.Duration(min(2000, 250<<min(attempt, 3))) * time.Millisecond)
	}
	fmt.Println("[forge] done")
	os.Exit(0)
}

// capProcs sets GOMAXPROCS to the container's CPU quota. Go 1.24 sizes it from the CPUs it can see,
// which in a sandbox can be the whole node: many idle Ps mean extra threads and wake-ups on every
// request. The quota comes from cgroup v2 cpu.max or v1 cfs files; when neither is readable it falls
// back to the trial's documented limit of 2 CPUs.
func capProcs() {
	if os.Getenv("GOMAXPROCS") != "" {
		return
	}
	n := 2
	if q, p, ok := cpuQuota(); ok {
		n = (q + p - 1) / p
		if n < 2 {
			n = 2
		}
	}
	if n < runtime.GOMAXPROCS(0) {
		runtime.GOMAXPROCS(n)
	}
}

func cpuQuota() (quota, period int, ok bool) {
	if b, err := os.ReadFile("/sys/fs/cgroup/cpu.max"); err == nil {
		f := strings.Fields(string(b))
		if len(f) == 2 && f[0] != "max" {
			q, e1 := strconv.Atoi(f[0])
			p, e2 := strconv.Atoi(f[1])
			if e1 == nil && e2 == nil && q > 0 && p > 0 {
				return q, p, true
			}
		}
	}
	qb, e1 := os.ReadFile("/sys/fs/cgroup/cpu/cpu.cfs_quota_us")
	pb, e2 := os.ReadFile("/sys/fs/cgroup/cpu/cpu.cfs_period_us")
	if e1 == nil && e2 == nil {
		q, e3 := strconv.Atoi(strings.TrimSpace(string(qb)))
		p, e4 := strconv.Atoi(strings.TrimSpace(string(pb)))
		if e3 == nil && e4 == nil && q > 0 && p > 0 {
			return q, p, true
		}
	}
	return 0, 0, false
}
