// Package githubmcp is the read-only GitHub MCP provider. It holds a read
// credential and exposes no branch, commit, pull-request or merge operation.
// The publishing path is a separate process and credential.
package githubmcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/a-hagi613/pagerless/internal/mcpserver"
)

const (
	maxFileBytes = 256 << 10
	maxEntries   = 200
	// JSON schema keys reused by the tool descriptions.
	keyPath                 = "path"
	keyAdditionalProperties = "additionalProperties"
	keyType                 = "type"
	keyObject               = "object"
	keyProperties           = "properties"
	typeString              = "string"
)

// Server is the read-only GitHub provider.
type Server struct {
	Owner      string
	Name       string
	APIURL     string
	PathPrefix string
	Token      func() (string, error)
	HTTP       *http.Client
}

// New builds the provider.
func New(apiURL, repository, pathPrefix string, token func() (string, error)) (*Server, error) {
	owner, name, found := strings.Cut(strings.TrimSpace(repository), "/")
	if !found || owner == "" || name == "" || strings.Contains(name, "/") {
		return nil, fmt.Errorf("github mcp: %q is not an owner/name repository", repository)
	}
	if strings.TrimSpace(apiURL) == "" {
		apiURL = "https://api.github.com"
	}
	return &Server{
		Owner: owner, Name: name, APIURL: strings.TrimSuffix(apiURL, "/"),
		PathPrefix: pathPrefix, Token: token, HTTP: &http.Client{Timeout: 30 * time.Second},
	}, nil
}

// Tools returns the provider's read tools.
func (s *Server) Tools() []mcpserver.Tool {
	return []mcpserver.Tool{
		{
			Name:        "github_repository",
			Description: "Read the configured repository's name, default branch and permissions.",
			InputSchema: schemaOf(map[string]any{
				keyType: keyObject, keyProperties: map[string]any{}, keyAdditionalProperties: false,
			}),
			Call: s.repository,
		},
		{
			Name:        "github_get_file",
			Description: "Read one text file from the configured repository at a reference.",
			InputSchema: schemaOf(map[string]any{
				"type": "object",
				"properties": map[string]any{
					keyPath: map[string]any{keyType: typeString},
					"ref":   map[string]any{keyType: typeString},
				},
				"required":              []string{keyPath},
				keyAdditionalProperties: false,
			}),
			Call: s.getFile,
		},
		{
			Name:        "github_list_directory",
			Description: "List one directory of the configured repository at a reference.",
			InputSchema: schemaOf(map[string]any{
				"type": "object",
				"properties": map[string]any{
					keyPath: map[string]any{keyType: typeString},
					"ref":   map[string]any{keyType: typeString},
				},
				keyAdditionalProperties: false,
			}),
			Call: s.listDirectory,
		},
	}
}

type repositoryResponse struct {
	FullName      string          `json:"full_name"`
	DefaultBranch string          `json:"default_branch"`
	Private       bool            `json:"private"`
	Permissions   map[string]bool `json:"permissions"`
}

func (s *Server) repository(ctx context.Context, _ json.RawMessage) (string, error) {
	var repository repositoryResponse
	if err := s.get(ctx, "", &repository); err != nil {
		return "", err
	}
	return encode(map[string]any{
		"repository": repository.FullName, "defaultBranch": repository.DefaultBranch,
		"private": repository.Private, "permissions": repository.Permissions,
	})
}

type fileArguments struct {
	Path string `json:"path"`
	Ref  string `json:"ref"`
}

func (s *Server) getFile(ctx context.Context, raw json.RawMessage) (string, error) {
	var arguments fileArguments
	if err := json.Unmarshal(raw, &arguments); err != nil {
		return "", errors.New("the file arguments are not valid")
	}
	path, err := s.path(arguments.Path)
	if err != nil {
		return "", err
	}
	var file struct {
		Type     string `json:"type"`
		Encoding string `json:"encoding"`
		Content  string `json:"content"`
		Size     int    `json:"size"`
	}
	query := ""
	if arguments.Ref != "" {
		query = "?ref=" + url.QueryEscape(arguments.Ref)
	}
	if err := s.get(ctx, "/contents/"+escapePath(path)+query, &file); err != nil {
		return "", err
	}
	if file.Type != "file" {
		return "", fmt.Errorf("%q is not a file", path)
	}
	if file.Size > maxFileBytes {
		return "", fmt.Errorf("the file exceeds the %d byte read bound", maxFileBytes)
	}
	if file.Encoding != "" && file.Encoding != "base64" {
		return "", fmt.Errorf("the file uses the unsupported encoding %q", file.Encoding)
	}
	content, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(file.Content, "\n", ""))
	if err != nil {
		return "", fmt.Errorf("decode the file: %w", err)
	}
	if len(content) > maxFileBytes {
		return "", fmt.Errorf("the file exceeds the %d byte read bound", maxFileBytes)
	}
	return string(content), nil
}

func (s *Server) listDirectory(ctx context.Context, raw json.RawMessage) (string, error) {
	var arguments fileArguments
	if err := json.Unmarshal(raw, &arguments); err != nil {
		return "", errors.New("the directory arguments are not valid")
	}
	path := strings.Trim(strings.TrimSpace(arguments.Path), "/")
	if path != "" {
		var err error
		if path, err = s.path(path); err != nil {
			return "", err
		}
	}
	var entries []struct {
		Name string `json:"name"`
		Path string `json:"path"`
		Type string `json:"type"`
		Size int    `json:"size"`
	}
	query := ""
	if arguments.Ref != "" {
		query = "?ref=" + url.QueryEscape(arguments.Ref)
	}
	endpoint := "/contents"
	if path != "" {
		endpoint += "/" + escapePath(path)
	}
	if err := s.get(ctx, endpoint+query, &entries); err != nil {
		return "", err
	}
	if len(entries) > maxEntries {
		entries = entries[:maxEntries]
	}
	return encode(entries)
}

// path validates a repository-relative path against the configured prefix.
func (s *Server) path(requested string) (string, error) {
	path := strings.TrimPrefix(strings.TrimSpace(requested), "/")
	if path == "" || strings.Contains(path, "..") || strings.Contains(path, `\`) ||
		strings.Contains(path, "\x00") {
		return "", errors.New("the path is not a relative repository path")
	}
	prefix := strings.Trim(strings.TrimSpace(s.PathPrefix), "/")
	if prefix == "" {
		return path, nil
	}
	if path != prefix && !strings.HasPrefix(path, prefix+"/") {
		return "", fmt.Errorf("the path is outside the configured source directory %q", prefix)
	}
	return path, nil
}

func (s *Server) get(ctx context.Context, path string, result any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet,
		s.APIURL+"/repos/"+s.Owner+"/"+s.Name+path, nil)
	if err != nil {
		return fmt.Errorf("build the GitHub request: %w", err)
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	request.Header.Set("User-Agent", "pagerless")
	if s.Token != nil {
		token, err := s.Token()
		if err != nil {
			return fmt.Errorf("read the GitHub credential: %w", err)
		}
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := s.HTTP.Do(request)
	if err != nil {
		return fmt.Errorf("call GitHub: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("read the GitHub response: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("GitHub answered HTTP %d", response.StatusCode)
	}
	if err := json.Unmarshal(raw, result); err != nil {
		return fmt.Errorf("decode the GitHub response: %w", err)
	}
	return nil
}

func escapePath(path string) string {
	segments := strings.Split(strings.Trim(path, "/"), "/")
	for index, segment := range segments {
		segments[index] = url.PathEscape(segment)
	}
	return strings.Join(segments, "/")
}

func encode(value any) (string, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("encode the result: %w", err)
	}
	return string(raw), nil
}

func schemaOf(value any) json.RawMessage {
	raw, err := json.Marshal(value)
	if err != nil {
		panic("githubmcp: encode a fixed schema: " + err.Error())
	}
	return raw
}
