package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// Probe cache policy. A Kubernetes readiness probe is frequent, so the
// integration is not re-listed on every request. A success is cached for a
// short time; a failure backs off so an outage does not turn into a storm.
const (
	probeSuccessTTL = 20 * time.Second
	probeMinBackoff = 5 * time.Second
	probeMaxBackoff = 60 * time.Second
)

// Readiness check names reported by the toolbox and aggregated by the service.
const (
	CheckKubernetes = "kubernetes_mcp"
	CheckGitHubRead = "github_read"
)

// Check is one reported capability.
type Check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Reason string `json:"reason,omitempty"`
}

// Toolbox owns the integrations, builds the admitted tool registry, and caches
// the capability probes that readiness reports.
type Toolbox struct {
	Kubernetes *Kubernetes
	GitHub     *GitHub

	mu                   sync.Mutex
	registry             *Registry
	expires              time.Time
	backoff              time.Duration
	lastKubernetes       bool
	lastGitHub           bool
	lastReasonKubernetes string
	lastReasonGitHub     string
	probedAt             time.Time
}

// NewToolbox returns a toolbox over the two integrations.
func NewToolbox(kubernetes *Kubernetes, github *GitHub) *Toolbox {
	return &Toolbox{Kubernetes: kubernetes, GitHub: github}
}

// Tools returns the admitted tools, rebuilding the registry when the cached one
// expires. A partial outage keeps the tools of the integration that answers.
func (t *Toolbox) Tools(ctx context.Context) ([]Tool, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.registry != nil && time.Now().Before(t.expires) {
		return t.registry.Tools(), nil
	}
	return t.refreshLocked(ctx)
}

// Call executes one admitted tool.
func (t *Toolbox) Call(ctx context.Context, name string, arguments json.RawMessage) (Result, error) {
	t.mu.Lock()
	registry := t.registry
	expired := registry == nil || !time.Now().Before(t.expires)
	t.mu.Unlock()
	if expired {
		var err error
		if _, err = t.Tools(ctx); err != nil {
			return Result{}, err
		}
		t.mu.Lock()
		registry = t.registry
		t.mu.Unlock()
	}
	if registry == nil {
		return Result{}, fmt.Errorf("no MCP integration is available")
	}
	return registry.Call(ctx, name, arguments)
}

// Names returns the admitted tool names of the cached registry.
func (t *Toolbox) Names(ctx context.Context) []string {
	tools, err := t.Tools(ctx)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Name)
	}
	return names
}

// Probe reports the Kubernetes and GitHub capabilities. The result is cached
// with a bounded refresh and backoff.
func (t *Toolbox) Probe(ctx context.Context) []Check {
	t.mu.Lock()
	if !t.probedAt.IsZero() && time.Now().Before(t.probedAt) {
		checks := t.cachedChecksLocked()
		t.mu.Unlock()
		return checks
	}
	t.mu.Unlock()

	checks := t.probe(ctx)

	t.mu.Lock()
	defer t.mu.Unlock()
	failed := false
	for _, check := range checks {
		if !check.OK {
			failed = true
		}
	}
	delay := probeSuccessTTL
	if failed {
		if t.backoff == 0 {
			t.backoff = probeMinBackoff
		} else {
			t.backoff *= 2
		}
		if t.backoff > probeMaxBackoff {
			t.backoff = probeMaxBackoff
		}
		delay = t.backoff
	} else {
		t.backoff = 0
	}
	t.probedAt = time.Now().Add(delay)
	for _, check := range checks {
		switch check.Name {
		case CheckKubernetes:
			t.lastKubernetes = check.OK
			t.lastReasonKubernetes = check.Reason
		case CheckGitHubRead:
			t.lastGitHub = check.OK
			t.lastReasonGitHub = check.Reason
		}
	}
	return checks
}

func (t *Toolbox) cachedChecksLocked() []Check {
	return []Check{
		{Name: CheckKubernetes, OK: t.lastKubernetes, Reason: t.lastReasonKubernetes},
		{Name: CheckGitHubRead, OK: t.lastGitHub, Reason: t.lastReasonGitHub},
	}
}

// probe performs both capability checks without the cache.
func (t *Toolbox) probe(ctx context.Context) []Check {
	checks := make([]Check, 0, 2)
	if t.Kubernetes == nil {
		checks = append(checks, Check{Name: CheckKubernetes, Reason: "the Kubernetes integration is not configured"})
	} else if tools, err := t.Kubernetes.Tools(ctx); err != nil {
		checks = append(checks, Check{Name: CheckKubernetes,
			Reason: "the Kubernetes MCP server did not answer with admitted read tools"})
	} else {
		checks = append(checks, Check{Name: CheckKubernetes, OK: true,
			Reason: fmt.Sprintf("%d read tools admitted", len(tools))})
	}
	if t.GitHub == nil {
		checks = append(checks, Check{Name: CheckGitHubRead, Reason: "the GitHub integration is not configured"})
		return checks
	}
	if _, err := t.GitHub.Tools(ctx); err != nil {
		checks = append(checks, Check{Name: CheckGitHubRead,
			Reason: "the GitHub MCP server did not answer with admitted read tools"})
		return checks
	}
	result, err := t.GitHub.Call(ctx, "github_repository", json.RawMessage(`{}`))
	if err != nil {
		checks = append(checks, Check{Name: CheckGitHubRead,
			Reason: "the configured repository could not be read"})
		return checks
	}
	if result.IsError {
		checks = append(checks, Check{Name: CheckGitHubRead,
			Reason: "the configured repository read was refused"})
		return checks
	}
	checks = append(checks, Check{Name: CheckGitHubRead, OK: true,
		Reason: "the configured repository is readable"})
	return checks
}

// refreshLocked rebuilds the registry. The caller holds t.mu.
func (t *Toolbox) refreshLocked(ctx context.Context) ([]Tool, error) {
	adapters := make(map[string]Adapter, 2)
	fetched := make(map[string][]Tool, 2)
	reasons := make([]string, 0, 2)
	if t.Kubernetes != nil {
		if tools, err := t.Kubernetes.Tools(ctx); err != nil {
			reasons = append(reasons, "kubernetes: "+err.Error())
		} else if len(tools) > 0 {
			adapters["kubernetes"] = t.Kubernetes
			fetched["kubernetes"] = tools
		}
	}
	if t.GitHub != nil {
		if tools, err := t.GitHub.Tools(ctx); err != nil {
			reasons = append(reasons, "github: "+err.Error())
		} else if len(tools) > 0 {
			adapters["github"] = t.GitHub
			fetched["github"] = tools
		}
	}
	if len(adapters) == 0 {
		t.expires = time.Now().Add(probeMinBackoff)
		if len(reasons) == 0 {
			reasons = append(reasons, "no integration is configured")
		}
		return nil, fmt.Errorf("admit the MCP tools: %v", reasons)
	}
	registry, err := newRegistry(adapters, fetched)
	if err != nil {
		t.expires = time.Now().Add(probeMinBackoff)
		return nil, err
	}
	t.registry = registry
	t.expires = time.Now().Add(probeSuccessTTL)
	return registry.Tools(), nil
}
