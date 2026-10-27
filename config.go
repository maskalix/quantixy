package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// Service is one domain → container mapping from services.yaml.
type Service struct {
	Container          string        `yaml:"container"`
	Host               string        `yaml:"host"` // upstream hostname, defaults to the container name
	Port               int           `yaml:"port"`
	Protocol           string        `yaml:"protocol"`
	Websocket          bool          `yaml:"websocket"`
	Timeout            time.Duration `yaml:"timeout"` // idle timeout, defaults to TIMEOUT_MINUTES
	InsecureSkipVerify bool          `yaml:"insecure_skip_verify"`
}

var (
	domainRe    = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$`)
	containerRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)
)

func loadConfig(path string, defTimeout time.Duration) (map[string]*Service, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	raw := map[string]*Service{}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)                                               // typos are errors, not silently ignored
	if err := dec.Decode(&raw); err != nil && !errors.Is(err, io.EOF) { // io.EOF: empty file
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	out := make(map[string]*Service, len(raw))
	for domain, s := range raw {
		d := strings.TrimSuffix(strings.ToLower(domain), ".")
		if s == nil {
			return nil, fmt.Errorf("%s: empty service", domain)
		}
		if _, dup := out[d]; dup {
			return nil, fmt.Errorf("%s: defined twice", d)
		}
		if !domainRe.MatchString(d) || len(d) > 253 {
			return nil, fmt.Errorf("%q: invalid domain", domain)
		}
		if !containerRe.MatchString(s.Container) {
			return nil, fmt.Errorf("%s: invalid or missing container name %q", d, s.Container)
		}
		if s.Host == "" {
			s.Host = s.Container
		}
		if !domainRe.MatchString(strings.ToLower(s.Host)) && !containerRe.MatchString(s.Host) {
			return nil, fmt.Errorf("%s: invalid host %q", d, s.Host)
		}
		if s.Port < 1 || s.Port > 65535 {
			return nil, fmt.Errorf("%s: invalid port %d", d, s.Port)
		}
		s.Protocol = strings.ToLower(s.Protocol)
		switch s.Protocol {
		case "":
			s.Protocol = "http"
		case "http", "https":
		default:
			return nil, fmt.Errorf("%s: protocol must be http or https", d)
		}
		if s.Timeout < 0 {
			return nil, fmt.Errorf("%s: negative timeout", d)
		}
		if s.Timeout == 0 {
			s.Timeout = defTimeout
		}
		out[d] = s
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s: no services configured", path)
	}
	return out, nil
}
