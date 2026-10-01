// Package tools provides a permission-aware tool registry for the AI agent.
package tools

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Permission defines who may invoke a tool.
type Permission string

const (
	PermEveryone Permission = "everyone"
	PermUser     Permission = "user"
	PermAdmin    Permission = "admin"
	PermDisabled Permission = "disabled"
)

// Tool describes a single executable capability exposed to the agent.
type Tool struct {
	Name        string
	Description string
	InputSchema string
	Timeout     time.Duration
	Permission  Permission
	Execute     func(ctx context.Context, userID, input string) (string, error)
}

// Registry holds the registered tools.
type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool
}

// NewRegistry creates an empty tool registry.
func NewRegistry() *Registry {
	return &Registry{
		tools: make(map[string]Tool),
	}
}

// Register adds or replaces a tool in the registry.
func (r *Registry) Register(t Tool) error {
	name := strings.ToLower(strings.TrimSpace(t.Name))
	if name == "" {
		return errors.New("tool name must not be empty")
	}
	if t.Execute == nil {
		return fmt.Errorf("tool %q has nil Execute function", name)
	}
	if t.Timeout <= 0 {
		t.Timeout = 10 * time.Second
	}
	if t.Permission == "" {
		t.Permission = PermUser
	}
	t.Name = name

	r.mu.Lock()
	r.tools[name] = t
	r.mu.Unlock()
	return nil
}

// Get retrieves a tool by name.
func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	t, ok := r.tools[strings.ToLower(strings.TrimSpace(name))]
	return t, ok
}

// ListForRole returns all enabled tools permitted for the caller's role.
func (r *Registry) ListForRole(role string) []Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]Tool, 0, len(r.tools))
	for _, t := range r.tools {
		if Allowed(t.Permission, role) {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Name < out[j].Name
	})
	return out
}

// Allowed reports whether a user with role ("ADMIN", "USER", "GUEST") may
// execute a tool with the given permission policy.
func Allowed(perm Permission, role string) bool {
	role = strings.ToUpper(strings.TrimSpace(role))
	switch perm {
	case PermDisabled:
		return false
	case PermAdmin:
		return role == "ADMIN"
	case PermUser:
		return role == "ADMIN" || role == "USER"
	case PermEveryone:
		return true
	default:
		return false
	}
}

// Run validates permissions, enforces the tool's timeout, and executes it.
func (r *Registry) Run(parentCtx context.Context, name, userID, role, input string) (string, error) {
	t, ok := r.Get(name)
	if !ok {
		return "", fmt.Errorf("unknown tool %q", name)
	}
	if !Allowed(t.Permission, role) {
		return "", fmt.Errorf("tool %q is not permitted for role %s", t.Name, role)
	}

	ctx, cancel := context.WithTimeout(parentCtx, t.Timeout)
	defer cancel()

	return t.Execute(ctx, userID, strings.TrimSpace(input))
}
