package viewer

import (
	"errors"
	"os"
	"strings"
	"testing"
)

func TestConfigureTmuxKitty(t *testing.T) {
	t.Setenv("NYALA_TMUX_KITTY", "1")
	t.Setenv("STY", "")
	t.Setenv("HERDR_ENV", "")
	t.Setenv("ZELLIJ", "")
	if err := os.Unsetenv("ZELLIJ"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, option, limit, transport string
		unverified, fallback, multiple bool
		queryError                     bool
		want                           bool
	}{
		{name: "verified visible passthrough", option: "on", limit: "1048576", want: true},
		{name: "disabled", option: "off"},
		{name: "hidden pane passthrough", option: "all"},
		{name: "unknown pane", unverified: true},
		{name: "fallback geometry", fallback: true},
		{name: "multiple clients", multiple: true},
		{name: "unreadable option", queryError: true},
		{name: "unknown ceiling", option: "on"},
		{name: "ceiling too small for continuation", option: "on", limit: "4096"},
		{name: "shared memory requested", option: "on", limit: "1048576", transport: "shm"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			session := tmuxSession{paneID: "%4", clients: []tmuxClient{{termName: "xterm-kitty", features: "RGB", utf8: true}}}
			if tc.multiple {
				session.clients = append(session.clients, tmuxClient{termName: "xterm-ghostty"})
			}
			session.query = func(args ...string) (string, error) {
				switch strings.Join(args, " ") {
				case "show-options -Apqv -t %4 allow-passthrough":
					if tc.queryError {
						return "", errors.New("tmux option unavailable")
					}
					return tc.option, nil
				case "show-options -sv input-buffer-size":
					return tc.limit, nil
				default:
					t.Fatalf("unexpected tmux query: %q", args)
					return "", nil
				}
			}
			st := State{Cols: 80, Rows: 24}
			got := configureTmuxKitty(&st, session, !tc.unverified, "tmux 3.8", !tc.fallback, tc.transport)
			if got != tc.want || st.kittyTmux != tc.want {
				t.Fatalf("enable = %t, state = %t, want %t", got, st.kittyTmux, tc.want)
			}
			if got && st.kittyTransport != KittyTransportDirect {
				t.Fatalf("transport = %q, want direct", st.kittyTransport)
			}
		})
	}
	t.Setenv("NYALA_TMUX_KITTY", "0")
	if configureTmuxKitty(&State{}, tmuxSession{}, true, "tmux 3.8", true, "direct") {
		t.Fatal("Kitty relay enabled without opt-in")
	}
}

func TestTmuxKittyRefusesKnownNestedClients(t *testing.T) {
	session := tmuxSession{clients: []tmuxClient{{termName: "xterm-kitty", features: "RGB", utf8: true}}}
	if !tmuxKittyClientOK(session, nil) {
		t.Fatal("direct Kitty client refused")
	}
	for _, marker := range []string{"STY=outer", "ZELLIJ=0", "HERDR_ENV=1"} {
		if tmuxKittyClientOK(session, []string{marker}) {
			t.Errorf("nested marker accepted: %s", marker)
		}
	}
	for _, name := range []string{"tmux-256color", "screen-256color"} {
		session.clients[0].termName = name
		if tmuxKittyClientOK(session, nil) {
			t.Errorf("nested terminal accepted: %s", name)
		}
	}
	session.clients[0] = tmuxClient{termName: "xterm-kitty", utf8: true}
	if tmuxKittyClientOK(session, nil) {
		t.Error("client without truecolour accepted")
	}
	session.clients[0] = tmuxClient{termName: "xterm-kitty", features: "RGB"}
	if tmuxKittyClientOK(session, nil) {
		t.Error("client without UTF-8 accepted")
	}
}
