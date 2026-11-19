// Package credentials owns the one Secret that holds every credential Pagerless
// needs. The Portal settings API and `kubectl page config` both write it through
// this package, so the key set of a Secret written from either surface is
// identical (ADR-0033). A credential value reaches this package and the Secret,
// never a Helm value, a response body, a log line, or a command-line flag.
package credentials

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The fixed, non-secret keys of the credentials Secret. Every credential has
// exactly one key, in upper snake case, and nothing else is accepted.
const (
	// KeyAIBaseURL is the OpenAI-compatible endpoint base URL.
	KeyAIBaseURL = "AI_BASE_URL"
	// KeyAIAPIKey is the model provider's API key.
	KeyAIAPIKey = "AI_API_KEY"
	// KeyAIModel is the model identifier the agent requests.
	KeyAIModel = "AI_MODEL"
	// KeyGitHubToken is the write-scoped token that publishes a Proposal.
	KeyGitHubToken = "GITHUB_TOKEN"
	// KeyGitHubRepo is the Target Repository as owner/name.
	KeyGitHubRepo = "GITHUB_REPO"
	// KeyGitHubAPIURL optionally overrides the GitHub API base URL for GitHub Enterprise.
	KeyGitHubAPIURL = "GITHUB_API_URL"

	// ConnectorTokenPrefix and ConnectorTokenSuffix bracket the upper-snake name
	// of every connector token, such as CONNECTOR_LOKI_TOKEN.
	ConnectorTokenPrefix = "CONNECTOR_"
	ConnectorTokenSuffix = "_TOKEN"

	// AnnotationWrittenAtPrefix records when Pagerless last wrote one key.
	// Kubernetes keeps no per-key timestamp, so the Secret carries it.
	AnnotationWrittenAtPrefix = "pagerless.dev/written-at."

	// managedByPortal and managedByExternal are the two owners of the Secret.
	managedByPortal   = "portal"
	managedByExternal = "external"
)

// Keys returns the fixed keys, in the order a person reads them.
func Keys() []string {
	return []string{KeyAIBaseURL, KeyAIAPIKey, KeyAIModel, KeyGitHubToken, KeyGitHubRepo, KeyGitHubAPIURL}
}

// NonSecretKeys returns the keys whose values are configuration rather than
// credentials. A settings response may return these values; every other key is
// masked.
func NonSecretKeys() []string {
	return []string{KeyAIBaseURL, KeyAIModel, KeyGitHubRepo, KeyGitHubAPIURL}
}

// NonSecret reports whether a key's value may be returned unmasked.
func NonSecret(key string) bool {
	return slices.Contains(NonSecretKeys(), key)
}

// ConnectorTokenKey returns the Secret key that holds one connector's token.
func ConnectorTokenKey(name string) string {
	return ConnectorTokenPrefix +
		strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(name), "-", "_")) +
		ConnectorTokenSuffix
}

// IsConnectorTokenKey reports whether key names a connector token.
func IsConnectorTokenKey(key string) bool {
	if !strings.HasPrefix(key, ConnectorTokenPrefix) || !strings.HasSuffix(key, ConnectorTokenSuffix) {
		return false
	}
	name := strings.TrimSuffix(strings.TrimPrefix(key, ConnectorTokenPrefix), ConnectorTokenSuffix)
	return name != "" && strings.ToUpper(name) == name
}

// KnownKey reports whether key is a fixed key or a connector token key.
func KnownKey(key string) bool {
	return slices.Contains(Keys(), key) || IsConnectorTokenKey(key)
}

// SecretKey reports whether key may be written to the Secret. The connector
// portion must be letters, digits, or underscores so that it is also a valid
// annotation suffix.
func SecretKey(key string) bool {
	if !KnownKey(key) {
		return false
	}
	for _, r := range key {
		if !unicode.IsUpper(r) && !unicode.IsDigit(r) && r != '_' {
			return false
		}
	}
	return true
}

// Field is the masked state of one credential. It never carries the value.
type Field struct {
	Configured  bool       `json:"configured"`
	Fingerprint string     `json:"fingerprint,omitempty"`
	UpdatedAt   *time.Time `json:"updatedAt,omitempty"`
}

// View is the masked state of the whole Secret.
type View struct {
	// ManagedBy is "portal" when Pagerless owns the Secret and "external" when a
	// secret manager does.
	ManagedBy string `json:"managedBy"`
	// Manager names the external manager, or the Secret that Pagerless owns.
	Manager string `json:"manager"`
	// WriteCredentials reports whether this surface may write. It is false when
	// the Secret is externally managed.
	WriteCredentials bool `json:"writeCredentials"`
	// Onboarding is true while no AI endpoint or Target Repository is configured.
	Onboarding bool `json:"onboarding"`
	// Values carries the non-secret configuration only, so a form can prefill it.
	// It never carries a key, token, or password.
	Values map[string]string `json:"values,omitempty"`
	// Fields carries one masked entry per documented key.
	Fields map[string]Field `json:"fields"`
}

// ExternalError is returned when a write is refused because a secret manager
// owns the Secret. It is a client error, not an internal failure.
type ExternalError struct {
	// Manager is the externally managed Secret's name, as configured.
	Manager string
}

func (e *ExternalError) Error() string {
	return fmt.Sprintf("credentials are managed externally by Secret %q and are read-only", e.Manager)
}

// InvalidError is returned when a submitted key or value is unusable, which is
// the caller's mistake rather than an internal failure.
type InvalidError struct {
	// Message explains the refusal.
	Message string
}

func (e *InvalidError) Error() string { return e.Message }

// MissingError is returned when the Secret Pagerless owns does not exist. The
// server reports a misconfiguration rather than creating a Secret it does not
// own.
type MissingError struct {
	// Secret is the Secret's name.
	Secret string
}

func (e *MissingError) Error() string {
	return fmt.Sprintf(
		"credentials Secret %q does not exist: create it, or set credentials.existingSecret", e.Secret)
}

// Config assembles a Store.
type Config struct {
	// Client reads and writes the Secret. It is required in every mode; an
	// externally managed Secret is read from the mounted volume instead, but the
	// client is still needed to report its absence.
	Client client.Client
	// Namespace is the release namespace. It is required.
	Namespace string
	// SecretName is the credentials Secret. It is required.
	SecretName string
	// ExternalManager is the name of the secret manager that owns SecretName.
	// Empty means the Secret is Pagerless's.
	ExternalManager string
	// MountPath is where the Secret is mounted, keyed by file name. It is read
	// when ExternalManager is set, because the server holds no Secret RBAC then.
	MountPath string
	// Now returns the current time. It defaults to time.Now.
	Now func() time.Time
	// Logger records a write by field name only. It defaults to slog.Default().
	Logger *slog.Logger
}

// Store reads the masked credentials and writes the Secret.
type Store struct {
	client          client.Client
	namespace       string
	secretName      string
	externalManager string
	mountPath       string
	now             func() time.Time
	logger          *slog.Logger
}

// New validates cfg and returns a Store.
func New(cfg Config) (*Store, error) {
	switch {
	case cfg.Client == nil:
		return nil, errors.New("a Kubernetes client is required")
	case cfg.Namespace == "":
		return nil, errors.New("a namespace is required")
	case cfg.SecretName == "":
		return nil, errors.New("a credentials Secret name is required")
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Store{
		client:          cfg.Client,
		namespace:       cfg.Namespace,
		secretName:      cfg.SecretName,
		externalManager: strings.TrimSpace(cfg.ExternalManager),
		mountPath:       strings.TrimSpace(cfg.MountPath),
		now:             now,
		logger:          logger,
	}, nil
}

// Hermetic reports whether the Secret's state comes from the mounted volume
// rather than the API. It is true exactly when a secret manager owns it.
func (s *Store) Hermetic() bool {
	return s.externalManager != ""
}

// Read returns the current values, keyed by Secret key. A caller must not echo
// a value. In external mode the values come from the mounted volume.
func (s *Store) Read(ctx context.Context) (map[string]string, error) {
	if s.externalManager != "" {
		return s.readMounted()
	}
	secret, err := s.secret(ctx)
	if err != nil {
		return nil, err
	}
	values := make(map[string]string, len(secret.Data))
	for key, raw := range secret.Data {
		values[key] = string(raw)
	}
	return values, nil
}

// View returns the masked state of every documented key.
func (s *Store) View(ctx context.Context) (View, error) {
	secret, err := s.secretForView(ctx)
	if err != nil {
		return View{}, err
	}
	fields := make(map[string]Field, len(Keys()))
	keys := append(Keys(), connectorKeys(secret)...)
	for _, key := range keys {
		if !SecretKey(key) {
			continue
		}
		value, present := secretString(secret, key)
		configured := present && strings.TrimSpace(value) != ""
		field := Field{Configured: configured}
		if configured {
			field.Fingerprint = Fingerprint(value)
		}
		if at, ok := writtenAt(secret, key); ok {
			field.UpdatedAt = &at
		}
		fields[key] = field
	}
	view := View{
		ManagedBy:        managedByPortal,
		Manager:          s.secretName,
		WriteCredentials: true,
		Fields:           fields,
	}
	if s.externalManager != "" {
		view.ManagedBy = managedByExternal
		view.Manager = s.externalManager
		view.WriteCredentials = false
	}
	view.Onboarding = !fields[KeyAIBaseURL].Configured || !fields[KeyGitHubRepo].Configured
	return view, nil
}

// Write merges values into the Secret: it reads the Secret, replaces only the
// submitted keys, and updates with the observed resource version, retrying once
// on conflict. It never creates the Secret and never clears an unsubmitted key.
func (s *Store) Write(ctx context.Context, values map[string]string) (View, error) {
	if s.externalManager != "" {
		return View{}, &ExternalError{Manager: s.externalManager}
	}
	if len(values) == 0 {
		return View{}, &InvalidError{Message: "no credential keys were submitted"}
	}
	keys := make([]string, 0, len(values))
	for key, value := range values {
		if !SecretKey(key) {
			return View{}, &InvalidError{Message: fmt.Sprintf(
				"unknown credential key %q: accepted keys are %s, and CONNECTOR_<NAME>_TOKEN",
				key, strings.Join(Keys(), ", "))}
		}
		if strings.TrimSpace(value) == "" {
			return View{}, &InvalidError{Message: fmt.Sprintf(
				"credential key %q was submitted empty: use unset to clear a key", key)}
		}
		keys = append(keys, key)
	}
	slices.Sort(keys)

	var lastErr error
	for attempt := range 2 {
		secret, err := s.secret(ctx)
		if err != nil {
			return View{}, err
		}
		if secret.Data == nil {
			secret.Data = map[string][]byte{}
		}
		if secret.Annotations == nil {
			secret.Annotations = map[string]string{}
		}
		stamp := s.now().UTC().Format(time.RFC3339Nano)
		for key, value := range values {
			secret.Data[key] = []byte(value)
			secret.Annotations[AnnotationWrittenAtPrefix+key] = stamp
		}
		if err := s.client.Update(ctx, secret); err != nil {
			if apierrors.IsConflict(err) && attempt == 0 {
				lastErr = err
				continue
			}
			return View{}, fmt.Errorf("update credentials Secret %q: %w", s.secretName, err)
		}
		s.logger.InfoContext(ctx, "wrote credentials", "secret", s.secretName, "fields", keys)
		return s.View(ctx)
	}
	return View{}, fmt.Errorf("update credentials Secret %q: %w", s.secretName, lastErr)
}

// Clear empties keys while keeping them present, so the mounted volume still
// has a file for every documented key. It refuses to touch an external Secret.
func (s *Store) Clear(ctx context.Context, keys []string) (View, error) {
	if s.externalManager != "" {
		return View{}, &ExternalError{Manager: s.externalManager}
	}
	if len(keys) == 0 {
		return View{}, &InvalidError{Message: "no credential keys were given to unset"}
	}
	secret, err := s.secret(ctx)
	if err != nil {
		return View{}, err
	}
	if secret.Data == nil {
		secret.Data = map[string][]byte{}
	}
	if secret.Annotations == nil {
		secret.Annotations = map[string]string{}
	}
	stamp := s.now().UTC().Format(time.RFC3339Nano)
	for _, key := range keys {
		if !SecretKey(key) {
			return View{}, &InvalidError{Message: fmt.Sprintf("unknown credential key %q", key)}
		}
		secret.Data[key] = []byte("")
		secret.Annotations[AnnotationWrittenAtPrefix+key] = stamp
	}
	slices.Sort(keys)
	if err := s.client.Update(ctx, secret); err != nil {
		return View{}, fmt.Errorf("update credentials Secret %q: %w", s.secretName, err)
	}
	s.logger.InfoContext(ctx, "cleared credentials", "secret", s.secretName, "fields", keys)
	return s.View(ctx)
}

// secret reads the Secret Pagerless owns, mapping absence to MissingError.
func (s *Store) secret(ctx context.Context) (*corev1.Secret, error) {
	var secret corev1.Secret
	key := client.ObjectKey{Namespace: s.namespace, Name: s.secretName}
	if err := s.client.Get(ctx, key, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, &MissingError{Secret: s.secretName}
		}
		return nil, fmt.Errorf("read credentials Secret %q: %w", s.secretName, err)
	}
	return &secret, nil
}

// secretForView reads the Secret in owned mode, or the mounted volume in
// external mode. The mounted volume is wrapped in a Secret-shaped value so the
// masking code is shared.
func (s *Store) secretForView(ctx context.Context) (*corev1.Secret, error) {
	if s.externalManager == "" {
		return s.secret(ctx)
	}
	values, err := s.readMounted()
	if err != nil {
		return nil, err
	}
	secret := &corev1.Secret{
		Data:        make(map[string][]byte, len(values)),
		Annotations: map[string]string{},
	}
	for key, value := range values {
		secret.Data[key] = []byte(value)
	}
	return secret, nil
}

// readMounted reads the credentials from the volume the kubelet projects. The
// kubelet updates a projected Secret in place, so a rotation is visible without
// a restart.
func (s *Store) readMounted() (map[string]string, error) {
	if s.mountPath == "" {
		return nil, errors.New("no credentials mount path is configured for an externally managed Secret")
	}
	entries, err := os.ReadDir(s.mountPath)
	if err != nil {
		return nil, fmt.Errorf("read credentials mount %q: %w", s.mountPath, err)
	}
	values := make(map[string]string, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !SecretKey(entry.Name()) {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(s.mountPath, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("read mounted credential %q: %w", entry.Name(), err)
		}
		values[entry.Name()] = strings.TrimRight(string(raw), "\n")
	}
	return values, nil
}

// connectorKeys returns the connector token keys the Secret carries.
func connectorKeys(secret *corev1.Secret) []string {
	keys := make([]string, 0, len(secret.Data))
	for key := range secret.Data {
		if IsConnectorTokenKey(key) {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	return keys
}

// secretString returns one value and whether it is present. In external mode
// the mounted volume fills the same map, so one path masks both.
func secretString(secret *corev1.Secret, key string) (string, bool) {
	raw, ok := secret.Data[key]
	if !ok {
		return "", false
	}
	return string(raw), true
}

// writtenAt returns when Pagerless last wrote a key.
func writtenAt(secret *corev1.Secret, key string) (time.Time, bool) {
	if secret.Annotations == nil {
		return time.Time{}, false
	}
	raw, ok := secret.Annotations[AnnotationWrittenAtPrefix+key]
	if !ok {
		return time.Time{}, false
	}
	at, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, false
	}
	return at.UTC(), true
}

// Fingerprint returns the short SHA-256 fingerprint of a value. It names the
// value without revealing it and is stable across surfaces.
func Fingerprint(value string) string {
	sum := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(sum[:])[:8]
}

// DiffKeys returns the keys that differ between two value sets, for the
// settings test that proves both write paths produce one key set.
func DiffKeys(first, second map[string]string) []string {
	differing := make([]string, 0)
	for key := range first {
		if _, ok := second[key]; !ok {
			differing = append(differing, key)
		}
	}
	for key := range second {
		if _, ok := first[key]; !ok {
			differing = append(differing, key)
		}
	}
	slices.Sort(differing)
	return differing
}
