package base

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrMissingCredential means a Connector needed a credential that is not
// mounted. The message names the Secret and key so the operator can fix the
// mount; it never includes a value.
var ErrMissingCredential = errors.New("credential is not mounted")

// Credentials resolves a Secret reference to its value at call time, so a
// rotated Secret takes effect without a restart.
type Credentials interface {
	// Get returns the value of key in secretName. An empty secretName means the
	// credentials Secret mounted at the configured root.
	Get(ctx context.Context, secretName, key string) (string, error)
}

// FileCredentials reads values from a credentials Secret mounted as a
// directory. The default Secret's keys sit directly under the root; an
// additional Secret is mounted under a subdirectory named after it.
type FileCredentials struct {
	// Root is the mount path of the credentials Secret.
	Root string
	// DefaultSecret is the name of the Secret mounted at Root.
	DefaultSecret string
}

// Get reads the named key. A key that is not present is an error that names the
// reference, never a value.
func (c *FileCredentials) Get(_ context.Context, secretName, key string) (string, error) {
	if key == "" {
		return "", fmt.Errorf("%w: no key was configured", ErrMissingCredential)
	}
	candidates := make([]string, 0, 2)
	if secretName == "" || secretName == c.DefaultSecret {
		candidates = append(candidates, filepath.Join(c.Root, key))
	}
	if secretName != "" {
		candidates = append(candidates, filepath.Join(c.Root, secretName, key))
	}
	for _, candidate := range candidates {
		raw, err := os.ReadFile(candidate)
		if err == nil {
			return strings.TrimSpace(string(raw)), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("read credential %s/%s: %w", secretName, key, err)
		}
	}
	return "", fmt.Errorf("%w: %s/%s", ErrMissingCredential, secretName, key)
}

// MapCredentials is an in-memory Credentials for tests. Keys of the form
// "Secret/key" and "key" are both accepted.
type MapCredentials map[string]string

// Get reads the key from the map.
func (m MapCredentials) Get(_ context.Context, secretName, key string) (string, error) {
	for _, candidate := range []string{secretName + "/" + key, key} {
		if value, ok := m[candidate]; ok {
			return value, nil
		}
	}
	return "", fmt.Errorf("%w: %s/%s", ErrMissingCredential, secretName, key)
}
