// Package investigations is the investigation service: admission, turns,
// durable history, ordered events, tool callbacks and the publication glue.
//
// One HTTP API serves the Go CLI and the embedded chat interface. The Python
// agent is a private down-stream service; it never reaches the public API.
package investigations

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/a-hagi613/pagerless/internal/agentclient"
	"github.com/a-hagi613/pagerless/internal/githubpublish"
	"github.com/a-hagi613/pagerless/internal/mcp"
	"github.com/a-hagi613/pagerless/internal/redact"
	"github.com/a-hagi613/pagerless/internal/transcript"
)

// Config is the service's fixed configuration.
type Config struct {
	// Version is reported by the API and the readiness body.
	Version string
	// Agent is the private Python client. A nil agent refuses every turn.
	Agent *agentclient.Client
	// Toolbox is the admitted MCP tool set. A nil toolbox admits no tool.
	Toolbox *mcp.Toolbox
	// Publisher is the isolated GitHub write path. A nil publisher rejects a
	// proposal without failing the answer.
	Publisher *githubpublish.Publisher
	// CallbackOrigin is the exact origin the agent may call. It carries no
	// path, query or fragment.
	CallbackOrigin string
	// PublishCheck verifies the publishing credential against the configured
	// repository without opening a pull request.
	PublishCheck func(context.Context) (string, error)
	// Gate refuses admission while a required capability is missing. A nil
	// gate admits every request.
	Gate func(context.Context) error

	Retention      time.Duration
	MaxActiveTurns int
	TurnTimeout    time.Duration
	StopGrace      time.Duration
	ToolTimeout    time.Duration
	MaxToolCalls   int
	MaxResultBytes int
	// Conversation bounds keep one request inside the agent's context window.
	MaxConversationMessages int
	MaxConversationBytes    int
	// Limits are passed down and may only lower the agent's own budgets.
	BudgetToolCalls   int
	BudgetModelCalls  int
	BudgetInputTokens int
	BudgetSeconds     int
}

// Wire and internal constants shared by the package.
const (
	codeUnauthorized = "unauthorized"
	roleUser         = "user"
	roleAssistant    = "assistant"
	keyError         = "error"
	keyStatus        = "status"
	stopStopped      = "stopped"
	stopFinished     = "finished"
	stopUnknown      = "unknown"
)

// Defaults returns the documented defaults for an unset field.
func (c Config) withDefaults() Config {
	if c.Retention <= 0 {
		c.Retention = 7 * 24 * time.Hour
	}
	if c.MaxActiveTurns <= 0 {
		c.MaxActiveTurns = 4
	}
	if c.TurnTimeout <= 0 {
		c.TurnTimeout = 10 * time.Minute
	}
	if c.StopGrace <= 0 {
		c.StopGrace = 10 * time.Second
	}
	if c.ToolTimeout <= 0 {
		c.ToolTimeout = 30 * time.Second
	}
	if c.MaxToolCalls <= 0 {
		c.MaxToolCalls = 16
	}
	if c.MaxResultBytes <= 0 {
		c.MaxResultBytes = 64 << 10
	}
	if c.MaxConversationMessages <= 0 {
		c.MaxConversationMessages = 60
	}
	if c.MaxConversationBytes <= 0 {
		c.MaxConversationBytes = 512 << 10
	}
	if c.BudgetToolCalls <= 0 {
		c.BudgetToolCalls = c.MaxToolCalls
	}
	if c.BudgetModelCalls <= 0 {
		c.BudgetModelCalls = 8
	}
	if c.BudgetSeconds <= 0 {
		c.BudgetSeconds = int(c.TurnTimeout / time.Second)
	}
	return c
}

// Error is a request failure with an HTTP status and a stable code.
type Error struct {
	HTTPStatus int
	Code       string
	Message    string
}

// Error renders the message.
func (e *Error) Error() string { return e.Code + ": " + e.Message }

func invalid(message string) *Error {
	return &Error{HTTPStatus: 400, Code: "invalid_request", Message: message}
}

func notFound(message string) *Error {
	return &Error{HTTPStatus: 404, Code: "not_found", Message: message}
}

func conflict(code, message string) *Error {
	return &Error{HTTPStatus: 409, Code: code, Message: message}
}

func unavailable(message string) *Error {
	return &Error{HTTPStatus: 503, Code: "not_ready", Message: message}
}

func internal(message string) *Error {
	return &Error{HTTPStatus: 500, Code: "internal_error", Message: message}
}

// Target is the client target hint.
type Target struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
}

// Request is the shared create and reply body.
type Request struct {
	Message   string  `json:"message"`
	Namespace string  `json:"namespace,omitempty"`
	Target    *Target `json:"target,omitempty"`
}

// Created is the create and reply response.
type Created struct {
	PageID string `json:"pageId"`
	TurnID string `json:"turnId"`
}

// Service implements the investigation API.
type Service struct {
	cfg   Config
	store *transcript.Store

	// admitMu serializes admission so capacity and idempotency cannot race.
	admitMu sync.Mutex
	// mu guards runs and the readiness cache.
	mu     sync.Mutex
	runs   map[string]*run
	closed bool

	readiness readinessCache
}

// run is one executing turn.
type run struct {
	turn transcript.Turn
	ctx  context.Context
	stop context.CancelFunc
	done chan struct{}

	mu sync.Mutex
	// outcome is the terminal status observed by a stop caller.
	outcome string
	// stopped is true once an operator asked to stop.
	stopped bool
	// stopStatus is the agent's answer to the stop request.
	stopStatus string
}

// New builds the service and marks every turn a previous process left running
// as interrupted.
func New(ctx context.Context, store *transcript.Store, config Config) (*Service, error) {
	if store == nil {
		return nil, errors.New("investigations: the service needs a transcript store")
	}
	cfg := config.withDefaults()
	if cfg.CallbackOrigin != "" {
		if err := validateCallbackOrigin(cfg.CallbackOrigin); err != nil {
			return nil, err
		}
	}
	service := &Service{cfg: cfg, store: store, runs: make(map[string]*run)}
	if _, err := store.InterruptRunningTurns(ctx); err != nil {
		return nil, fmt.Errorf("mark the interrupted turns: %w", err)
	}
	return service, nil
}

// Store exposes the transcript for the HTTP history handlers in tests.
func (s *Service) Store() *transcript.Store { return s.store }

// CreatePage admits a new investigation.
func (s *Service) CreatePage(ctx context.Context, idempotencyKey string, request Request) (Created, error) {
	if err := validateKey(idempotencyKey); err != nil {
		return Created{}, err
	}
	message, namespace, target, validErr := validateRequest(request)
	if validErr != nil {
		return Created{}, validErr
	}
	hash := requestHash(request)

	s.admitMu.Lock()
	defer s.admitMu.Unlock()
	if response, stored, found, err := s.store.Idempotent(ctx, idempotencyKey, "create", ""); err != nil {
		return Created{}, internal("The idempotency record could not be read.")
	} else if found {
		if stored != hash {
			return Created{}, conflict("idempotency_conflict", "The Idempotency-Key was already used for a different request.")
		}
		var created Created
		if err := json.Unmarshal(response, &created); err != nil {
			return Created{}, internal("The stored response could not be read.")
		}
		return created, nil
	}
	if err := s.admit(ctx); err != nil {
		return Created{}, err
	}
	now := time.Now().UTC()
	page := transcript.Page{
		ID: transcript.NewID(), Title: titleOf(message), Status: transcript.StatusRunning,
		Namespace: namespace, Target: target, CreatedAt: now, UpdatedAt: now,
	}
	turn := transcript.Turn{
		ID: transcript.NewID(), PageID: page.ID, Status: transcript.StatusRunning,
		CreatedAt: now, UpdatedAt: now, IdempotencyKey: idempotencyKey,
		Namespace: namespace, Target: target,
	}
	page.ActiveTurnID = turn.ID
	userMessage := transcript.Message{
		ID: transcript.NewID(), PageID: page.ID, TurnID: turn.ID, Role: roleUser,
		Content: message, CreatedAt: now,
	}
	if err := s.store.CreatePage(ctx, page, userMessage, turn); err != nil {
		return Created{}, internal("The page could not be stored.")
	}
	created := Created{PageID: page.ID, TurnID: turn.ID}
	s.startRun(page, turn)
	encoded, encodeErr := json.Marshal(created)
	if encodeErr != nil {
		return Created{}, internal("The response could not be encoded.")
	}
	if err := s.store.SaveIdempotent(ctx, idempotencyKey, "create", "", turn.ID, hash, encoded); err != nil {
		return Created{}, internal("The idempotency record could not be stored.")
	}
	return created, nil
}

// AddTurn admits a follow-up turn.
func (s *Service) AddTurn(ctx context.Context, pageID, idempotencyKey string, request Request) (Created, error) {
	if err := validateKey(idempotencyKey); err != nil {
		return Created{}, err
	}
	message, namespace, target, validErr := validateRequest(request)
	if validErr != nil {
		return Created{}, validErr
	}
	hash := requestHash(request)

	s.admitMu.Lock()
	defer s.admitMu.Unlock()
	page, err := s.store.Page(ctx, pageID)
	if err != nil {
		if errors.Is(err, transcript.ErrNotFound) {
			return Created{}, notFound("The page does not exist.")
		}
		return Created{}, internal("The page could not be read.")
	}
	resolvedNamespace, resolvedTarget, hintErr := resolveHints(page, namespace, request.Target, target)
	if hintErr != nil {
		return Created{}, hintErr
	}
	operation := "reply:" + pageID
	if response, stored, found, err := s.store.Idempotent(ctx, idempotencyKey, operation, pageID); err != nil {
		return Created{}, internal("The idempotency record could not be read.")
	} else if found {
		if stored != hash {
			return Created{}, conflict("idempotency_conflict", "The Idempotency-Key was already used for a different request.")
		}
		var created Created
		if err := json.Unmarshal(response, &created); err != nil {
			return Created{}, internal("The stored response could not be read.")
		}
		return created, nil
	}
	active, found, err := s.store.ActiveTurn(ctx, pageID)
	if err != nil {
		return Created{}, internal("The active turn could not be read.")
	}
	if found {
		if active.IdempotencyKey == idempotencyKey {
			return Created{}, conflict("turn_running", "The identical request is still running.")
		}
		return Created{}, conflict("page_busy", "The page already has a running turn.")
	}
	if err := s.admit(ctx); err != nil {
		return Created{}, err
	}
	now := time.Now().UTC()
	turn := transcript.Turn{
		ID: transcript.NewID(), PageID: pageID, Status: transcript.StatusRunning,
		CreatedAt: now, UpdatedAt: now, IdempotencyKey: idempotencyKey,
		Namespace: resolvedNamespace, Target: resolvedTarget,
	}
	userMessage := transcript.Message{
		ID: transcript.NewID(), PageID: pageID, TurnID: turn.ID, Role: roleUser,
		Content: message, CreatedAt: now,
	}
	if err := s.store.AddTurn(ctx, pageID, userMessage, turn, resolvedNamespace, resolvedTarget); err != nil {
		if errors.Is(err, transcript.ErrTurnInProgress) {
			return Created{}, conflict("page_busy", "The page already has a running turn.")
		}
		return Created{}, internal("The turn could not be stored.")
	}
	page.Namespace = resolvedNamespace
	page.Target = resolvedTarget
	created := Created{PageID: pageID, TurnID: turn.ID}
	s.startRun(page, turn)
	encoded, encodeErr := json.Marshal(created)
	if encodeErr != nil {
		return Created{}, internal("The response could not be encoded.")
	}
	if err := s.store.SaveIdempotent(ctx, idempotencyKey, operation, pageID, turn.ID, hash, encoded); err != nil {
		return Created{}, internal("The idempotency record could not be stored.")
	}
	return created, nil
}

// admit checks readiness and global capacity. The caller holds admitMu.
func (s *Service) admit(ctx context.Context) *Error {
	if s.cfg.Gate != nil {
		if err := s.cfg.Gate(ctx); err != nil {
			return unavailable(err.Error())
		}
	}
	if s.activeRuns() >= s.cfg.MaxActiveTurns {
		return &Error{HTTPStatus: 429, Code: "capacity_exhausted",
			Message: "The installation is investigating the maximum number of pages."}
	}
	return nil
}

func (s *Service) activeRuns() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.runs)
}

// startRun registers and starts one turn. The caller holds admitMu.
func (s *Service) startRun(page transcript.Page, turn transcript.Turn) {
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.TurnTimeout)
	run := &run{turn: turn, ctx: ctx, stop: cancel, done: make(chan struct{})}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		cancel()
		_ = s.store.FinishTurn(context.Background(), turn.PageID, turn.ID, transcript.StatusInterrupted,
			nil, nil, "interrupted", "The server stopped before the turn started.")
		return
	}
	s.runs[turn.ID] = run
	s.mu.Unlock()
	go s.execute(page, run)
}

// execute runs one turn to a terminal outcome.
func (s *Service) execute(page transcript.Page, current *run) {
	defer func() {
		current.stop()
		close(current.done)
		s.mu.Lock()
		delete(s.runs, current.turn.ID)
		s.mu.Unlock()
	}()
	messages, err := s.store.Messages(current.ctx, page.ID)
	if err != nil {
		s.fail(current, "store_unavailable", "The conversation could not be read.")
		return
	}
	tools := make([]mcp.Tool, 0)
	if s.cfg.Toolbox != nil {
		tools, err = s.cfg.Toolbox.Tools(current.ctx)
		if err != nil {
			s.fail(current, "tools_unavailable", "The investigation tools are unavailable.")
			return
		}
	}
	if s.cfg.Agent == nil {
		s.fail(current, "agent_unavailable", "The investigation agent is not configured.")
		return
	}
	request := agentclient.Request{
		PageID:      page.ID,
		TurnID:      current.turn.ID,
		Messages:    agentMessages(messages, s.cfg.MaxConversationMessages, s.cfg.MaxConversationBytes),
		Namespace:   current.turn.Namespace,
		Tools:       agentTools(tools),
		CallbackURL: s.cfg.CallbackOrigin,
		Limits: &agentclient.Limits{
			ToolCalls:   s.cfg.BudgetToolCalls,
			ModelCalls:  s.cfg.BudgetModelCalls,
			InputTokens: s.cfg.BudgetInputTokens,
			Seconds:     s.cfg.BudgetSeconds,
		},
	}
	if !current.turn.Target.Empty() {
		request.Target = &agentclient.Target{
			Kind: current.turn.Target.Kind, Name: current.turn.Target.Name,
			Namespace: current.turn.Namespace,
		}
	}
	terminal, err := s.cfg.Agent.StartTurn(current.ctx, request)
	if err != nil {
		s.handleAgentError(current, err)
		return
	}
	s.complete(current, page, terminal)
}

// handleAgentError records the conservative outcome of a failed turn. An
// ambiguous failure is interrupted: the service cannot prove the agent did not
// execute the turn, and re-running it silently could duplicate work.
func (s *Service) handleAgentError(current *run, err error) {
	if current.isStopped() {
		s.finishStopped(current)
		return
	}
	if errors.Is(current.ctx.Err(), context.DeadlineExceeded) {
		s.fail(current, "turn_timeout", "The turn exceeded its time limit.")
		return
	}
	if status, found := errors.AsType[*agentclient.StatusError](err); found {
		if status.Ambiguous {
			s.finishInterrupted(current, "the agent connection failed before its outcome was known")
			return
		}
		code := status.Code
		if code == "" {
			code = "agent_error"
		}
		s.fail(current, code, "The agent rejected the turn: "+sanitize(status.Message))
		return
	}
	s.finishInterrupted(current, "the turn ended without a confirmed outcome")
}

// finishStopped records a stop whose agent answer confirmed it, or an
// interrupted outcome when the agent could not confirm what happened.
func (s *Service) finishStopped(current *run) {
	status := transcript.StatusStopped
	code, message := "", ""
	if current.getStopStatus() != stopStopped {
		status = transcript.StatusInterrupted
		code = "interrupted"
		message = "The turn was stopped and its outcome could not be confirmed."
	}
	_ = s.store.FinishTurn(context.Background(), current.turn.PageID, current.turn.ID,
		status, nil, nil, code, message)
	current.setOutcome(status)
}

// fail records a definitive failure.
func (s *Service) fail(current *run, code, message string) {
	_ = s.store.FinishTurn(context.Background(), current.turn.PageID, current.turn.ID,
		transcript.StatusFailed, nil, nil, code, message)
	current.setOutcome(transcript.StatusFailed)
}

// finishInterrupted records an ambiguous turn.
func (s *Service) finishInterrupted(current *run, reason string) {
	_ = s.store.FinishTurn(context.Background(), current.turn.PageID, current.turn.ID,
		transcript.StatusInterrupted, nil, nil, "interrupted", reason)
	current.setOutcome(transcript.StatusInterrupted)
}

// complete persists the terminal answer and, when present, publishes the
// proposal.
func (s *Service) complete(current *run, page transcript.Page, terminal agentclient.Terminal) {
	status := transcript.StatusCompleted
	code, message := "", ""
	switch terminal.Status {
	case transcript.StatusCompleted:
	case transcript.StatusStopped:
		status = transcript.StatusStopped
	case transcript.StatusFailed:
		status = transcript.StatusFailed
		if terminal.Error != nil {
			code = terminal.Error.Code
			message = sanitize(terminal.Error.Message)
		}
		if code == "" {
			code = "agent_failed"
		}
	}
	evidence, err := s.store.EvidenceForTurn(context.Background(), page.ID, current.turn.ID)
	if err != nil {
		s.fail(current, "store_unavailable", "The evidence could not be read.")
		return
	}
	citations := validateCitations(terminal.Citations, evidence)
	events := make([]transcript.Event, 0, 2)
	if status == transcript.StatusCompleted && terminal.Proposal != nil && !current.isStopped() {
		events = append(events, s.publish(page, current, terminal.Proposal, terminal.Content, citations, evidence))
	}
	content, _ := redact.Text(terminal.Content)
	var stored *transcript.Message
	if strings.TrimSpace(content) != "" {
		var citationsWire []transcript.Citation
		for _, citation := range citations {
			citationsWire = append(citationsWire, transcript.Citation{
				CallID: citation.CallID, JSONPointer: citation.Pointer,
			})
		}
		stored = &transcript.Message{
			ID: transcript.NewID(), PageID: page.ID, TurnID: current.turn.ID,
			Role: roleAssistant, Content: content, CreatedAt: time.Now().UTC(),
			Citations: citationsWire,
		}
		events = append(events, transcript.Event{
			Type: transcript.EventAnswer,
			Data: mustJSON(map[string]any{
				"messageId": stored.ID, "role": roleAssistant, "content": stored.Content,
				"citations": citationsWire,
			}),
		})
	}
	if err := s.store.FinishTurn(context.Background(), page.ID, current.turn.ID, status, stored, events,
		code, message); err != nil {
		current.setOutcome(transcript.StatusFailed)
		return
	}
	current.setOutcome(status)
}

// publish validates and publishes one proposal, returning the proposal event.
// A rejected proposal never fails the answer: the investigation is complete and
// the operator can see why no pull request was opened.
func (s *Service) publish(
	page transcript.Page, current *run, proposal *agentclient.Proposal, findings string,
	citations []agentclient.Citation, evidence []transcript.Evidence,
) transcript.Event {
	now := time.Now().UTC()
	base := transcript.Proposal{
		PageID: page.ID, TurnID: current.turn.ID, BaseSHA: proposal.BaseSHA, CreatedAt: now,
	}
	files := make([]transcript.ProposedFile, 0, len(proposal.Files))
	reject := func(reason string) transcript.Event {
		base.Status = transcript.ProposalRejected
		base.Error = reason
		_ = s.store.SaveProposal(context.Background(), base)
		return transcript.Event{
			Type: transcript.EventProposal,
			Data: mustJSON(map[string]any{
				keyStatus: transcript.ProposalRejected, "baseSha": proposal.BaseSHA, keyError: reason,
			}),
		}
	}
	for _, file := range proposal.Files {
		_, count := redact.Text(file.Content)
		if count > 0 {
			return reject("The proposed change contains credential-shaped text.")
		}
		files = append(files, transcript.ProposedFile{Path: file.Path, Content: file.Content})
	}
	if s.cfg.Publisher == nil {
		return reject("GitHub publishing is not configured.")
	}
	question, _ := s.store.Messages(context.Background(), page.ID)
	questionText := firstQuestion(question)
	refs := make([]githubpublish.EvidenceRef, 0, len(citations))
	byCall := make(map[string]transcript.Evidence, len(evidence))
	for _, item := range evidence {
		byCall[item.CallID] = item
	}
	for _, citation := range citations {
		if item, found := byCall[citation.CallID]; found && len(refs) < 20 {
			refs = append(refs, githubpublish.EvidenceRef{CallID: citation.CallID, Tool: item.Tool})
		}
	}
	proposalFiles := make([]githubpublish.File, 0, len(proposal.Files))
	for _, file := range proposal.Files {
		proposalFiles = append(proposalFiles, githubpublish.File{Path: file.Path, Content: file.Content})
	}
	publishContext, cancel := context.WithTimeout(context.WithoutCancel(current.ctx), time.Minute)
	defer cancel()
	published, err := s.cfg.Publisher.Publish(publishContext, githubpublish.Proposal{
		PageID: page.ID, TurnID: current.turn.ID, Question: questionText,
		Findings: findings, Evidence: refs, BaseSHA: proposal.BaseSHA,
		Files: proposalFiles,
	})
	if err != nil {
		return reject(safePublishError(err))
	}
	base.Status = transcript.ProposalPublished
	base.Branch = published.Branch
	base.PRURL = published.PullRequest.URL
	base.Files = files
	base.Diff = make([]transcript.FileDiff, 0, len(published.Diff))
	for _, file := range published.Diff {
		base.Diff = append(base.Diff, transcript.FileDiff{Path: file.Path, Diff: file.Diff})
	}
	_ = s.store.SaveProposal(context.Background(), base)
	return transcript.Event{
		Type: transcript.EventProposal,
		Data: mustJSON(map[string]any{
			keyStatus: transcript.ProposalPublished, "baseSha": proposal.BaseSHA,
			"files": base.Diff, "prUrl": published.PullRequest.URL,
		}),
	}
}

// Stop requests cancellation of the page's active turn. It is safe to repeat.
func (s *Service) Stop(ctx context.Context, pageID string) (string, error) {
	if _, err := s.store.Page(ctx, pageID); err != nil {
		if errors.Is(err, transcript.ErrNotFound) {
			return "", notFound("The page does not exist.")
		}
		return "", internal("The page could not be read.")
	}
	s.mu.Lock()
	var current *run
	for _, candidate := range s.runs {
		if candidate.turn.PageID == pageID {
			current = candidate
			break
		}
	}
	if current == nil {
		s.mu.Unlock()
		return stopFinished, nil
	}
	current.requestStop()
	s.mu.Unlock()

	agentStatus := stopUnknown
	if s.cfg.Agent != nil {
		stopContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), agentclient.StopTimeout)
		status, err := s.cfg.Agent.StopTurn(stopContext, current.turn.ID)
		cancel()
		if err == nil {
			agentStatus = status
		}
	}
	current.setStopStatus(agentStatus)

	// The agent returns the original turn's terminal object after a stop. Wait
	// for it before cancelling the request, so a confirmed stop is recorded as
	// stopped rather than as an ambiguous interruption.
	select {
	case <-current.done:
	case <-time.After(s.cfg.StopGrace):
		current.stop()
		select {
		case <-current.done:
		case <-time.After(2 * time.Second):
			return stopUnknown, nil
		}
	}
	switch outcome := current.getOutcome(); outcome {
	case transcript.StatusCompleted:
		return stopFinished, nil
	case transcript.StatusStopped:
		return stopStopped, nil
	case transcript.StatusInterrupted:
		return stopUnknown, nil
	case transcript.StatusFailed:
		return stopFinished, nil
	default:
		return stopUnknown, nil
	}
}

// Close cancels every running turn. The startup recovery marks whatever the
// process could not settle as interrupted on the next start.
func (s *Service) Close() {
	s.mu.Lock()
	s.closed = true
	candidates := make([]*run, 0, len(s.runs))
	for _, current := range s.runs {
		candidates = append(candidates, current)
	}
	s.mu.Unlock()
	for _, current := range candidates {
		current.stop()
	}
	for _, current := range candidates {
		<-current.done
	}
}

func (r *run) isStopped() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stopped
}

func (r *run) requestStop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stopped = true
}

func (r *run) setStopStatus(status string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stopStatus = status
}

func (r *run) getStopStatus() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stopStatus
}

func (r *run) setOutcome(outcome string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.outcome = outcome
}

func (r *run) getOutcome() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.outcome
}

// DeleteExpired applies the retention policy.
func (s *Service) DeleteExpired(ctx context.Context) (int, error) {
	return s.store.DeleteExpired(ctx, time.Now().UTC().Add(-s.cfg.Retention))
}

// startRetention deletes expired pages until the context ends.
func (s *Service) startRetention(ctx context.Context) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_, _ = s.DeleteExpired(ctx)
		}
	}
}

// RunRetention runs the hourly retention loop.
func (s *Service) RunRetention(ctx context.Context) { s.startRetention(ctx) }

// Retention returns the configured retention window.
func (s *Service) Retention() time.Duration { return s.cfg.Retention }

// CallTool executes one tool callback from the agent.
func (s *Service) CallTool(ctx context.Context, turnID string, request ToolCallRequest) (ToolCallResult, error) {
	if err := validateCallID(request.CallID); err != nil {
		return ToolCallResult{}, err
	}
	if len(request.Arguments) == 0 {
		request.Arguments = json.RawMessage("{}")
	}
	if len(request.Arguments) > mcp.MaxArgumentBytes {
		return ToolCallResult{}, invalid("The tool arguments exceed the size bound.")
	}
	s.mu.Lock()
	current, found := s.runs[turnID]
	s.mu.Unlock()
	if !found {
		return ToolCallResult{}, conflict("turn_finished", "The turn is not running.")
	}
	pageID := current.turn.PageID
	redactedArguments, argumentRedactions := redact.Bytes(request.Arguments)
	if existing, found, err := s.store.EvidenceInTurn(ctx, pageID, turnID, request.CallID); err != nil {
		return ToolCallResult{}, internal("The evidence could not be read.")
	} else if found {
		if string(existing.Arguments) != string(redactedArguments) {
			return ToolCallResult{}, conflict("idempotency_conflict",
				"The callId was already used with different arguments.")
		}
		return evidenceResult(existing), nil
	}
	evidence, err := s.store.EvidenceForTurn(ctx, pageID, turnID)
	if err != nil {
		return ToolCallResult{}, internal("The evidence could not be read.")
	}
	if len(evidence) >= s.cfg.MaxToolCalls {
		return s.recordToolOutcome(ctx, current, request, redactedArguments, argumentRedactions,
			transcript.Evidence{
				Status: transcript.EvidenceBudgetExhausted,
				Result: mustJSON(map[string]any{keyError: "the turn exhausted its tool-call budget"}),
			}, 0)
	}
	if _, err := s.store.AppendEvent(ctx, transcript.Event{
		PageID: pageID, TurnID: turnID, Type: transcript.EventToolStarted,
		Data: mustJSON(map[string]any{
			"callId": request.CallID, "tool": request.Tool, "arguments": redactedArguments,
		}),
	}); err != nil {
		slog.Error("store the tool start event", "turn", current.turn.ID, "call", request.CallID, "error", err)
		return ToolCallResult{}, internal("The tool event could not be stored.")
	}
	if s.cfg.Toolbox == nil {
		return s.recordToolOutcome(ctx, current, request, redactedArguments, argumentRedactions,
			transcript.Evidence{
				Status: transcript.EvidenceDenied,
				Result: mustJSON(map[string]any{keyError: "the tool integration is not configured"}),
			}, 0)
	}
	started := time.Now()
	toolContext, cancel := context.WithTimeout(current.ctx, s.cfg.ToolTimeout)
	result, callErr := s.cfg.Toolbox.Call(toolContext, request.Tool, redactedArguments)
	cancel()
	duration := time.Since(started)
	outcome := transcript.Evidence{
		Tool: request.Tool, Status: transcript.EvidenceOK,
		Arguments: redactedArguments, DurationMS: duration.Milliseconds(),
		CreatedAt: time.Now().UTC(),
	}
	switch {
	case callErr != nil && errors.Is(callErr, context.DeadlineExceeded):
		outcome.Status = transcript.EvidenceTimedOut
		outcome.Result = mustJSON(map[string]any{keyError: "the tool call timed out"})
	case callErr != nil && errors.Is(callErr, mcp.ErrDenied):
		outcome.Status = transcript.EvidenceDenied
		outcome.Result = mustJSON(map[string]any{keyError: "the tool call was denied by policy"})
	case callErr != nil:
		outcome.Status = transcript.EvidenceFailed
		outcome.Result = mustJSON(map[string]any{keyError: "the tool call failed"})
	default:
		encoded, redactions := redact.Bytes(toolResultJSON(result.Text))
		outcome.Redacted += redactions
		encoded, truncated := truncate(encoded, s.cfg.MaxResultBytes)
		outcome.Result = encoded
		outcome.Truncated = truncated
		if truncated {
			outcome.Status = transcript.EvidenceTruncated
		}
		if result.IsError {
			outcome.Status = transcript.EvidenceFailed
		}
	}
	return s.recordToolOutcome(ctx, current, request, redactedArguments, argumentRedactions, outcome, duration)
}

// recordToolOutcome stores one attempt and appends its event.
func (s *Service) recordToolOutcome(
	ctx context.Context, current *run, request ToolCallRequest, arguments json.RawMessage,
	argumentRedactions int, evidence transcript.Evidence, duration time.Duration,
) (ToolCallResult, error) {
	evidence.PageID = current.turn.PageID
	evidence.TurnID = current.turn.ID
	evidence.CallID = request.CallID
	evidence.Tool = request.Tool
	evidence.Arguments = arguments
	if evidence.CreatedAt.IsZero() {
		evidence.CreatedAt = time.Now().UTC()
	}
	if evidence.Result == nil {
		evidence.Result = json.RawMessage("null")
	}
	evidence.Redacted += argumentRedactions
	if duration > 0 {
		evidence.DurationMS = duration.Milliseconds()
	}
	stored, err := s.store.PutEvidence(ctx, evidence)
	if err != nil {
		slog.Error("store the tool evidence", "turn", current.turn.ID, "call", request.CallID, "error", err)
		return ToolCallResult{}, internal("The evidence could not be stored.")
	}
	if _, err := s.store.AppendEvent(ctx, transcript.Event{
		PageID: current.turn.PageID, TurnID: current.turn.ID, Type: transcript.EventToolFinished,
		Data: mustJSON(map[string]any{
			"callId": request.CallID, "tool": request.Tool, "status": stored.Status,
			"durationMs": stored.DurationMS, "redacted": stored.Redacted > 0,
			"truncated": stored.Truncated,
		}),
	}); err != nil {
		slog.Error("store the tool event", "turn", current.turn.ID, "call", request.CallID, "error", err)
		return ToolCallResult{}, internal("The tool event could not be stored.")
	}
	return evidenceResult(stored), nil
}

// ToolCallRequest is the private callback body.
type ToolCallRequest struct {
	CallID    string          `json:"callId"`
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments"`
}

// ToolCallResult is the private callback response.
type ToolCallResult struct {
	Status    string          `json:"status"`
	Result    json.RawMessage `json:"result"`
	Redacted  bool            `json:"redacted"`
	Truncated bool            `json:"truncated"`
}

func evidenceResult(evidence transcript.Evidence) ToolCallResult {
	// The transcript keeps "failed" for the dashboard's tool history. The
	// private agent interface documents "error" for the same outcome, and its
	// contract rejects "failed" as malformed, so translate at the callback
	// boundary rather than changing the stored evidence.
	status := evidence.Status
	if status == transcript.EvidenceFailed {
		status = "error"
	}
	return ToolCallResult{
		Status: status, Result: evidence.Result,
		Redacted: evidence.Redacted > 0, Truncated: evidence.Truncated,
	}
}

// validateRequest validates the shared client body.
func validateRequest(request Request) (string, string, transcript.Target, *Error) {
	message := strings.TrimSpace(request.Message)
	if message == "" {
		return "", "", transcript.Target{}, invalid("The message is required.")
	}
	if len(message) > 16<<10 {
		return "", "", transcript.Target{}, invalid("The message exceeds 16 KiB.")
	}
	namespace := strings.TrimSpace(request.Namespace)
	if namespace != "" && !validName(namespace) {
		return "", "", transcript.Target{}, invalid("The namespace is not a valid namespace name.")
	}
	target := transcript.Target{}
	if request.Target != nil {
		target.Kind = strings.TrimSpace(request.Target.Kind)
		target.Name = strings.TrimSpace(request.Target.Name)
		if target.Kind == "" || target.Name == "" {
			return "", "", transcript.Target{}, invalid("The target requires kind and name.")
		}
		if !validKind(target.Kind) || !validResourceName(target.Name) {
			return "", "", transcript.Target{}, invalid("The target kind or name is not valid.")
		}
		if request.Target.Namespace != "" {
			if namespace != "" && request.Target.Namespace != namespace {
				return "", "", transcript.Target{}, invalid("The target namespace conflicts with the namespace hint.")
			}
			if !validName(request.Target.Namespace) {
				return "", "", transcript.Target{}, invalid("The target namespace is not a valid namespace name.")
			}
			namespace = request.Target.Namespace
		}
	}
	return message, namespace, target, nil
}

// resolveHints applies the conversation hints a reply inherits.
func resolveHints(
	page transcript.Page, namespace string, rawTarget *Target, target transcript.Target,
) (string, transcript.Target, *Error) {
	resolved := namespace
	if resolved == "" {
		resolved = page.Namespace
	} else if page.Namespace != "" && resolved != page.Namespace {
		return "", transcript.Target{}, invalid("The namespace conflicts with the page's namespace.")
	}
	if rawTarget == nil {
		return resolved, page.Target, nil
	}
	return resolved, target, nil
}

func validateKey(key string) *Error {
	trimmed := strings.TrimSpace(key)
	if trimmed == "" {
		return invalid("The Idempotency-Key header is required.")
	}
	if len(trimmed) > 200 || strings.ContainsAny(trimmed, "\r\n") {
		return invalid("The Idempotency-Key header is not valid.")
	}
	return nil
}

func validateCallID(callID string) *Error {
	trimmed := strings.TrimSpace(callID)
	if trimmed == "" || len(trimmed) > 128 {
		return invalid("The callId is required and bounded to 128 characters.")
	}
	for _, character := range trimmed {
		switch {
		case character >= 'a' && character <= 'z':
		case character >= 'A' && character <= 'Z':
		case character >= '0' && character <= '9':
		case character == '-', character == '_', character == '.', character == ':':
		default:
			return invalid("The callId contains an unsupported character.")
		}
	}
	return nil
}

// validateCallbackOrigin requires an origin with no path, query, fragment or
// user information.
func validateCallbackOrigin(origin string) error {
	parsed, err := url.Parse(origin)
	if err != nil {
		return fmt.Errorf("investigations: the callback origin is not a URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return errors.New("investigations: the callback origin must be http or https")
	}
	if parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
		(parsed.Path != "" && parsed.Path != "/") {
		return errors.New("investigations: the callback origin must carry no user information, path, query or fragment")
	}
	return nil
}

func requestHash(request Request) string {
	encoded, _ := json.Marshal(struct {
		Message   string  `json:"message"`
		Namespace string  `json:"namespace"`
		Target    *Target `json:"target"`
	}{Message: request.Message, Namespace: request.Namespace, Target: request.Target})
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// agentMessages keeps the newest messages within the conversation bounds, and
// always keeps the newest message so the agent sees the current request.
func agentMessages(messages []transcript.Message, maxMessages, maxBytes int) []agentclient.Message {
	start := 0
	if len(messages) > maxMessages {
		start = len(messages) - maxMessages
	}
	total := 0
	for index := len(messages) - 1; index >= start; index-- {
		size := len(messages[index].Content)
		if total+size > maxBytes && index != len(messages)-1 {
			start = index + 1
			break
		}
		total += size
	}
	selected := messages[start:]
	out := make([]agentclient.Message, 0, len(selected))
	for _, message := range selected {
		out = append(out, agentclient.Message{
			Role: message.Role, Content: message.Content, TurnID: message.TurnID,
		})
	}
	return out
}

func agentTools(tools []mcp.Tool) []agentclient.Tool {
	out := make([]agentclient.Tool, 0, len(tools))
	for _, tool := range tools {
		out = append(out, agentclient.Tool{
			Name: tool.Name, Description: tool.Description, InputSchema: tool.InputSchema,
		})
	}
	return out
}

// toolResultJSON turns tool text into a JSON value.
func toolResultJSON(text string) []byte {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return []byte("null")
	}
	if json.Valid([]byte(trimmed)) {
		return []byte(trimmed)
	}
	encoded, err := json.Marshal(trimmed)
	if err != nil {
		return []byte("null")
	}
	return encoded
}

// truncate cuts a JSON document's text at a byte bound on a rune boundary. A
// truncated JSON document is not valid JSON, so the result is wrapped as a
// string before it is cut, and the cut document is itself returned as a JSON
// string. Callers mark the evidence as truncated, so the model knows the result
// is partial.
func truncate(document json.RawMessage, limit int) (json.RawMessage, bool) {
	if len(document) <= limit {
		return document, false
	}
	text := string(document)
	if utf8.RuneStart(text[limit]) {
		text = text[:limit]
	} else {
		cut := limit
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		text = text[:cut]
	}
	wrapped, err := json.Marshal(text)
	if err != nil {
		return json.RawMessage(`"[truncated]"`), true
	}
	return wrapped, true
}

func sanitize(text string) string {
	clean, _ := redact.Text(strings.TrimSpace(text))
	if len(clean) > 500 {
		clean = strings.ToValidUTF8(clean[:500], "") + "..."
	}
	return clean
}

func safePublishError(err error) string {
	switch {
	case errors.Is(err, githubpublish.ErrBase):
		return "The base revision changed before publication; revalidate the change."
	case errors.Is(err, githubpublish.ErrConflict):
		return "The proposal branch already carries a different change."
	case errors.Is(err, githubpublish.ErrPath):
		return "The proposal path is outside the configured source directory."
	case errors.Is(err, githubpublish.ErrRepository):
		return "The proposal repository is not the configured repository."
	case errors.Is(err, githubpublish.ErrUnchanged):
		return "The proposal does not change the base content."
	case errors.Is(err, githubpublish.ErrInvalid):
		return "The proposal is not valid: " + sanitize(err.Error())
	default:
		return "The proposal could not be published: " + sanitize(err.Error())
	}
}

func titleOf(message string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(message), "\n")
	runes := []rune(strings.TrimSpace(line))
	if len(runes) > 120 {
		return string(runes[:117]) + "..."
	}
	if len(runes) == 0 {
		return "Untitled investigation"
	}
	return string(runes)
}

func firstQuestion(messages []transcript.Message) string {
	for _, message := range messages {
		if message.Role == "user" {
			return message.Content
		}
	}
	return ""
}

func validName(name string) bool {
	if len(name) == 0 || len(name) > 253 {
		return false
	}
	for _, character := range name {
		if (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') || character == '-' {
			continue
		}
		return false
	}
	return true
}

// validKind accepts a Kubernetes Kind, which is PascalCase (Deployment,
// CronJob), unlike a namespace or a resource name.
func validKind(kind string) bool {
	if len(kind) == 0 || len(kind) > 253 {
		return false
	}
	for index, character := range kind {
		switch {
		case character >= 'A' && character <= 'Z':
		case character >= 'a' && character <= 'z':
		case character >= '0' && character <= '9' && index > 0:
		default:
			return false
		}
	}
	return true
}

func validResourceName(name string) bool {
	if len(name) == 0 || len(name) > 253 {
		return false
	}
	for _, character := range name {
		switch {
		case character >= 'a' && character <= 'z':
		case character >= 'A' && character <= 'Z':
		case character >= '0' && character <= '9':
		case character == '-', character == '.', character == ':':
		default:
			return false
		}
	}
	return true
}

// mustJSON encodes a fixed shape.
func mustJSON(value any) json.RawMessage {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic("investigations: encode a fixed shape: " + err.Error())
	}
	return encoded
}
