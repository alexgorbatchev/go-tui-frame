package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"syscall"
	"time"
)

func main() {
	cpus := flag.Int("cpus", runtime.NumCPU(), "number of worker goroutines to spin")
	timeout := flag.Duration("timeout", 60*time.Second, "maximum runtime before self-terminating")
	flag.Parse()

	runtime.GOMAXPROCS(*cpus)

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		select {
		case <-sigCh:
			cancel()
		case <-ctx.Done():
		}
	}()

	var wg sync.WaitGroup
	for i := 0; i < *cpus; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				default:
					for j := 0; j < 1_000_000; j++ {
					}
				}
			}
		}()
	}
	wg.Wait()
}
