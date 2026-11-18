// Command cpuburn burns CPU on demand, for the CPU-target scaling tests
// (#1789, #1800).
//
// As a Cloud Run service it listens on $PORT: GET /burn?ms=N spins one
// goroutine for N milliseconds and answers; anything else answers at once.
// As a worker pool instance (no request ever arrives) it spins every CPU it
// has until the Unix time in BURN_UNTIL, then idles, so every instance,
// including those started while it burns, stops at the same moment.
package main

import (
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"time"
)

func spin(until time.Time) {
	x := 0
	for time.Now().Before(until) {
		for i := 0; i < 100000; i++ {
			x += i * i
		}
	}
	_ = x
}

func main() {
	if v := os.Getenv("BURN_UNTIL"); v != "" {
		if sec, err := strconv.ParseInt(v, 10, 64); err == nil {
			until := time.Unix(sec, 0)
			for i := 0; i < runtime.NumCPU(); i++ {
				go spin(until)
			}
		}
	}
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	http.HandleFunc("/burn", func(w http.ResponseWriter, r *http.Request) {
		ms, _ := strconv.Atoi(r.URL.Query().Get("ms"))
		spin(time.Now().Add(time.Duration(ms) * time.Millisecond))
		fmt.Fprintf(w, "burned %d ms\n", ms)
	})
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "ok") })
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
