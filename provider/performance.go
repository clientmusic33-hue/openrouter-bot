package provider

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// CircuitState represents the circuit breaker state of a provider.
type CircuitState string

const (
	CircuitClosed   CircuitState = "closed"
	CircuitOpen     CircuitState = "open"
	CircuitHalfOpen CircuitState = "half_open"
)

// ErrClass categorises an upstream error for retry and circuit breaker logic.
type ErrClass int

const (
	ErrClassNone ErrClass = iota
	ErrClassCanceled
	ErrClassTimeout
	ErrClassRateLimit
	ErrClassTransient
	ErrClassAuth
	ErrClassPermanent
)

var (
	sharedTransportOnce sync.Once
	sharedTransport     *http.Transport
	httpClientsMu       sync.Mutex
	httpClients         = make(map[time.Duration]*http.Client)
)

// SharedTransport returns a tuned HTTP transport with connection pooling.
func SharedTransport() *http.Transport {
	sharedTransportOnce.Do(func() {
		sharedTransport = &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   10 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          128,
			MaxIdleConnsPerHost:   24,
			MaxConnsPerHost:       64,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			ResponseHeaderTimeout: 45 * time.Second,
		}
	})

	return sharedTransport
}

// SharedHTTPClient returns a pooled HTTP client for the given timeout.
func SharedHTTPClient(timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = 60 * time.Second
	}

	httpClientsMu.Lock()
	defer httpClientsMu.Unlock()

	if client, ok := httpClients[timeout]; ok {
		return client
	}

	client := &http.Client{
		Transport: SharedTransport(),
		Timeout:   timeout,
	}
	httpClients[timeout] = client

	return client
}

// ClassifyError inspects an error from an OpenAI-compatible provider and
// classifies it so callers know whether to retry, fail over, or trip cooldown.
func ClassifyError(err error) ErrClass {
	if err == nil {
		return ErrClassNone
	}

	if errors.Is(err, context.Canceled) {
		return ErrClassCanceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return ErrClassTimeout
	}

	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return ErrClassTimeout
	}

	msg := strings.ToLower(err.Error())

	switch {
	case strings.Contains(msg, "context canceled"):
		return ErrClassCanceled
	case strings.Contains(msg, "deadline exceeded"),
		strings.Contains(msg, "timeout"),
		strings.Contains(msg, "timed out"),
		strings.Contains(msg, "status 408"),
		strings.Contains(msg, "status 504"),
		strings.Contains(msg, "status code: 408"),
		strings.Contains(msg, "status code: 504"):
		return ErrClassTimeout
	case strings.Contains(msg, "429"),
		strings.Contains(msg, "rate limit"),
		strings.Contains(msg, "rate_limit"),
		strings.Contains(msg, "too many requests"),
		strings.Contains(msg, "quota"):
		return ErrClassRateLimit
	case strings.Contains(msg, "status 401"),
		strings.Contains(msg, "status 403"),
		strings.Contains(msg, "status code: 401"),
		strings.Contains(msg, "status code: 403"),
		strings.Contains(msg, "unauthorized"),
		strings.Contains(msg, "invalid api key"),
		strings.Contains(msg, "invalid_api_key"),
		strings.Contains(msg, "authentication"):
		return ErrClassAuth
	case strings.Contains(msg, "status 400"),
		strings.Contains(msg, "status 404"),
		strings.Contains(msg, "status 422"),
		strings.Contains(msg, "status code: 400"),
		strings.Contains(msg, "status code: 404"),
		strings.Contains(msg, "status code: 422"),
		strings.Contains(msg, "invalid_request_error"),
		strings.Contains(msg, "model_not_found"),
		strings.Contains(msg, "does not exist"):
		return ErrClassPermanent
	case strings.Contains(msg, "status 500"),
		strings.Contains(msg, "status 502"),
		strings.Contains(msg, "status 503"),
		strings.Contains(msg, "status code: 500"),
		strings.Contains(msg, "status code: 502"),
		strings.Contains(msg, "status code: 503"),
		strings.Contains(msg, "overloaded"),
		strings.Contains(msg, "connection reset"),
		strings.Contains(msg, "broken pipe"),
		strings.Contains(msg, "eof"),
		strings.Contains(msg, "empty completion"):
		return ErrClassTransient
	default:
		return ErrClassTransient
	}
}

// IsRetryableError reports whether the same candidate can be retried after a
// short backoff. Permanent, auth, rate-limit (better to failover immediately),
// and canceled errors are not retried on the same candidate.
func IsRetryableError(err error) bool {
	class := ClassifyError(err)

	return class == ErrClassTransient || class == ErrClassTimeout
}

// IsPermanentError reports whether the error is non-retryable on the same model
// (such as invalid request parameters, 404 model not found, or 401/403).
func IsPermanentError(err error) bool {
	class := ClassifyError(err)

	return class == ErrClassPermanent || class == ErrClassAuth
}

// IsTimeoutError reports whether the error was caused by a timeout.
func IsTimeoutError(err error) bool {
	return ClassifyError(err) == ErrClassTimeout
}

// IsRateLimitError reports whether the error was a 429 / rate limit error.
func IsRateLimitError(err error) bool {
	return ClassifyError(err) == ErrClassRateLimit
}

// BackoffDuration computes exponential backoff capped at maxDelay.
func BackoffDuration(attempt int, baseDelay, maxDelay time.Duration) time.Duration {
	if baseDelay <= 0 {
		baseDelay = 150 * time.Millisecond
	}
	if maxDelay <= 0 {
		maxDelay = 2 * time.Second
	}
	if attempt <= 0 {
		return baseDelay
	}
	if attempt > 10 {
		attempt = 10
	}

	d := baseDelay << attempt
	if d > maxDelay || d <= 0 {
		return maxDelay
	}

	return d
}

// RetryWithBackoff executes fn up to maxAttempts times when it returns a
// retryable error, sleeping with exponential backoff between attempts.
func RetryWithBackoff(
	ctx context.Context,
	maxAttempts int,
	baseDelay time.Duration,
	fn func() error,
) error {
	if maxAttempts <= 1 {
		return fn()
	}

	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		err := fn()
		if err == nil {
			return nil
		}
		lastErr = err

		if !IsRetryableError(err) || attempt == maxAttempts-1 {
			return err
		}

		delay := BackoffDuration(attempt, baseDelay, 2*time.Second)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}

	return lastErr
}

// LatencyStats tracks exponentially weighted moving average latency.
type LatencyStats struct {
	Samples int64
	Avg     time.Duration
	Last    time.Duration
	Min     time.Duration
	Max     time.Duration
}

// Record updates the moving average with a new sample.
func (s *LatencyStats) Record(d time.Duration) {
	if d <= 0 {
		return
	}
	s.Samples++
	s.Last = d
	if s.Min == 0 || d < s.Min {
		s.Min = d
	}
	if d > s.Max {
		s.Max = d
	}
	if s.Samples == 1 {
		s.Avg = d
		return
	}
	// EWMA with alpha = 0.3
	s.Avg = time.Duration(float64(s.Avg)*0.7 + float64(d)*0.3)
}
