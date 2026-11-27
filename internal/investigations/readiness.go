package investigations

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/a-hagi613/pagerless/internal/mcp"
	"github.com/a-hagi613/pagerless/internal/redact"
)

// Readiness check names. The agent owns the model check; the rest are verified
// by this service.
const (
	CheckTranscript    = "transcript"
	CheckAgent         = "agent"
	CheckModel         = "model"
	CheckGitHubPublish = "github_publish"
)

// Readiness cache policy. Probes are active, so they are cached; a failure
// backs off rather than turning a Kubernetes probe into a request storm.
const (
	readySuccessTTL = 15 * time.Second
	readyMinBackoff = 5 * time.Second
	readyMaxBackoff = 60 * time.Second
	// probeBudget bounds one complete readiness probe.
	probeBudget = 6 * time.Second
)

// Check is one capability the installation needs.
type Check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Reason string `json:"reason,omitempty"`
}

// Readiness is the GET /readyz body. The shape matches the agent's body so an
// operator reads one format everywhere.
type Readiness struct {
	Status  string  `json:"status"`
	Version string  `json:"version,omitempty"`
	Checks  []Check `json:"checks"`
}

type readinessCache struct {
	mu      sync.Mutex
	checks  []Check
	expires time.Time
	backoff time.Duration
	probing bool
	wait    chan struct{}
}

// Checks returns the cached readiness checks, probing when the cache expired.
// A concurrent caller waits for an in-flight probe instead of starting another.
func (s *Service) Checks(ctx context.Context) []Check {
	s.readiness.mu.Lock()
	if len(s.readiness.checks) > 0 && time.Now().Before(s.readiness.expires) {
		checks := append([]Check(nil), s.readiness.checks...)
		s.readiness.mu.Unlock()
		return checks
	}
	if s.readiness.probing {
		wait := s.readiness.wait
		s.readiness.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			return degradedUnavailable()
		}
		s.readiness.mu.Lock()
		checks := append([]Check(nil), s.readiness.checks...)
		s.readiness.mu.Unlock()
		return checks
	}
	s.readiness.probing = true
	s.readiness.wait = make(chan struct{})
	wait := s.readiness.wait
	s.readiness.mu.Unlock()

	probeContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), probeBudget)
	checks := s.probe(probeContext)
	cancel()

	s.readiness.mu.Lock()
	failed := false
	for _, check := range checks {
		if !check.OK {
			failed = true
		}
	}
	delay := readySuccessTTL
	if failed {
		if s.readiness.backoff == 0 {
			s.readiness.backoff = readyMinBackoff
		} else {
			s.readiness.backoff *= 2
			if s.readiness.backoff > readyMaxBackoff {
				s.readiness.backoff = readyMaxBackoff
			}
		}
		delay = s.readiness.backoff
	} else {
		s.readiness.backoff = 0
	}
	s.readiness.checks = checks
	s.readiness.expires = time.Now().Add(delay)
	s.readiness.probing = false
	close(wait)
	s.readiness.mu.Unlock()
	return append([]Check(nil), checks...)
}

// Ready renders the readiness document.
func (s *Service) Ready(ctx context.Context) Readiness {
	checks := s.Checks(ctx)
	status := "ready"
	for _, check := range checks {
		if !check.OK {
			status = "degraded"
			break
		}
	}
	return Readiness{Status: status, Version: s.cfg.Version, Checks: checks}
}

// ReadyGate refuses admission with a sanitized list of the failing checks.
func (s *Service) ReadyGate(ctx context.Context) error {
	checks := s.Checks(ctx)
	failed := make([]string, 0, len(checks))
	for _, check := range checks {
		if !check.OK {
			failed = append(failed, check.Name)
		}
	}
	if len(failed) == 0 {
		return nil
	}
	return fmt.Errorf("the installation is not ready: %s", strings.Join(failed, ", "))
}

// probe performs the active checks. It never prints a credential and never
// creates a pull request.
func (s *Service) probe(ctx context.Context) []Check {
	checks := make([]Check, 0, 6)
	storeContext, cancel := context.WithTimeout(ctx, 2*time.Second)
	err := s.store.Ping(storeContext)
	cancel()
	if err != nil {
		checks = append(checks, Check{Name: CheckTranscript, Reason: "the transcript database did not answer"})
	} else {
		checks = append(checks, Check{Name: CheckTranscript, OK: true, Reason: "the transcript database answers"})
	}
	checks = append(checks, s.probeAgent(ctx)...)
	if s.cfg.Toolbox == nil {
		checks = append(checks,
			Check{Name: mcp.CheckKubernetes, Reason: "the Kubernetes integration is not configured"},
			Check{Name: mcp.CheckGitHubRead, Reason: "the GitHub integration is not configured"})
	} else {
		for _, check := range s.cfg.Toolbox.Probe(ctx) {
			checks = append(checks, Check{Name: check.Name, OK: check.OK, Reason: check.Reason})
		}
	}
	checks = append(checks, s.probePublish(ctx))
	return checks
}

// probeAgent fetches the agent's own readiness, which owns the model check.
func (s *Service) probeAgent(ctx context.Context) []Check {
	if s.cfg.Agent == nil {
		return []Check{
			{Name: CheckAgent, Reason: "the investigation agent is not configured"},
			{Name: CheckModel, Reason: "the model capability is unknown"},
		}
	}
	agentContext, cancel := context.WithTimeout(ctx, 3*time.Second)
	readiness, err := s.cfg.Agent.Ready(agentContext)
	cancel()
	if err != nil {
		// A timeout cannot prove the endpoint is reachable, so both checks are
		// unverified rather than partly green.
		return []Check{
			{Name: CheckAgent, Reason: "the investigation agent did not answer"},
			{Name: CheckModel, Reason: "the model capability could not be verified"},
		}
	}
	checks := []Check{{Name: CheckAgent, OK: true, Reason: "the investigation agent answers"}}
	foundModel := false
	for _, reported := range readiness.Checks {
		if strings.TrimSpace(reported.Name) == "" {
			continue
		}
		if reported.Name == CheckModel {
			foundModel = true
		}
		checks = append(checks, Check{
			Name: reported.Name, OK: reported.OK, Reason: sanitizeReadiness(reported.Reason),
		})
	}
	if !foundModel {
		checks = append(checks, Check{Name: CheckModel, Reason: "the agent did not report a model check"})
	}
	return checks
}

// probePublish verifies the publishing credential against the repository
// without opening a pull request.
func (s *Service) probePublish(ctx context.Context) Check {
	if s.cfg.PublishCheck == nil {
		return Check{Name: CheckGitHubPublish, Reason: "the publishing path is not configured"}
	}
	publishContext, cancel := context.WithTimeout(ctx, 3*time.Second)
	reason, err := s.cfg.PublishCheck(publishContext)
	cancel()
	if err != nil {
		return Check{Name: CheckGitHubPublish,
			Reason: "the repository could not be reached with the publishing credential"}
	}
	if strings.TrimSpace(reason) == "" {
		reason = "the repository is reachable with the publishing credential"
	}
	return Check{Name: CheckGitHubPublish, OK: true, Reason: sanitizeReadiness(reason)}
}

func sanitizeReadiness(reason string) string {
	clean, _ := redact.Text(reason)
	clean = strings.TrimSpace(clean)
	runes := []rune(clean)
	if len(runes) > 200 {
		return string(runes[:197]) + "..."
	}
	return clean
}

func degradedUnavailable() []Check {
	return []Check{{Name: "readiness", Reason: "the readiness probe did not settle"}}
}
