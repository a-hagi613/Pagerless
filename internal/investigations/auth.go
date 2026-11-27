package investigations

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// SessionCookie is the browser session cookie name.
const SessionCookie = "pagerless_session"

// defaultSessionTTL bounds a browser session.
const defaultSessionTTL = 12 * time.Hour

// Auth authenticates the two interfaces. The operator token is supplied by the
// installer and grants the installation's scope; the internal token is shared
// only with the agent and the MCP integrations.
type Auth struct {
	// OperatorToken returns the installer-provided operator token.
	OperatorToken func() (string, error)
	// InternalToken returns the installer-provided private credential.
	InternalToken func() (string, error)
	// SessionTTL bounds one browser session.
	SessionTTL time.Duration
}

// NewAuth returns an authenticator.
func NewAuth(operator, internal func() (string, error)) *Auth {
	return &Auth{OperatorToken: operator, InternalToken: internal, SessionTTL: defaultSessionTTL}
}

// Authenticate accepts a bearer header or a browser session cookie.
func (a *Auth) Authenticate(request *http.Request) (bool, error) {
	if token := bearerOf(request); token != "" {
		return a.operator(token)
	}
	cookie, err := request.Cookie(SessionCookie)
	if errors.Is(err, http.ErrNoCookie) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return a.verifySession(cookie.Value)
}

// IsBrowser reports whether a request authenticated with the session cookie.
func IsBrowser(request *http.Request) bool {
	return bearerOf(request) == "" && request.Header.Get("Cookie") != ""
}

// Internal authenticates a private callback.
func (a *Auth) Internal(request *http.Request) bool {
	if a.InternalToken == nil {
		return false
	}
	expected, err := a.InternalToken()
	if err != nil || expected == "" {
		return false
	}
	provided := bearerOf(request)
	if provided == "" {
		return false
	}
	return subtleCompare(expected, provided)
}

// Session exchanges the operator token for an HttpOnly session cookie.
func (a *Auth) Session(response http.ResponseWriter, request *http.Request, token string) *Error {
	ok, err := a.operator(token)
	if err != nil {
		return internal("The operator token could not be read.")
	}
	if !ok {
		return &Error{HTTPStatus: http.StatusUnauthorized, Code: codeUnauthorized,
			Message: "The operator token is not valid."}
	}
	value, err := a.issue(time.Now().Add(a.ttl()))
	if err != nil {
		return internal("The session could not be created.")
	}
	http.SetCookie(response, &http.Cookie{
		Name:     SessionCookie,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   requestIsSecure(request),
		MaxAge:   int(a.ttl() / time.Second),
	})
	return nil
}

func (a *Auth) ttl() time.Duration {
	if a.SessionTTL <= 0 {
		return defaultSessionTTL
	}
	return a.SessionTTL
}

// operator compares a candidate to the installer token in constant time.
func (a *Auth) operator(candidate string) (bool, error) {
	if a.OperatorToken == nil {
		return false, nil
	}
	expected, err := a.OperatorToken()
	if err != nil {
		return false, err
	}
	if expected == "" || candidate == "" {
		return false, nil
	}
	return subtleCompare(expected, candidate), nil
}

// sessionKey derives the cookie signing key from the operator token, so a
// rotated token also invalidates every existing session.
func (a *Auth) sessionKey() ([]byte, error) {
	if a.OperatorToken == nil {
		return nil, errors.New("no operator token")
	}
	token, err := a.OperatorToken()
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte("pagerless-session:" + token))
	return sum[:], nil
}

// issue signs an expiry with the session key.
func (a *Auth) issue(expiry time.Time) (string, error) {
	key, err := a.sessionKey()
	if err != nil {
		return "", err
	}
	payload := strconv.FormatInt(expiry.UTC().Unix(), 10)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." +
		base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

// verifySession checks the signature and the expiry.
func (a *Auth) verifySession(value string) (bool, error) {
	key, err := a.sessionKey()
	if err != nil {
		return false, err
	}
	encodedPayload, encodedMAC, found := strings.Cut(value, ".")
	if !found {
		return false, nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(encodedPayload)
	if err != nil {
		return false, nil
	}
	providedMAC, err := base64.RawURLEncoding.DecodeString(encodedMAC)
	if err != nil {
		return false, nil
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(payload)
	if !hmac.Equal(mac.Sum(nil), providedMAC) {
		return false, nil
	}
	expiry, err := strconv.ParseInt(string(payload), 10, 64)
	if err != nil {
		return false, nil
	}
	return time.Now().Before(time.Unix(expiry, 0)), nil
}

// SameOrigin accepts a browser write only from the installation's own origin.
// A port-forward in the browser sends an Origin for every non-GET request; a
// request with no Origin and no Referer cannot be a browser write and is
// refused rather than assumed.
func SameOrigin(request *http.Request) bool {
	origin := strings.TrimSpace(request.Header.Get("Origin"))
	if origin == "" {
		origin = strings.TrimSpace(request.Header.Get("Referer"))
		if origin == "" {
			return false
		}
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Host == "" {
		return false
	}
	scheme := "http"
	if requestIsSecure(request) {
		scheme = "https"
	}
	return parsed.Scheme == scheme && subtleCompare(parsed.Host, request.Host)
}

func requestIsSecure(request *http.Request) bool {
	if request.TLS != nil {
		return true
	}
	return strings.EqualFold(request.Header.Get("X-Forwarded-Proto"), "https")
}

func bearerOf(request *http.Request) string {
	header := strings.TrimSpace(request.Header.Get("Authorization"))
	scheme, token, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(token)
}

func subtleCompare(expected, provided string) bool {
	return subtle.ConstantTimeCompare([]byte(expected), []byte(provided)) == 1
}

// ErrorObject is the wire error body.
type ErrorObject struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ErrorBody wraps the wire error.
type ErrorBody struct {
	Error ErrorObject `json:"error"`
}

// writeError renders an error response. The message is already sanitized by
// the caller; no credential is ever put in it.
func writeError(response http.ResponseWriter, err *Error) {
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(err.HTTPStatus)
	_ = json.NewEncoder(response).Encode(ErrorBody{Error: ErrorObject{Code: err.Code, Message: err.Message}})
}
