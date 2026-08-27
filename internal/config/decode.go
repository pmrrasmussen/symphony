package config

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// This file holds the generic front-matter decode machinery every per-section
// decoder builds on: the small typed-value accessors over a raw
// map[string]any block, and sourceSnapshot, which resolves $VAR environment
// references and repository-relative paths and freezes what it read so a
// reload can tell whether either changed.

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for key := range m {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func object(parent map[string]any, key string) (map[string]any, error) {
	v, exists := parent[key]
	if !exists {
		return map[string]any{}, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("invalid configuration: %s must be an object", key)
	}
	return m, nil
}

func stringValue(m map[string]any, key string) (string, error) {
	v, exists := m[key]
	if !exists {
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("invalid configuration: %s must be a string", key)
	}
	return s, nil
}

func stringDefault(m map[string]any, key, fallback string) (string, error) {
	v, exists := m[key]
	if !exists {
		return fallback, nil
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("invalid configuration: %s must be a string", key)
	}
	return s, nil
}

func stringList(m map[string]any, key string) ([]string, error) {
	v, exists := m[key]
	if !exists {
		return nil, nil
	}
	values, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("invalid configuration: %s must be a list of strings", key)
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		s, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("invalid configuration: %s must be a list of strings", key)
		}
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out, nil
}

func integer(m map[string]any, key string, fallback int) (int, error) {
	v, exists := m[key]
	if !exists {
		return fallback, nil
	}
	i, ok := v.(int)
	if !ok {
		return 0, fmt.Errorf("invalid configuration: %s must be an integer", key)
	}
	return i, nil
}

func durationMS(m map[string]any, key string, fallback int) (time.Duration, error) {
	i, err := integer(m, key, fallback)
	if err != nil {
		return 0, err
	}
	return time.Duration(i) * time.Millisecond, nil
}

func script(m map[string]any, key string) (string, error) {
	v, exists := m[key]
	if !exists || v == nil {
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("invalid configuration: %s must be a string", key)
	}
	return s, nil
}

func stateLimits(v any) (map[string]int, error) {
	out := map[string]int{}
	if v == nil {
		return out, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("invalid configuration: max_concurrent_agents_by_state must be an object")
	}
	for state, value := range m {
		limit, ok := value.(int)
		// This map deliberately ignores invalid per-state entries, as specified.
		if !ok || limit <= 0 || strings.TrimSpace(state) == "" {
			continue
		}
		out[strings.ToLower(strings.TrimSpace(state))] = limit
	}
	return out, nil
}

func pathValue(m map[string]any, key, fallback, base string, sources *sourceSnapshot) (string, error) {
	v, exists := m[key]
	if !exists {
		return normalizePath(fallback, base), nil
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("invalid configuration: %s must be a path string", key)
	}
	s, err := sources.expand(s, "workspace."+key)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(s) == "" {
		return "", fmt.Errorf("invalid configuration: %s must not be empty", key)
	}
	return normalizePath(s, base), nil
}

func optionalPathValue(m map[string]any, key, base string, sources *sourceSnapshot) (string, error) {
	v, exists := m[key]
	if !exists || v == nil {
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("invalid configuration: %s must be a path string", key)
	}
	if strings.TrimSpace(s) == "" {
		return "", nil
	}
	s, err := sources.expand(s, "workspace."+key)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(s) == "" {
		return "", fmt.Errorf("invalid configuration: workspace.%s environment reference is unresolved", key)
	}
	return normalizePath(s, base), nil
}

type fileSource struct {
	content []byte
	err     error
}

// sourceSnapshot freezes process environment values and caches referenced
// files while one candidate is decoded. The resulting digest deliberately
// includes only source identities and bytes, never values in errors or logs.
type sourceSnapshot struct {
	workflow    []byte
	environment map[string]string
	references  map[string]string
	files       map[string]fileSource
}

func newSourceSnapshot(workflow []byte, overlay map[string]string) *sourceSnapshot {
	environment := make(map[string]string)
	for _, assignment := range os.Environ() {
		name, value, found := strings.Cut(assignment, "=")
		if found {
			environment[name] = value
		}
	}
	for name, value := range overlay {
		environment[name] = value
	}
	return &sourceSnapshot{workflow: workflow, environment: environment, references: map[string]string{}, files: map[string]fileSource{}}
}

func (s *sourceSnapshot) expand(value, field string) (string, error) {
	if !strings.HasPrefix(value, "$") {
		return value, nil
	}
	name := strings.TrimPrefix(value, "$")
	if !ValidEnvironmentName(name) {
		return "", fmt.Errorf("invalid configuration: %s must use exact $VARNAME environment syntax", field)
	}
	resolved := s.environment[name]
	s.references[name] = resolved
	return resolved, nil
}

func (s *sourceSnapshot) readFile(path string) ([]byte, error) {
	if source, ok := s.files[path]; ok {
		return append([]byte(nil), source.content...), source.err
	}
	content, err := os.ReadFile(path)
	s.files[path] = fileSource{content: append([]byte(nil), content...), err: err}
	return content, err
}

func (s *sourceSnapshot) digest() [sha256.Size]byte {
	hash := sha256.New()
	_, _ = hash.Write(s.workflow)
	names := make([]string, 0, len(s.references))
	for name := range s.references {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		_, _ = fmt.Fprintf(hash, "\x00env:%s\x00%s", name, s.references[name])
	}
	paths := make([]string, 0, len(s.files))
	for path := range s.files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		source := s.files[path]
		_, _ = fmt.Fprintf(hash, "\x00file:%s\x00", path)
		_, _ = hash.Write(source.content)
		if source.err != nil {
			_, _ = fmt.Fprint(hash, "\x00unreadable")
		}
	}
	var digest [sha256.Size]byte
	copy(digest[:], hash.Sum(nil))
	return digest
}

func normalizePath(value, base string) string {
	if value == "~" || strings.HasPrefix(value, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			if value == "~" {
				value = home
			} else {
				value = filepath.Join(home, value[2:])
			}
		}
	}
	if !filepath.IsAbs(value) {
		value = filepath.Join(base, value)
	}
	abs, err := filepath.Abs(filepath.Clean(value))
	if err != nil {
		return filepath.Clean(value)
	}
	return abs
}

func logRootOrDefault(value string) string {
	if strings.TrimSpace(value) == "" {
		return ".symphony/logs"
	}
	return value
}

func stringsLower(values []string) []string {
	for i := range values {
		values[i] = strings.ToLower(strings.TrimSpace(values[i]))
	}
	return values
}
