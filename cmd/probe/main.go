// Command probe times a full collection with the integrations switched on,
// reporting the best and worst sample. The best is the steady state; the
// worst is a sample that had to refresh a cache.
package main

import (
	"context"
	"fmt"
	"time"

	"github.com/nelvixboy/eye-of-lotus/internal/collect"
	"github.com/nelvixboy/eye-of-lotus/internal/docker"
	"github.com/nelvixboy/eye-of-lotus/internal/systemd"
)

func timeIt(label string, opts collect.Options, runs int) {
	c, err := collect.New(opts)
	if err != nil {
		fmt.Println(label, "error:", err)
		return
	}
	ctx := context.Background()
	best, worst := time.Hour, time.Duration(0)
	total := time.Duration(0)
	for i := 0; i < runs; i++ {
		start := time.Now()
		snap := c.Collect(ctx)
		d := time.Since(start)
		total += d
		if d < best {
			best = d
		}
		if d > worst {
			worst = d
		}
		if i == runs-1 {
			fmt.Printf("  %-22s лучший %-9v средний %-9v худший %-9v (сокетов %d, юнитов %d, контейнеров %d)\n",
				label, best.Round(time.Millisecond), (total / time.Duration(runs)).Round(time.Millisecond),
				worst.Round(time.Millisecond), len(snap.Net.Listening), snap.Services.Systemd.Total,
				snap.Services.Docker.Total)
		}
		time.Sleep(2 * time.Second)
	}
}

func main() {
	base := collect.Options{TopProcs: 12}
	fmt.Println("=== полный сбор, как в панели ===")
	full := base
	full.Systemd = systemd.New("")
	full.Docker = docker.New("")
	timeIt("systemd + docker", full, 8)
}
