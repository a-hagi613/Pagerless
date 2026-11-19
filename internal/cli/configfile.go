package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"sigs.k8s.io/yaml"
)

// configFile is the CLI's local configuration. It holds one server URL and one
// optional session token, and nothing else: no AI key, no repository token, and
// no cluster credential ever reaches this file.
type configFile struct {
	// Server is the Portal base URL.
	Server string `json:"server,omitempty"`
	// SessionToken is the signed session cookie that `login` obtained.
	SessionToken string `json:"sessionToken,omitempty"`
}

// configPath returns the configuration file path, honouring --config, then
// $PAGERLESS_CONFIG, then the XDG base directory.
func configPath(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	if path := os.Getenv("PAGERLESS_CONFIG"); path != "" {
		return path, nil
	}
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("find the home directory: %w", err)
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "pagerless", "config.yaml"), nil
}

// loadConfig reads the configuration file. A missing file is an empty
// configuration, not an error.
func loadConfig(override string) (configFile, error) {
	path, err := configPath(override)
	if err != nil {
		return configFile{}, err
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return configFile{}, nil
	}
	if err != nil {
		return configFile{}, fmt.Errorf("read %s: %w", path, err)
	}
	var fields map[string]string
	if err := yaml.Unmarshal(raw, &fields); err != nil {
		return configFile{}, fmt.Errorf("parse %s: %w", path, err)
	}
	config := configFile{}
	for key, value := range fields {
		switch key {
		case modeServer:
			config.Server = value
		case "sessionToken":
			config.SessionToken = value
		default:
			return configFile{}, fmt.Errorf(
				"%s holds %q, which is not a setting: the file may hold only server and sessionToken", path, key)
		}
	}
	return config, nil
}

// saveConfig writes the configuration with mode 0600, creating its directory
// with mode 0700. The session token is a credential, so the file is never
// group- or world-readable.
func saveConfig(override string, config configFile) (string, error) {
	path, err := configPath(override)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("create the configuration directory: %w", err)
	}
	raw, err := yaml.Marshal(config)
	if err != nil {
		return "", fmt.Errorf("encode the configuration: %w", err)
	}
	// Write to a temporary file in the same directory and rename, so a reader
	// never sees a partial file and the mode applies from the first byte.
	temporary, err := os.CreateTemp(filepath.Dir(path), ".config-*")
	if err != nil {
		return "", fmt.Errorf("create a temporary configuration file: %w", err)
	}
	temporaryName := temporary.Name()
	defer func() { _ = os.Remove(temporaryName) }()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return "", fmt.Errorf("restrict the configuration file: %w", err)
	}
	if _, err := temporary.Write(raw); err != nil {
		_ = temporary.Close()
		return "", fmt.Errorf("write the configuration file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return "", fmt.Errorf("close the configuration file: %w", err)
	}
	if err := os.Rename(temporaryName, path); err != nil {
		return "", fmt.Errorf("install the configuration file: %w", err)
	}
	return path, nil
}
