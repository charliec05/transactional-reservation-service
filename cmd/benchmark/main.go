package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/charliec05/transactional-reservation-service/internal/reservation"
)

type result struct {
	Requests          int     `json:"requests"`
	Workers           int     `json:"workers"`
	Successes         int64   `json:"successes"`
	Failures          int64   `json:"failures"`
	InvariantFailures int64   `json:"invariant_failures"`
	ElapsedMS         float64 `json:"elapsed_ms"`
	RequestsPerSecond float64 `json:"requests_per_second"`
	P50Microseconds   int64   `json:"p50_microseconds"`
	P95Microseconds   int64   `json:"p95_microseconds"`
	P99Microseconds   int64   `json:"p99_microseconds"`
}

func main() {
	requests := flag.Int("requests", 10000, "number of hold-and-checkout operations")
	workers := flag.Int("workers", 64, "number of concurrent workers")
	flag.Parse()
	if *requests <= 0 || *workers <= 0 {
		fmt.Fprintln(os.Stderr, "requests and workers must be positive")
		os.Exit(2)
	}

	store, err := reservation.NewStore("", nil)
	if err != nil {
		panic(err)
	}
	if _, err := store.AddResource("benchmark", *requests); err != nil {
		panic(err)
	}

	jobs := make(chan int)
	latencies := make([]time.Duration, *requests)
	var successes atomic.Int64
	var failures atomic.Int64
	var wait sync.WaitGroup
	start := time.Now()
	for worker := 0; worker < *workers; worker++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for index := range jobs {
				operationStart := time.Now()
				hold, _, err := store.CreateHold(reservation.CreateHoldRequest{
					ResourceID:     "benchmark",
					Quantity:       1,
					TTL:            time.Minute,
					IdempotencyKey: fmt.Sprintf("request-%d", index),
				})
				if err == nil {
					_, err = store.Checkout(hold.ID)
				}
				latencies[index] = time.Since(operationStart)
				if err != nil {
					failures.Add(1)
				} else {
					successes.Add(1)
				}
			}
		}()
	}
	for index := 0; index < *requests; index++ {
		jobs <- index
	}
	close(jobs)
	wait.Wait()
	elapsed := time.Since(start)

	var invariantFailures int64
	if err := store.CheckInvariants(); err != nil {
		invariantFailures = 1
		fmt.Fprintln(os.Stderr, err)
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	output := result{
		Requests:          *requests,
		Workers:           *workers,
		Successes:         successes.Load(),
		Failures:          failures.Load(),
		InvariantFailures: invariantFailures,
		ElapsedMS:         float64(elapsed.Microseconds()) / 1000,
		RequestsPerSecond: float64(*requests) / elapsed.Seconds(),
		P50Microseconds:   percentile(latencies, 0.50).Microseconds(),
		P95Microseconds:   percentile(latencies, 0.95).Microseconds(),
		P99Microseconds:   percentile(latencies, 0.99).Microseconds(),
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(output)
}

func percentile(values []time.Duration, fraction float64) time.Duration {
	if len(values) == 0 {
		return 0
	}
	index := int(float64(len(values)-1) * fraction)
	return values[index]
}
