package engine

import (
	"fmt"
	"net/url"
	"os"
)

// ProxyConfig holds the proxy settings parsed from the environment, ready for
// the launch path. Server is the credential-free --proxy-server value
// (scheme://host[:port]); the scheme passes through unchanged. Bypass is the
// verbatim NO_PROXY/no_proxy value for --proxy-bypass-list. User and Password
// carry any credentials stripped from the URL for the CDP auth listener.
// HasCredentials is true only when a username was present. A zero ProxyConfig
// means no proxy is configured.
type ProxyConfig struct {
	Server         string
	Bypass         string
	User           string
	Password       string
	HasCredentials bool
}

// proxyVars lists the proxy environment variables tried, in precedence order.
// HTTPS variants win over HTTP, and the explicit scheme variants win over the
// catch-all ALL_PROXY pair.
var proxyVars = []string{
	"HTTPS_PROXY",
	"https_proxy",
	"HTTP_PROXY",
	"http_proxy",
	"ALL_PROXY",
	"all_proxy",
}

// bypassVars lists the bypass environment variables tried, in precedence order.
var bypassVars = []string{
	"NO_PROXY",
	"no_proxy",
}

// parseProxyEnv selects and parses the proxy configuration from the
// environment. It reads the six proxy variables in precedence order (HTTPS_PROXY
// down to all_proxy) and uses the first non-empty one, then reads the two bypass
// variables (NO_PROXY then no_proxy) and uses the first non-empty value
// verbatim. It parses the selected proxy URL with net/url, strips any
// credentials into the config, and builds a credential-free scheme://host[:port]
// server string with the scheme unchanged. A malformed proxy URL returns an
// error naming the source variable without echoing the value, since the value
// may carry credentials. With no proxy variable set it returns a zero
// ProxyConfig and a nil error, because no proxy is not an error. The function is
// pure beyond reading the environment, so it is hermetically testable.
func parseProxyEnv() (ProxyConfig, error) {
	var raw, source string
	for _, name := range proxyVars {
		if v := os.Getenv(name); v != "" {
			raw, source = v, name
			break
		}
	}

	var cfg ProxyConfig
	// Bypass passes verbatim. NO_PROXY and Chromium's --proxy-bypass-list
	// overlap but differ (leading-dot prefixes, CIDR ranges); the gap is
	// documented, not translated, so a mismatch is a known limitation.
	for _, name := range bypassVars {
		if v := os.Getenv(name); v != "" {
			cfg.Bypass = v
			break
		}
	}

	if raw == "" {
		return cfg, nil
	}

	parsed, err := url.Parse(raw)
	if err != nil {
		// Do not wrap err: net/url embeds the raw input in its message, which
		// may carry credentials. Name the source variable only.
		return ProxyConfig{}, fmt.Errorf("invalid proxy URL in %s", source)
	}
	// Require a real scheme and host. A scheme-less value like
	// user:pass@host:8080 parses to an opaque form (Host empty, User nil), so
	// the credential strip below would miss it and the password would reach
	// Server. Reject anything without a scheme://host authority.
	if parsed.Scheme == "" || parsed.Host == "" {
		return ProxyConfig{}, fmt.Errorf("invalid proxy URL in %s", source)
	}

	if parsed.User != nil {
		cfg.User = parsed.User.Username()
		cfg.Password, _ = parsed.User.Password()
		cfg.HasCredentials = cfg.User != ""
	}
	// Build Server explicitly from scheme and host only, never parsed.String(),
	// so no userinfo, path, query, or opaque can leak into --proxy-server.
	cfg.Server = parsed.Scheme + "://" + parsed.Host

	return cfg, nil
}
