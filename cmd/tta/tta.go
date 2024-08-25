// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// The tta server is the Tailscale Test Agent.
//
// It runs on each Tailscale node being integration tested and permits the test
// harness to control the node. It connects out to the test drver (rather than
// accepting any TCP connections inbound, which might be blocked depending on
// the scenario being tested) and then the test driver turns the TCP connection
// around and sends request back.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"tailscale.com/atomicfile"
	"tailscale.com/client/tailscale"
	"tailscale.com/hostinfo"
	"tailscale.com/util/must"
	"tailscale.com/util/set"
	"tailscale.com/version/distro"
)

var (
	driverAddr = flag.String("driver", "test-driver.tailscale:8008", "address of the test driver; by default we use the DNS name test-driver.tailscale which is special cased in the emulated network's DNS server")
)

func absify(cmd string) string {
	if distro.Get() == distro.Gokrazy && !strings.Contains(cmd, "/") {
		return "/user/" + cmd
	}
	return cmd
}

func serveCmd(w http.ResponseWriter, cmd string, args ...string) {
	log.Printf("Got serveCmd for %q %v", cmd, args)
	out, err := exec.Command(absify(cmd), args...).CombinedOutput()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if err != nil {
		w.Header().Set("Exec-Err", err.Error())
		w.WriteHeader(500)
		log.Printf("Err on serveCmd for %q %v, %d bytes of output: %v", cmd, args, len(out), err)
	} else {
		log.Printf("Did serveCmd for %q %v, %d bytes of output", cmd, args, len(out))
	}
	w.Write(out)
}

type localClientRoundTripper struct {
	lc tailscale.LocalClient
}

func (rt *localClientRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.RequestURI = ""
	return rt.lc.DoLocalRequest(req)
}

func main() {
	var logBuf logBuffer
	log.SetOutput(io.MultiWriter(os.Stderr, &logBuf))

	if distro.Get() == distro.Gokrazy {
		if !hostinfo.IsNATLabGuestVM() {
			// "Exiting immediately with status code 0 when the
			// GOKRAZY_FIRST_START=1 environment variable is set means “don’t
			// start the program on boot”"
			return
		}
	}
	flag.Parse()

	if distro.Get() == distro.Gokrazy {
		cmdLine, _ := os.ReadFile("/proc/cmdline")
		explicitNS := false
		for _, s := range strings.Fields(string(cmdLine)) {
			if ns, ok := strings.CutPrefix(s, "tta.nameserver="); ok {
				err := atomicfile.WriteFile("/tmp/resolv.conf", []byte("nameserver "+ns+"\n"), 0644)
				log.Printf("Wrote /tmp/resolv.conf: %v", err)
				explicitNS = true
				break
			}
		}
		if !explicitNS {
			nsRx := regexp.MustCompile(`(?m)^nameserver (.*)`)
			for t := time.Now(); time.Since(t) < 10*time.Second; time.Sleep(10 * time.Millisecond) {
				all, _ := os.ReadFile("/etc/resolv.conf")
				if nsRx.Match(all) {
					break
				}
			}
		}
	}

	log.Printf("Tailscale Test Agent running.")

	gokRP := httputil.NewSingleHostReverseProxy(must.Get(url.Parse("http://gokrazy")))
	gokRP.Transport = &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if network != "tcp" {
				return nil, errors.New("unexpected network")
			}
			if addr != "gokrazy:80" {
				return nil, errors.New("unexpected addr")
			}
			var d net.Dialer
			return d.DialContext(ctx, "unix", "/run/gokrazy-http.sock")
		},
	}

	var ttaMux http.ServeMux // agent mux
	var serveMux http.ServeMux
	serveMux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-TTA-GoKrazy") == "1" {
			gokRP.ServeHTTP(w, r)
			return
		}
		ttaMux.ServeHTTP(w, r)
	})
	var hs http.Server
	hs.Handler = &serveMux
	var (
		stMu   sync.Mutex
		newSet = set.Set[net.Conn]{} // conns in StateNew
	)
	needConnCh := make(chan bool, 1)
	hs.ConnState = func(c net.Conn, s http.ConnState) {
		stMu.Lock()
		defer stMu.Unlock()
		oldLen := len(newSet)
		switch s {
		case http.StateNew:
			newSet.Add(c)
		default:
			newSet.Delete(c)
		}
		if oldLen != 0 && len(newSet) == 0 {
			select {
			case needConnCh <- true:
			default:
			}
		}
	}
	conns := make(chan net.Conn, 1)

	lcRP := httputil.NewSingleHostReverseProxy(must.Get(url.Parse("http://local-tailscaled.sock")))
	lcRP.Transport = new(localClientRoundTripper)
	ttaMux.HandleFunc("/localapi/", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("Got localapi request: %v", r.URL)
		t0 := time.Now()
		lcRP.ServeHTTP(w, r)
		log.Printf("Did localapi request in %v: %v", time.Since(t0).Round(time.Millisecond), r.URL)
	})

	ttaMux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "TTA\n")
		return
	})
	ttaMux.HandleFunc("/up", func(w http.ResponseWriter, r *http.Request) {
		serveCmd(w, "tailscale", "up", "--login-server=http://control.tailscale")
	})
	ttaMux.HandleFunc("/fw", addFirewallHandler)
	ttaMux.HandleFunc("/logs", func(w http.ResponseWriter, r *http.Request) {
		logBuf.mu.Lock()
		defer logBuf.mu.Unlock()
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write(logBuf.buf.Bytes())
	})
	go hs.Serve(chanListener(conns))

	// For doing agent operations locally from gokrazy:
	// (e.g. with "wget -O - localhost:8123/fw" or "wget -O - localhost:8123/logs"
	// to get early tta logs before the port 124 connection is established)
	go func() {
		err := http.ListenAndServe("127.0.0.1:8123", &ttaMux)
		if err != nil {
			log.Fatalf("ListenAndServe: %v", err)
		}
	}()

	var lastErr string
	needConnCh <- true
	for {
		<-needConnCh
		c, err := connect()
		if err != nil {
			s := err.Error()
			if s != lastErr {
				log.Printf("Connect failure: %v", s)
			}
			lastErr = s
			time.Sleep(time.Second)
			continue
		}
		conns <- c
	}
}

func connect() (net.Conn, error) {
	c, err := net.Dial("tcp", *driverAddr)
	if err != nil {
		return nil, err
	}
	return c, nil
}

type chanListener <-chan net.Conn

func (cl chanListener) Accept() (net.Conn, error) {
	c, ok := <-cl
	if !ok {
		return nil, errors.New("closed")
	}
	return c, nil
}

func (cl chanListener) Close() error {
	return nil
}

func (cl chanListener) Addr() net.Addr {
	return &net.TCPAddr{
		IP:   net.ParseIP("52.0.0.34"), // TS..DR(iver)
		Port: 123,
	}
}

func addFirewallHandler(w http.ResponseWriter, r *http.Request) {
	if addFirewall == nil {
		http.Error(w, "firewall not supported", 500)
		return
	}
	err := addFirewall()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	io.WriteString(w, "OK\n")
}

var addFirewall func() error // set by fw_linux.go

// logBuffer is a bytes.Buffer that is safe for concurrent use
// intended to capture early logs from the process, even if
// gokrazy's syslog streaming isn't working or yet working.
// It only captures the first 1MB of logs, as that's considered
// plenty for early debugging. At runtime, it's assumed that
// syslog log streaming is working.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (lb *logBuffer) Write(p []byte) (n int, err error) {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	const maxSize = 1 << 20 // more than plenty; see type comment
	if lb.buf.Len() > maxSize {
		return len(p), nil
	}
	return lb.buf.Write(p)
}