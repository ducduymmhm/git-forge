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
	"runtime/debug"
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
