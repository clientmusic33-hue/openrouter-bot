// Package telemetry tracks operational metrics (requests, active concurrency,
// errors, model usage, estimated tokens, cost, and runtime memory) for /admin
// observability and structured logging.
package telemetry

import (
	"fmt"
	"log"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"openrouter-bot/internal/security"
)

// Collector aggregates bot-wide operational statistics safely across goroutines.
type Collector struct {
	started time.Time

	activeRequests atomic.Int64
	totalRequests  atomic.Int64
	errorCount     atomic.Int64
	tokensUsed     atomic.Int64

	mu         sync.RWMutex
	modelUsage map[string]int64
	totalCost  float64
}

// NewCollector creates a new telemetry collector.
func NewCollector() *Collector {
	return &Collector{
		started:    time.Now(),
		modelUsage: make(map[string]int64),
	}
}

// BeginRequest increments active and total request counters and returns a
// completion callback to call with the finished model, token estimate, and error.
func (c *Collector) BeginRequest() func(model string, approxTokens int, err error) {
	if c == nil {
		return func(string, int, error) {}
	}
	c.activeRequests.Add(1)
	c.totalRequests.Add(1)

	return func(model string, approxTokens int, err error) {
		c.activeRequests.Add(-1)
		if err != nil {
			c.errorCount.Add(1)
		}
		if approxTokens > 0 {
			c.tokensUsed.Add(int64(approxTokens))
		}
		model = strings.TrimSpace(model)
		if model != "" {
			c.mu.Lock()
			c.modelUsage[model]++
			c.mu.Unlock()
		}
	}
}

// AddCost records incurred API cost in USD.
func (c *Collector) AddCost(cost float64) {
	if c == nil || cost <= 0 {
		return
	}
	c.mu.Lock()
	c.totalCost += cost
	c.mu.Unlock()
}

// Snapshot is an immutable point-in-time view of all metrics.
type Snapshot struct {
	Uptime         time.Duration
	ActiveRequests int64
	TotalRequests  int64
	ErrorCount     int64
	ErrorRatePct   float64
	TokensUsed     int64
	TotalCostUSD   float64
	TopModels      []ModelCount
	HeapAllocMB    float64
	Goroutines     int
}

// ModelCount pairs a model identifier with its request count.
type ModelCount struct {
	Model string
	Count int64
}

// Snapshot returns the current metrics.
func (c *Collector) Snapshot() Snapshot {
	if c == nil {
		return Snapshot{}
	}

	total := c.totalRequests.Load()
	errs := c.errorCount.Load()
	var errRate float64
	if total > 0 {
		errRate = (float64(errs) / float64(total)) * 100
	}

	c.mu.RLock()
	cost := c.totalCost
	models := make([]ModelCount, 0, len(c.modelUsage))
	for m, cnt := range c.modelUsage {
		models = append(models, ModelCount{Model: m, Count: cnt})
	}
	c.mu.RUnlock()

	sort.Slice(models, func(i, j int) bool {
		if models[i].Count != models[j].Count {
			return models[i].Count > models[j].Count
		}
		return models[i].Model < models[j].Model
	})
	if len(models) > 5 {
		models = models[:5]
	}

	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)

	return Snapshot{
		Uptime:         time.Since(c.started).Round(time.Second),
		ActiveRequests: c.activeRequests.Load(),
		TotalRequests:  total,
		ErrorCount:     errs,
		ErrorRatePct:   errRate,
		TokensUsed:     c.tokensUsed.Load(),
		TotalCostUSD:   cost,
		TopModels:      models,
		HeapAllocMB:    float64(mem.Alloc) / (1024 * 1024),
		Goroutines:     runtime.NumGoroutine(),
	}
}

// LogEvent writes a structured key=value log entry with automatic secret redaction.
func LogEvent(event string, kv ...any) {
	var b strings.Builder
	b.WriteString("event=")
	b.WriteString(event)

	for i := 0; i+1 < len(kv); i += 2 {
		key := fmt.Sprint(kv[i])
		val := security.RedactSecrets(fmt.Sprint(kv[i+1]))
		b.WriteString(fmt.Sprintf(" %s=%q", key, val))
	}
	log.Println(b.String())
}
