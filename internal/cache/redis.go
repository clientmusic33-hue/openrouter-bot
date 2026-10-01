// Package cache provides bounded in-memory TTL caching, deterministic AI
// response caching, and an optional zero-dependency Redis RESP client.
package cache

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// RedisClient is a lightweight, zero-dependency Redis RESP2 client used when
// REDIS_URL or REDIS_ADDR is configured. When nil or unreachable, all callers
// fall back to in-memory structures automatically.
type RedisClient struct {
	addr     string
	password string
	db       int
	timeout  time.Duration
}

// NewRedisFromEnv constructs a RedisClient if REDIS_URL or REDIS_ADDR is set.
func NewRedisFromEnv() *RedisClient {
	rawURL := strings.TrimSpace(os.Getenv("REDIS_URL"))
	addr := strings.TrimSpace(os.Getenv("REDIS_ADDR"))
	password := os.Getenv("REDIS_PASSWORD")
	db := 0
	if dbStr := strings.TrimSpace(os.Getenv("REDIS_DB")); dbStr != "" {
		if parsed, err := strconv.Atoi(dbStr); err == nil && parsed >= 0 {
			db = parsed
		}
	}

	if rawURL != "" {
		if u, err := url.Parse(rawURL); err == nil && u.Host != "" {
			addr = u.Host
			if !strings.Contains(addr, ":") {
				addr += ":6379"
			}
			if u.User != nil {
				if p, ok := u.User.Password(); ok {
					password = p
				}
			}
			if trimmed := strings.TrimPrefix(u.Path, "/"); trimmed != "" {
				if parsed, err := strconv.Atoi(trimmed); err == nil && parsed >= 0 {
					db = parsed
				}
			}
		}
	}

	if addr == "" {
		return nil
	}
	if !strings.Contains(addr, ":") {
		addr += ":6379"
	}

	return NewRedisClient(addr, password, db)
}

// NewRedisClient creates a RedisClient for the given address, password, and DB.
func NewRedisClient(addr, password string, db int) *RedisClient {
	if strings.TrimSpace(addr) == "" {
		return nil
	}
	return &RedisClient{
		addr:     addr,
		password: password,
		db:       db,
		timeout:  2 * time.Second,
	}
}

func (r *RedisClient) exec(args ...string) (any, error) {
	if r == nil || r.addr == "" {
		return nil, errors.New("redis not configured")
	}

	conn, err := net.DialTimeout("tcp", r.addr, r.timeout)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(r.timeout))

	rw := bufio.NewReadWriter(bufio.NewReader(conn), bufio.NewWriter(conn))

	if r.password != "" {
		if err := writeRESPCommand(rw.Writer, "AUTH", r.password); err != nil {
			return nil, err
		}
		if _, err := readRESP(rw.Reader); err != nil {
			return nil, err
		}
	}

	if r.db > 0 {
		if err := writeRESPCommand(rw.Writer, "SELECT", strconv.Itoa(r.db)); err != nil {
			return nil, err
		}
		if _, err := readRESP(rw.Reader); err != nil {
			return nil, err
		}
	}

	if err := writeRESPCommand(rw.Writer, args...); err != nil {
		return nil, err
	}
	return readRESP(rw.Reader)
}

// Ping verifies connectivity to the Redis server.
func (r *RedisClient) Ping() error {
	resp, err := r.exec("PING")
	if err != nil {
		return err
	}
	if s, ok := resp.(string); ok && strings.EqualFold(s, "PONG") {
		return nil
	}
	return fmt.Errorf("unexpected PING response: %v", resp)
}

// Get retrieves a string value by key.
func (r *RedisClient) Get(key string) (string, bool) {
	resp, err := r.exec("GET", key)
	if err != nil || resp == nil {
		return "", false
	}
	s, ok := resp.(string)
	return s, ok
}

// SetEX stores a string value with a TTL.
func (r *RedisClient) SetEX(key, value string, ttl time.Duration) error {
	secs := int(ttl.Seconds())
	if secs <= 0 {
		secs = 60
	}
	_, err := r.exec("SETEX", key, strconv.Itoa(secs), value)
	return err
}

// Del removes a key.
func (r *RedisClient) Del(key string) error {
	_, err := r.exec("DEL", key)
	return err
}

// IncrWithTTL increments a counter key and sets its expiry on creation.
func (r *RedisClient) IncrWithTTL(key string, ttl time.Duration) (int64, error) {
	resp, err := r.exec("INCR", key)
	if err != nil {
		return 0, err
	}
	val, ok := resp.(int64)
	if !ok {
		return 0, errors.New("unexpected INCR reply")
	}
	if val == 1 && ttl > 0 {
		secs := int(ttl.Seconds())
		if secs <= 0 {
			secs = 60
		}
		_, _ = r.exec("EXPIRE", key, strconv.Itoa(secs))
	}
	return val, nil
}

// AcquireLock attempts to acquire a distributed lock using SET key token NX PX.
func (r *RedisClient) AcquireLock(key, token string, ttl time.Duration) bool {
	ms := int(ttl.Milliseconds())
	if ms <= 0 {
		ms = 5000
	}
	resp, err := r.exec("SET", key, token, "NX", "PX", strconv.Itoa(ms))
	if err != nil {
		return false
	}
	s, ok := resp.(string)
	return ok && strings.EqualFold(s, "OK")
}

// ReleaseLock releases a distributed lock if it matches token.
func (r *RedisClient) ReleaseLock(key, token string) {
	if current, ok := r.Get(key); ok && current == token {
		_ = r.Del(key)
	}
}

func writeRESPCommand(w *bufio.Writer, args ...string) error {
	if _, err := fmt.Fprintf(w, "*%d\r\n", len(args)); err != nil {
		return err
	}
	for _, arg := range args {
		if _, err := fmt.Fprintf(w, "$%d\r\n%s\r\n", len(arg), arg); err != nil {
			return err
		}
	}
	return w.Flush()
}

func readRESP(r *bufio.Reader) (any, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
	if line == "" {
		return nil, errors.New("empty RESP reply")
	}

	prefix := line[0]
	payload := line[1:]

	switch prefix {
	case '+':
		return payload, nil
	case '-':
		return nil, errors.New(payload)
	case ':':
		return strconv.ParseInt(payload, 10, 64)
	case '$':
		length, err := strconv.Atoi(payload)
		if err != nil {
			return nil, err
		}
		if length < 0 {
			return nil, nil
		}
		buf := make([]byte, length+2)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		return string(buf[:length]), nil
	default:
		return nil, fmt.Errorf("unsupported RESP prefix %q", prefix)
	}
}
