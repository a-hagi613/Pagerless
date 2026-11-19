package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/credentials"
	"github.com/a-hagi613/pagerless/internal/dashboardauth"
)

// apiClient calls the Portal's HTTP API in server mode.
type apiClient struct {
	// base is the server's base URL without a trailing slash.
	base string
	// http is the client. Its timeout bounds a hung server without capping a
	// streamed watch, which is not a server-mode command.
	http *http.Client
	// token is the signed session cookie from `login`.
	token string
	// csrf is the session's CSRF token, derived from the cookie's payload.
	csrf string
}

// newAPIClient validates the server URL and the stored session.
func newAPIClient(server, token string) (*apiClient, error) {
	trimmed := strings.TrimSpace(server)
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, usagef("--server must be an absolute http or https URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, usagef("--server must be an http or https URL")
	}
	api := &apiClient{
		base:  strings.TrimSuffix(trimmed, "/"),
		http:  &http.Client{Timeout: 60 * time.Second},
		token: token,
	}
	if token != "" {
		csrf, err := csrfFromSession(token)
		if err != nil {
			return nil, fmt.Errorf("the stored session is unreadable: run `kubectl page login` again")
		}
		api.csrf = csrf
	}
	return api, nil
}

// errNoSession is returned when a server-mode write needs a session.
var errNoSession = errors.New("server mode needs a session: run `kubectl page login`")

// do performs one request and decodes a JSON answer into out.
func (c *apiClient) do(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode the request: %w", err)
		}
		reader = bytes.NewReader(raw)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return fmt.Errorf("build the request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		request.AddCookie(&http.Cookie{Name: dashboardauth.DefaultCookieName, Value: c.token})
	}
	if method != http.MethodGet {
		if c.token == "" {
			return errNoSession
		}
		request.Header.Set("X-CSRF-Token", c.csrf)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("call %s %s: %w", method, path, err)
	}
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("read the response: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return apiFailure(response.StatusCode, raw)
	}
	if out == nil || response.StatusCode == http.StatusNoContent {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode the response: %w", err)
	}
	return nil
}

// apiFailure maps an error response to a documented exit code.
func apiFailure(status int, raw []byte) error {
	var body struct {
		Error json.RawMessage `json:"error"`
	}
	code, message, manager := "", "", ""
	if err := json.Unmarshal(raw, &body); err == nil {
		var object struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Manager string `json:"manager"`
		}
		if err := json.Unmarshal(body.Error, &object); err == nil {
			code, message, manager = object.Code, object.Message, object.Manager
		} else if err := json.Unmarshal(body.Error, &message); err != nil {
			message = ""
		}
	}
	if message == "" {
		message = http.StatusText(status)
	}
	switch {
	case status == http.StatusUnauthorized:
		return &ExitError{Code: ExitFailure, Err: errNoSession}
	case code == "settings_externally_managed":
		return &ExitError{Code: ExitExternal, Err: fmt.Errorf("%s", message)}
	case code == "proposal_changed":
		return &ExitError{Code: ExitStale, Err: fmt.Errorf("%s", message)}
	default:
		if manager != "" {
			message = fmt.Sprintf("%s (managed by %s)", message, manager)
		}
		if code != "" {
			return &ExitError{Code: ExitFailure, Err: fmt.Errorf("%s: %s", code, message)}
		}
		return &ExitError{Code: ExitFailure, Err: fmt.Errorf("%d %s", status, message)}
	}
}

// csrfFromSession reads the csrf claim from a signed session cookie. The
// signature is verified by the server; the CLI only needs the claim it must
// repeat, so it does not hold or verify the signing key.
func csrfFromSession(cookie string) (string, error) {
	payload, _, ok := strings.Cut(cookie, ".")
	if !ok {
		return "", errors.New("the session is not a signed cookie")
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return "", fmt.Errorf("decode the session payload: %w", err)
	}
	var claims struct {
		CSRF string `json:"csrf"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil {
		return "", fmt.Errorf("parse the session payload: %w", err)
	}
	if claims.CSRF == "" {
		return "", errors.New("the session carries no CSRF token")
	}
	return claims.CSRF, nil
}

// serverPageSummary is one row of GET /api/pages.
type serverPageSummary struct {
	Name      string                   `json:"name"`
	Phase     v1alpha1.Phase           `json:"phase"`
	Target    v1alpha1.TargetReference `json:"target"`
	Trigger   v1alpha1.Trigger         `json:"trigger"`
	CreatedAt time.Time                `json:"createdAt"`
	Action    v1alpha1.Action          `json:"action,omitempty"`
}

// serverPages is the body of GET /api/pages.
type serverPages struct {
	Pages []serverPageSummary `json:"pages"`
}

// serverDetail is the body of GET /api/pages/{name}.
type serverDetail struct {
	Name         string                   `json:"name"`
	Phase        v1alpha1.Phase           `json:"phase"`
	Target       v1alpha1.TargetReference `json:"target"`
	Trigger      v1alpha1.Trigger         `json:"trigger"`
	CreatedAt    time.Time                `json:"createdAt"`
	Hint         string                   `json:"hint,omitempty"`
	PreviousPage string                   `json:"previousPage,omitempty"`
	TriggerCount int32                    `json:"triggerCount"`
	Findings     []v1alpha1.Finding       `json:"findings"`
	Diagnosis    string                   `json:"diagnosis,omitempty"`
	Proposal     *v1alpha1.Proposal       `json:"proposal,omitempty"`
	Decision     *v1alpha1.Decision       `json:"decision,omitempty"`
	PullRequest  *v1alpha1.PullRequest    `json:"pullRequest,omitempty"`
	Verification *struct {
		Checks []v1alpha1.VerificationCheck `json:"checks"`
	} `json:"verification,omitempty"`
	Narrative string `json:"narrative,omitempty"`
}

// serverStartResponse is the answer to POST /api/pages.
type serverStartResponse struct {
	Name          string `json:"name"`
	AlreadyActive bool   `json:"alreadyActive"`
}

// serverSettings is the masked body of GET /api/settings.
type serverSettings struct {
	ManagedBy        string                       `json:"managedBy"`
	Manager          string                       `json:"manager"`
	WriteCredentials bool                         `json:"writeCredentials"`
	Onboarding       bool                         `json:"onboarding"`
	Values           map[string]string            `json:"values,omitempty"`
	Fields           map[string]credentials.Field `json:"fields"`
}

// serverVersion is the body of GET /api/version.
type serverVersion struct {
	Version string `json:"version"`
	Mode    string `json:"mode"`
}

// page returns one Page's record from the selected transport.
func (s *session) page(ctx context.Context, name string) (*record, error) {
	if s.api != nil {
		var detail serverDetail
		if err := s.api.do(ctx, http.MethodGet, "/api/pages/"+url.PathEscape(name), nil, &detail); err != nil {
			return nil, err
		}
		r := &record{
			Raw: &detail, Name: detail.Name, Phase: detail.Phase, Target: detail.Target,
			Trigger: detail.Trigger, TriggerCount: detail.TriggerCount, CreatedAt: detail.CreatedAt,
			Hint: detail.Hint, PreviousPage: detail.PreviousPage, Diagnosis: detail.Diagnosis,
			Findings: detail.Findings, Proposal: detail.Proposal, Decision: detail.Decision,
			PullRequest: detail.PullRequest, Narrative: detail.Narrative,
		}
		if detail.Verification != nil {
			r.VerificationChecks = detail.Verification.Checks
		}
		return r, nil
	}
	var page v1alpha1.Page
	key := client.ObjectKey{Namespace: s.opts.pageNamespace, Name: name}
	if err := s.clus.client.Get(ctx, key, &page); err != nil {
		return nil, pageError(err, name, s.opts.pageNamespace)
	}
	return recordFromPage(&page), nil
}

// pages returns the Pages, most recent first, optionally narrowed to one
// phase, together with the transport's own list object for --output json or
// yaml.
func (s *session) pages(ctx context.Context, phase v1alpha1.Phase) ([]*record, any, error) {
	if s.api != nil {
		path := "/api/pages"
		if phase != "" {
			path += "?phase=" + url.QueryEscape(string(phase))
		}
		var list serverPages
		if err := s.api.do(ctx, http.MethodGet, path, nil, &list); err != nil {
			return nil, nil, err
		}
		records := make([]*record, 0, len(list.Pages))
		for i := range list.Pages {
			summary := list.Pages[i]
			records = append(records, &record{
				Raw: &summary, Name: summary.Name, Phase: summary.Phase, Target: summary.Target,
				Trigger: summary.Trigger, CreatedAt: summary.CreatedAt,
			})
		}
		slices.SortFunc(records, newestFirst)
		return records, &list, nil
	}
	var list v1alpha1.PageList
	if err := s.clus.client.List(ctx, &list, client.InNamespace(s.opts.pageNamespace)); err != nil {
		return nil, nil, fmt.Errorf("list Pages: %w", err)
	}
	list.TypeMeta = metaFor("PageList")
	if phase != "" {
		kept := list.Items[:0]
		for i := range list.Items {
			if list.Items[i].Status.Phase == phase {
				kept = append(kept, list.Items[i])
			}
		}
		list.Items = kept
	}
	records := make([]*record, 0, len(list.Items))
	for i := range list.Items {
		records = append(records, recordFromPage(&list.Items[i]))
	}
	slices.SortFunc(records, newestFirst)
	return records, &list, nil
}

// create opens a Page for a Target and reports its name and whether an Active
// Page already covered it.
func (s *session) create(ctx context.Context, ref v1alpha1.TargetReference, pod, hint string) (string, bool, error) {
	if s.api != nil {
		var answer serverStartResponse
		body := struct {
			Target v1alpha1.TargetReference `json:"target"`
			Hint   string                   `json:"hint,omitempty"`
		}{Target: ref, Hint: hint}
		if err := s.api.do(ctx, http.MethodPost, "/api/pages", body, &answer); err != nil {
			return "", false, err
		}
		return answer.Name, answer.AlreadyActive, nil
	}
	ref, err := confirmTarget(ctx, s, ref)
	if err != nil {
		return "", false, err
	}
	fingerprint := v1alpha1.Fingerprint(ref, manualReason)
	if name, ok, err := activePage(ctx, s.clus.client, s.opts.pageNamespace, fingerprint); err != nil {
		return "", false, err
	} else if ok {
		return name, true, nil
	}
	page := newPage(ref, s.opts.pageNamespace, hint, pod)
	if err := s.clus.client.Create(ctx, page); err != nil {
		return "", false, fmt.Errorf("create Page: %w", err)
	}
	return page.Name, false, nil
}

// decide records an approve or reject decision, bound to the Proposal hash the
// operator was shown.
func (s *session) decide(
	ctx context.Context, rec *record, outcome v1alpha1.DecisionOutcome, reason, decidedBy string,
) (string, error) {
	if s.api != nil {
		body := struct {
			Decision     string `json:"decision"`
			ProposalHash string `json:"proposalHash"`
			Reason       string `json:"reason,omitempty"`
		}{Decision: string(outcome), ProposalHash: rec.hash(), Reason: reason}
		if err := s.api.do(ctx, http.MethodPost, "/api/pages/"+url.PathEscape(rec.Name)+"/decision", body, nil); err != nil {
			return "", err
		}
		return "", nil
	}
	if rec.Proposal == nil {
		return "", fmt.Errorf("page %s has no Proposal to decide", rec.Name)
	}
	// Re-read before writing, so a Proposal that changed while the operator read
	// it is refused here rather than applied to a hash they never saw.
	var fresh v1alpha1.Page
	key := client.ObjectKey{Namespace: s.opts.pageNamespace, Name: rec.Name}
	if err := s.clus.client.Get(ctx, key, &fresh); err != nil {
		return "", pageError(err, rec.Name, s.opts.pageNamespace)
	}
	if fresh.Status.Proposal == nil || fresh.Status.Proposal.Hash != rec.hash() {
		return "", &ExitError{Code: ExitStale, Err: fmt.Errorf(
			"the Proposal changed since it was displayed; reopen the Page and decide again")}
	}
	if decidedBy == "" {
		decidedBy = authenticatedUser(ctx, s.clus.client)
	}
	if err := submitDecision(ctx, s.clus.client, &fresh, outcome, reason, decidedBy); err != nil {
		return "", err
	}
	return decidedBy, nil
}

// settings returns the masked credentials from the selected transport.
func (s *session) settings(ctx context.Context) (serverSettings, error) {
	if s.api != nil {
		var view serverSettings
		if err := s.api.do(ctx, http.MethodGet, "/api/settings", nil, &view); err != nil {
			return serverSettings{}, err
		}
		return view, nil
	}
	store, err := s.credentialStore()
	if err != nil {
		return serverSettings{}, err
	}
	view, err := store.View(ctx)
	if err != nil {
		return serverSettings{}, settingsError(err)
	}
	return serverSettings{
		ManagedBy: view.ManagedBy, Manager: view.Manager, WriteCredentials: view.WriteCredentials,
		Onboarding: view.Onboarding, Fields: view.Fields,
	}, nil
}

// writeSettings merges values into the credentials Secret.
func (s *session) writeSettings(ctx context.Context, values map[string]string) (serverSettings, error) {
	if s.api != nil {
		var view serverSettings
		body := struct {
			Values map[string]string `json:"values"`
		}{Values: values}
		if err := s.api.do(ctx, http.MethodPatch, "/api/settings", body, &view); err != nil {
			return serverSettings{}, err
		}
		return view, nil
	}
	store, err := s.credentialStore()
	if err != nil {
		return serverSettings{}, err
	}
	view, err := store.Write(ctx, values)
	if err != nil {
		return serverSettings{}, settingsError(err)
	}
	return serverSettings{
		ManagedBy: view.ManagedBy, Manager: view.Manager, WriteCredentials: view.WriteCredentials,
		Onboarding: view.Onboarding, Fields: view.Fields,
	}, nil
}

// settingsTestResult is the body of POST /api/settings/test.
type settingsTestResult struct {
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// testSetting probes one configured dependency. The probe runs in the server's
// network with the credentials the Secret holds, so it is available in server
// mode only.
func (s *session) testSetting(ctx context.Context, target string) (settingsTestResult, error) {
	if s.api == nil {
		return settingsTestResult{}, usagef(
			"config test needs --server: the probe runs in the server's network with the stored credentials")
	}
	var result settingsTestResult
	body := struct {
		Target string `json:"target"`
	}{Target: target}
	if err := s.api.do(ctx, http.MethodPost, "/api/settings/test", body, &result); err != nil {
		return settingsTestResult{}, err
	}
	return result, nil
}

// clearSetting empties one credential key.
func (s *session) clearSetting(ctx context.Context, key string) (serverSettings, error) {
	if s.api != nil {
		var view serverSettings
		if err := s.api.do(ctx, http.MethodDelete, "/api/settings/"+url.PathEscape(key), nil, &view); err != nil {
			return serverSettings{}, err
		}
		return view, nil
	}
	store, err := s.credentialStore()
	if err != nil {
		return serverSettings{}, err
	}
	view, err := store.Clear(ctx, []string{key})
	if err != nil {
		return serverSettings{}, settingsError(err)
	}
	return serverSettings{
		ManagedBy: view.ManagedBy, Manager: view.Manager, WriteCredentials: view.WriteCredentials,
		Onboarding: view.Onboarding, Fields: view.Fields,
	}, nil
}

// credentialStore builds the shared credentials store for kubeconfig mode.
func (s *session) credentialStore() (*credentials.Store, error) {
	return credentials.New(credentials.Config{
		Client:     s.clus.client,
		Namespace:  s.opts.pageNamespace,
		SecretName: s.opts.credentialsSecret,
		MountPath:  "/var/run/pagerless/credentials",
	})
}

// settingsError maps a credentials failure to a documented exit code.
func settingsError(err error) error {
	if _, ok := errors.AsType[*credentials.ExternalError](err); ok {
		return &ExitError{Code: ExitExternal, Err: err}
	}
	if _, ok := errors.AsType[*credentials.InvalidError](err); ok {
		return &ExitError{Code: ExitUsage, Err: err}
	}
	return err
}
