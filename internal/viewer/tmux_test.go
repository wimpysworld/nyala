package viewer

import (
	"errors"
	"slices"
	"testing"
)

func TestInspectTmuxSession(t *testing.T) {
	const (
		pane        = "%7"
		paneTTY     = "/dev/pts/12"
		sixelClient = "0\txterm-256color\tRGB,sixel\t1\t@1 @8 \n"
		kittyClient = "0\txterm-kitty\tRGB,title\t1\t@1 \n"
	)
	cases := []struct {
		name, rows       string
		ttyMatches       bool
		clientErr        error
		wantClients      int
		wantSixelSupport bool
	}{
		{"matching attached client", sixelClient, true, nil, 1, true},
		{"multiple Sixel clients", sixelClient + sixelClient, true, nil, 2, true},
		{"mixed linked clients", sixelClient + kittyClient, true, nil, 2, false},
		{"DA1-capable tmux with Kitty host", kittyClient, true, nil, 1, false},
		{"known outer multiplexer advertises Sixel", "0\ttmux-256color\tRGB,sixel\t1\t@1 \n", true, nil, 1, false},
		{"unknown terminal identity", "0\t\tRGB,sixel\t1\t@1 \n", true, nil, 1, false},
		{"unrelated client", sixelClient + "0\txterm-kitty\tRGB\t1\t@10 \n", true, nil, 1, true},
		{"control client", sixelClient + "1\t\t\t0\t@1 \n", true, nil, 1, true},
		{"feature token substring", "0\txterm-256color\tRGB,nosixel\t1\t@1 \n", true, nil, 1, false},
		{"no advertised features", "0\txterm-256color\t\t1\t@1 \n", true, nil, 1, false},
		{"malformed client output", "xterm-256color\tsixel\n", true, nil, 0, false},
		{"unknown second client's windows", sixelClient + "0\txterm-kitty\tRGB\t1\t\n", true, nil, 0, false},
		{"no attached clients", "", true, nil, 0, false},
		{"failed client query", sixelClient, true, errors.New("client list failed"), 0, false},
		{"stale inherited pane", sixelClient, false, nil, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ttyMatches := func(path string) bool { return path == paneTTY && tc.ttyMatches }
			query := func(args ...string) (string, error) {
				switch {
				case slices.Equal(args, []string{"display-message", "-p", "-t", pane, "#{pane_id}\t#{pane_tty}\t#{window_id}"}):
					return pane + "\t" + paneTTY + "\t@1\n", nil
				case slices.Equal(args, []string{"list-clients", "-F", "#{client_control_mode}\t#{client_termname}\t#{client_termfeatures}\t#{client_utf8}\t#{W:#{window_id} }"}):
					if !tc.ttyMatches {
						t.Fatal("queried clients for the wrong pane terminal")
					}
					return tc.rows, tc.clientErr
				default:
					t.Fatalf("unexpected tmux query: %v", args)
					return "", nil
				}
			}
			session, verified := inspectTmuxSession(pane, ttyMatches, query)
			if verified != (tc.wantClients > 0) || len(session.clients) != tc.wantClients {
				t.Fatalf("verified = %t, clients = %v, want %d verified clients", verified, session.clients, tc.wantClients)
			}
			if session.supportsSixel() != tc.wantSixelSupport {
				t.Fatalf("Sixel support = %t, want %t for %v", session.supportsSixel(), tc.wantSixelSupport, session.clients)
			}
		})
	}
}

func TestTmuxSocket(t *testing.T) {
	for input, want := range map[string]string{
		"/tmp/tmux,other,42,0": "/tmp/tmux,other",
		"/tmp/tmux,42,0":       "/tmp/tmux",
		"/tmp/tmux,,0":         "",
		"/tmp/tmux,42,z":       "",
		"/tmp/tmux,42":         "",
		"tmux,42,0":            "",
	} {
		if got := tmuxSocket(input); got != want {
			t.Errorf("tmuxSocket(%q) = %q, want %q", input, got, want)
		}
	}
}
