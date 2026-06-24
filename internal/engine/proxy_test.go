package engine

import (
	"strings"
	"testing"
)

// allProxyVars lists every proxy and bypass variable parseProxyEnv reads. Tests
// clear all of them per case so the host environment cannot leak a value in,
// keeping each t.Setenv case hermetic.
var allProxyVars = []string{
	"HTTPS_PROXY",
	"https_proxy",
	"HTTP_PROXY",
	"http_proxy",
	"ALL_PROXY",
	"all_proxy",
	"NO_PROXY",
	"no_proxy",
}

// clearProxyEnv sets every proxy and bypass variable to empty, then applies the
// supplied overrides. It pins the whole environment per case so an inherited
// proxy var on the host cannot change the result.
func clearProxyEnv(t *testing.T, set map[string]string) {
	t.Helper()
	for _, name := range allProxyVars {
		t.Setenv(name, "")
	}
	for name, value := range set {
		t.Setenv(name, value)
	}
}

func TestParseProxyEnvSelectionOrder(t *testing.T) {
	// Each variable must win when it is the first present in precedence order.
	// The case sets its variable plus every later variable, so a regression that
	// reads a later variable first is caught: the value differs per source.
	cases := []struct {
		name   string
		set    map[string]string
		server string
	}{
		{
			name:   "HTTPS_PROXY wins",
			set:    map[string]string{"HTTPS_PROXY": "http://https-upper:1", "https_proxy": "http://https-lower:2", "HTTP_PROXY": "http://http-upper:3", "http_proxy": "http://http-lower:4", "ALL_PROXY": "http://all-upper:5", "all_proxy": "http://all-lower:6"},
			server: "http://https-upper:1",
		},
		{
			name:   "https_proxy wins over the rest",
			set:    map[string]string{"https_proxy": "http://https-lower:2", "HTTP_PROXY": "http://http-upper:3", "http_proxy": "http://http-lower:4", "ALL_PROXY": "http://all-upper:5", "all_proxy": "http://all-lower:6"},
			server: "http://https-lower:2",
		},
		{
			name:   "HTTP_PROXY wins over the rest",
			set:    map[string]string{"HTTP_PROXY": "http://http-upper:3", "http_proxy": "http://http-lower:4", "ALL_PROXY": "http://all-upper:5", "all_proxy": "http://all-lower:6"},
			server: "http://http-upper:3",
		},
		{
			name:   "http_proxy wins over the rest",
			set:    map[string]string{"http_proxy": "http://http-lower:4", "ALL_PROXY": "http://all-upper:5", "all_proxy": "http://all-lower:6"},
			server: "http://http-lower:4",
		},
		{
			name:   "ALL_PROXY wins over all_proxy",
			set:    map[string]string{"ALL_PROXY": "http://all-upper:5", "all_proxy": "http://all-lower:6"},
			server: "http://all-upper:5",
		},
		{
			name:   "all_proxy is the last resort",
			set:    map[string]string{"all_proxy": "http://all-lower:6"},
			server: "http://all-lower:6",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clearProxyEnv(t, c.set)

			cfg, err := parseProxyEnv()
			if err != nil {
				t.Fatalf("parseProxyEnv() error: %v", err)
			}
			if cfg.Server != c.server {
				t.Errorf("Server = %q, want %q", cfg.Server, c.server)
			}
		})
	}
}

func TestParseProxyEnvBypassPrecedence(t *testing.T) {
	// NO_PROXY beats no_proxy, and the chosen value passes verbatim. A value with
	// mixed Chromium and shell syntax confirms no translation happens.
	const upper = ".internal,10.0.0.0/8,*.example.com"
	const lower = "localhost,127.0.0.1"
	cases := []struct {
		name   string
		set    map[string]string
		bypass string
	}{
		{
			name:   "NO_PROXY wins over no_proxy",
			set:    map[string]string{"HTTPS_PROXY": "http://host:3128", "NO_PROXY": upper, "no_proxy": lower},
			bypass: upper,
		},
		{
			name:   "no_proxy used when NO_PROXY empty",
			set:    map[string]string{"HTTPS_PROXY": "http://host:3128", "no_proxy": lower},
			bypass: lower,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clearProxyEnv(t, c.set)

			cfg, err := parseProxyEnv()
			if err != nil {
				t.Fatalf("parseProxyEnv() error: %v", err)
			}
			// Byte-for-byte: the bypass value must not be rewritten or trimmed.
			if cfg.Bypass != c.bypass {
				t.Errorf("Bypass = %q, want verbatim %q", cfg.Bypass, c.bypass)
			}
		})
	}
}

func TestParseProxyEnvStripsCredentials(t *testing.T) {
	// A credentialed URL must yield a credential-free Server while retaining the
	// parsed user and password and flagging HasCredentials.
	clearProxyEnv(t, map[string]string{"HTTPS_PROXY": "http://user:pass@host:3128"})

	cfg, err := parseProxyEnv()
	if err != nil {
		t.Fatalf("parseProxyEnv() error: %v", err)
	}
	if cfg.Server != "http://host:3128" {
		t.Errorf("Server = %q, want %q", cfg.Server, "http://host:3128")
	}
	if strings.Contains(cfg.Server, "user") || strings.Contains(cfg.Server, "pass") || strings.Contains(cfg.Server, "@") {
		t.Errorf("Server %q leaks credentials", cfg.Server)
	}
	if !cfg.HasCredentials {
		t.Error("HasCredentials = false, want true")
	}
	if cfg.User != "user" {
		t.Errorf("User = %q, want %q", cfg.User, "user")
	}
	if cfg.Password != "pass" {
		t.Errorf("Password = %q, want %q", cfg.Password, "pass")
	}
}

func TestParseProxyEnvSchemePassThrough(t *testing.T) {
	// The scheme must survive untouched for both SOCKS and HTTP proxies.
	cases := []struct {
		name   string
		value  string
		server string
	}{
		{"socks5 scheme retained", "socks5://host:1080", "socks5://host:1080"},
		{"http scheme retained", "http://host:3128", "http://host:3128"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clearProxyEnv(t, map[string]string{"ALL_PROXY": c.value})

			cfg, err := parseProxyEnv()
			if err != nil {
				t.Fatalf("parseProxyEnv() error: %v", err)
			}
			if cfg.Server != c.server {
				t.Errorf("Server = %q, want %q", cfg.Server, c.server)
			}
		})
	}
}

func TestParseProxyEnvMalformedURL(t *testing.T) {
	// A value net/url rejects must error, name the source variable, and never
	// echo the value (it may carry credentials). A control byte forces a real
	// url.Parse failure; the embedded token stands in for a leaked secret.
	const secret = "leaktoken"
	clearProxyEnv(t, map[string]string{"HTTPS_PROXY": "http://user:" + secret + "@host\x7f:3128"})

	cfg, err := parseProxyEnv()
	if err == nil {
		t.Fatalf("parseProxyEnv() = %+v, want error", cfg)
	}
	msg := err.Error()
	if !strings.Contains(msg, "HTTPS_PROXY") {
		t.Errorf("error %q does not name the source variable HTTPS_PROXY", msg)
	}
	if strings.Contains(msg, secret) {
		t.Errorf("error %q leaks the credential value", msg)
	}
}

func TestParseProxyEnvSchemeless(t *testing.T) {
	// A scheme-less value parses to an opaque form (Host empty, User nil), so
	// the credential strip would miss it. It must error, return a zero config,
	// and never echo the password. Bare host:port has the same shape.
	const secret = "leaktoken"
	cases := []struct {
		name  string
		value string
	}{
		{"credentialed no scheme", "user:" + secret + "@host:3128"},
		{"bare host port", "host:3128"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clearProxyEnv(t, map[string]string{"HTTPS_PROXY": c.value})

			cfg, err := parseProxyEnv()
			if err == nil {
				t.Fatalf("parseProxyEnv() = %+v, want error", cfg)
			}
			if cfg != (ProxyConfig{}) {
				t.Errorf("cfg = %+v, want zero ProxyConfig", cfg)
			}
			if strings.Contains(err.Error(), secret) {
				t.Errorf("error %q leaks the credential value", err.Error())
			}
		})
	}
}

func TestParseProxyEnvNoProxy(t *testing.T) {
	// With nothing set, parsing yields a zero config and no error: no proxy is
	// not an error.
	clearProxyEnv(t, nil)

	cfg, err := parseProxyEnv()
	if err != nil {
		t.Fatalf("parseProxyEnv() error: %v", err)
	}
	if cfg != (ProxyConfig{}) {
		t.Errorf("cfg = %+v, want zero ProxyConfig", cfg)
	}
	if cfg.HasCredentials {
		t.Error("HasCredentials = true, want false")
	}
}
