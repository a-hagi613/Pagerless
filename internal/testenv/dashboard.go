package testenv

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/a-hagi613/pagerless/internal/dashboardapi"
	"github.com/a-hagi613/pagerless/internal/dashboardauth"
)

const (
	// DashboardUser is the username of the session that the harness signs in by default.
	DashboardUser = "dashboard-approver@example.com"
	// dashboardCookie is the cookie that the harness's stand-in sign-in issues.
	dashboardCookie = "pagerless_test_session"
)

// DashboardSession is a signed-in dashboard browser: the cookie to send and the
// CSRF token that a write must repeat.
type DashboardSession struct {
	Cookie *http.Cookie
	CSRF   string
}

// fakeAuth is a stand-in for the Keycloak sign-in. It issues sessions without a
// browser, so a test drives the authenticated API directly; the real flow is
// covered by internal/dashboardauth's tests.
type fakeAuth struct {
	mu       sync.Mutex
	sessions map[string]*dashboardauth.User
	next     int
}

func newFakeAuth() *fakeAuth {
	return &fakeAuth{sessions: make(map[string]*dashboardauth.User)}
}

// signIn issues a session for a user with the approvers group when approver is true.
func (a *fakeAuth) signIn(username string, approver bool) DashboardSession {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.next++
	id := fmt.Sprintf("session-%d", a.next)
	csrf := fmt.Sprintf("csrf-%d", a.next)
	user := &dashboardauth.User{
		Subject:   "subject-" + username,
		Username:  username,
		Groups:    []string{"pagerless-approvers"},
		Approver:  approver,
		CSRF:      csrf,
		ExpiresAt: time.Now().Add(time.Hour),
	}
	if !approver {
		user.Groups = []string{}
	}
	a.sessions[id] = user
	return DashboardSession{
		Cookie: &http.Cookie{Name: dashboardCookie, Value: id, Path: "/"},
		CSRF:   csrf,
	}
}

// User implements dashboardapi.Auth.
func (a *fakeAuth) User(r *http.Request) *dashboardapi.User {
	cookie, err := r.Cookie(dashboardCookie)
	if err != nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	user, ok := a.sessions[cookie.Value]
	if !ok {
		return nil
	}
	copied := *user
	copied.Groups = append([]string(nil), user.Groups...)
	return &copied
}

// SignIn implements dashboardapi.Auth.
func (a *fakeAuth) SignIn() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
	})
}

// Callback implements dashboardapi.Auth.
func (a *fakeAuth) Callback() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
	})
}

// SignOut implements dashboardapi.Auth.
func (a *fakeAuth) SignOut() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/signed-out", http.StatusSeeOther)
	})
}

// RealmProxy implements dashboardapi.Auth.
func (a *fakeAuth) RealmProxy() http.Handler {
	return nil
}

// fakeApprovers is a stand-in for the Keycloak group check.
type fakeApprovers struct {
	mu      sync.Mutex
	members map[string]bool
	err     error
}

func newFakeApprovers(members ...string) *fakeApprovers {
	approvers := &fakeApprovers{members: make(map[string]bool)}
	for _, member := range members {
		approvers.members[member] = true
	}
	return approvers
}

// IsApprover implements admission.ApproverGroups.
func (f *fakeApprovers) IsApprover(_ context.Context, username string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return false, f.err
	}
	return f.members[username], nil
}

// WithApprovers sets the users that the Keycloak membership stand-in reports as
// members of the approvers group.
func WithApprovers(usernames ...string) Option {
	return func(o *options) {
		o.approvers = newFakeApprovers(usernames...)
	}
}

// WithDashboardUser sets the username and approver status of the session that
// the harness signs in by default.
func WithDashboardUser(username string, approver bool) Option {
	return func(o *options) {
		o.dashboardUser, o.dashboardApprover = username, approver
	}
}

// SignInDashboard issues a dashboard session for a user, with the approvers
// group when approver is true.
func (e *Env) SignInDashboard(t testing.TB, username string, approver bool) DashboardSession {
	t.Helper()
	return e.auth.signIn(username, approver)
}
