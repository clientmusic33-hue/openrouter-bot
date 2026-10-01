package cache

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

func TestResponseCacheKeyAndTTL(t *testing.T) {
	rc := NewResponseCache(4, 100*time.Millisecond, nil)

	k1 := BuildKey(KeyInput{Model: "m1", SystemPrompt: "sys", Temperature: 0.2, Prompt: "What is 2+2?"})
	k2 := BuildKey(KeyInput{Model: "m2", SystemPrompt: "sys", Temperature: 0.2, Prompt: "What is 2+2?"})

	if k1 == k2 {
		t.Fatal("different models must produce distinct cache keys")
	}

	rc.Set(k1, "4")
	if got, ok := rc.Get(k1); !ok || got != "4" {
		t.Fatalf("Get(k1) = %q, %v; want 4, true", got, ok)
	}

	time.Sleep(130 * time.Millisecond)
	if _, ok := rc.Get(k1); ok {
		t.Fatal("expired entry should not be returned")
	}
}

func TestIsCacheableFiltersSensitiveAndStateful(t *testing.T) {
	if !IsCacheable("Explain binary search in Go", 0, 0.5) {
		t.Error("expected stateless coding question to be cacheable")
	}
	if IsCacheable("Explain binary search", 3, 0.5) {
		t.Error("multi-turn conversation should not be cached")
	}
	if IsCacheable("My password is secret123", 0, 0.5) {
		t.Error("sensitive prompt containing password must not be cached")
	}
	if IsCacheable("Write a poem", 0, 0.95) {
		t.Error("high-temperature prompt should not be cached")
	}
}

func TestRedisClientWithMockServer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()

	store := make(map[string]string)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				r := bufio.NewReader(c)
				line, err := r.ReadString('\n')
				if err != nil {
					return
				}
				var count int
				fmt.Sscanf(line, "*%d", &count)
				args := make([]string, 0, count)
				for i := 0; i < count; i++ {
					szLine, _ := r.ReadString('\n')
					var sz int
					fmt.Sscanf(szLine, "$%d", &sz)
					buf := make([]byte, sz+2)
					_, _ = r.Read(buf)
					args = append(args, string(buf[:sz]))
				}
				if len(args) == 0 {
					return
				}
				switch strings.ToUpper(args[0]) {
				case "PING":
					_, _ = c.Write([]byte("+PONG\r\n"))
				case "SETEX":
					if len(args) >= 4 {
						store[args[1]] = args[3]
					}
					_, _ = c.Write([]byte("+OK\r\n"))
				case "GET":
					if val, ok := store[args[1]]; ok {
						_, _ = fmt.Fprintf(c, "$%d\r\n%s\r\n", len(val), val)
					} else {
						_, _ = c.Write([]byte("$-1\r\n"))
					}
				case "INCR":
					_, _ = c.Write([]byte(":1\r\n"))
				}
			}(conn)
		}
	}()

	client := NewRedisClient(ln.Addr().String(), "", 0)
	if err := client.Ping(); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if err := client.SetEX("k", "hello", 10*time.Second); err != nil {
		t.Fatalf("SetEX: %v", err)
	}
	if got, ok := client.Get("k"); !ok || got != "hello" {
		t.Fatalf("Get(k) = %q, %v; want hello, true", got, ok)
	}
}
