package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"sort"
	"sync"
	"time"
)

// RequestRecord holds a single request result.
type RequestRecord struct {
	Timestamp time.Time     // when request started
	Latency   time.Duration // round-trip time
	Status    int           // HTTP status code (0 = connection error)
}

// LoadResult holds all recorded requests from a run.
type LoadResult struct {
	Records   []RequestRecord
	StartTime time.Time
	EndTime   time.Time
	TargetQPS int
}

// LoadGen runs concurrent HTTP load at a target QPS.
type LoadGen struct {
	target      string
	qps         int
	concurrency int
	client      *http.Client
}

// NewLoadGen creates a load generator targeting the given URL.
func NewLoadGen(target string, qps int, concurrency int) *LoadGen {
	return &LoadGen{
		target:      target,
		qps:         qps,
		concurrency: concurrency,
		client: &http.Client{
			Timeout: 5 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
			Transport: &http.Transport{
				MaxIdleConnsPerHost: concurrency,
			},
		},
	}
}

// Run sends requests at target QPS until ctx is cancelled.
func (lg *LoadGen) Run(ctx context.Context) *LoadResult {
	result := &LoadResult{
		StartTime: time.Now(),
		TargetQPS: lg.qps,
	}

	// Per-worker record slices to avoid mutex contention
	type workerRecords struct {
		mu      sync.Mutex
		records []RequestRecord
	}
	workers := make([]workerRecords, lg.concurrency)

	// Work channel — buffered to allow ticker to enqueue without blocking
	work := make(chan struct{}, lg.concurrency*2)

	var wg sync.WaitGroup
	for i := 0; i < lg.concurrency; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			for range work {
				ts := time.Now()
				status := 0
				resp, err := lg.client.Get(lg.target)
				latency := time.Since(ts)
				if err == nil {
					status = resp.StatusCode
					resp.Body.Close()
				}
				workers[idx].mu.Lock()
				workers[idx].records = append(workers[idx].records, RequestRecord{
					Timestamp: ts,
					Latency:   latency,
					Status:    status,
				})
				workers[idx].mu.Unlock()
			}
		}(i)
	}

	// Ticker paces requests at target QPS
	ticker := time.NewTicker(time.Second / time.Duration(lg.qps))
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			close(work)
			wg.Wait()
			result.EndTime = time.Now()
			// Merge all worker records
			for i := range workers {
				result.Records = append(result.Records, workers[i].records...)
			}
			// Sort by timestamp for consistent output
			sort.Slice(result.Records, func(a, b int) bool {
				return result.Records[a].Timestamp.Before(result.Records[b].Timestamp)
			})
			return result
		case <-ticker.C:
			select {
			case work <- struct{}{}:
			default:
				// Workers are saturated — skip this tick
			}
		}
	}
}

// TotalRequests returns the total number of requests made.
func (r *LoadResult) TotalRequests() int {
	return len(r.Records)
}

// SuccessRate returns the fraction of status==200 requests.
func (r *LoadResult) SuccessRate() float64 {
	if len(r.Records) == 0 {
		return 0
	}
	ok := 0
	for i := range r.Records {
		if r.Records[i].Status == 200 {
			ok++
		}
	}
	return float64(ok) / float64(len(r.Records))
}

// ErrorCount returns the number of non-200 responses.
func (r *LoadResult) ErrorCount() int {
	errs := 0
	for i := range r.Records {
		if r.Records[i].Status != 200 {
			errs++
		}
	}
	return errs
}

// StatusCounts returns a map of status code → count.
func (r *LoadResult) StatusCounts() map[int]int {
	counts := make(map[int]int)
	for i := range r.Records {
		counts[r.Records[i].Status]++
	}
	return counts
}

// GetPercentile returns the latency at the given percentile (0-100).
func (r *LoadResult) GetPercentile(pct float64) time.Duration {
	if len(r.Records) == 0 {
		return 0
	}
	latencies := make([]time.Duration, len(r.Records))
	for i := range r.Records {
		latencies[i] = r.Records[i].Latency
	}
	sort.Slice(latencies, func(a, b int) bool { return latencies[a] < latencies[b] })
	idx := int(float64(len(latencies)-1) * pct / 100.0)
	return latencies[idx]
}

// ActualQPS returns total requests divided by duration.
func (r *LoadResult) ActualQPS() float64 {
	d := r.EndTime.Sub(r.StartTime).Seconds()
	if d == 0 {
		return 0
	}
	return float64(len(r.Records)) / d
}

// WriteCSV saves per-request records as CSV: start_ms,duration_us,error
// The swap offset is written as a comment on the first line for R to parse.
func (r *LoadResult) WriteCSV(path string, swapOffset time.Duration) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create csv: %w", err)
	}
	defer f.Close()

	fmt.Fprintf(f, "# swap_ms=%d\n", swapOffset.Milliseconds())
	fmt.Fprintln(f, "start_ms,duration_us,error")
	for i := range r.Records {
		tMs := r.Records[i].Timestamp.Sub(r.StartTime).Milliseconds()
		latUs := r.Records[i].Latency.Microseconds()
		errStr := "none"
		switch {
		case r.Records[i].Status == 200:
			// errStr stays "none"
		case r.Records[i].Status == 0:
			errStr = "conn"
		default:
			errStr = fmt.Sprintf("%d", r.Records[i].Status)
		}
		fmt.Fprintf(f, "%d,%d,%s\n", tMs, latUs, errStr)
	}
	return nil
}
