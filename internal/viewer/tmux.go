package viewer

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

type tmuxSession struct {
	paneID  string
	clients []tmuxClient
	query   func(args ...string) (string, error)
}

type tmuxClient struct {
	termName string
	features string
	utf8     bool
}

func (c tmuxClient) hasFeature(feature string) bool {
	return slices.Contains(strings.Split(c.features, ","), feature)
}

// supportsSixel requires every attached viewing client to advertise Sixel.
// A control-mode client does not display the pane image.
func (s tmuxSession) supportsSixel() bool {
	return len(s.clients) > 0 && !slices.ContainsFunc(s.clients, func(c tmuxClient) bool {
		return c.termName == "" || tmuxClientIsNested(c.termName) || !c.hasFeature("sixel")
	})
}

func tmuxClientIsNested(termName string) bool {
	name := strings.ToLower(termName)
	return strings.HasPrefix(name, "tmux") || strings.HasPrefix(name, "screen")
}

// hasOuterMultiplexer detects known outer mux markers after verifying that
// tmux itself owns the current terminal. Missing markers do not prove that
// the session is not nested.
func hasOuterMultiplexer(environ []string) bool {
	return slices.ContainsFunc(environ, func(entry string) bool {
		return strings.HasPrefix(entry, "ZELLIJ=") ||
			strings.HasPrefix(entry, "STY=") && entry != "STY=" ||
			entry == "HERDR_ENV=1"
	})
}

// inspectTmux binds read-only queries to the socket of the tmux server named
// by the environment. Matching the pane's tty rejects a stale inherited pane
// id, such as one inherited by a nested multiplexer.
func inspectTmux(tty *os.File) (tmuxSession, bool) {
	socket := tmuxSocket(os.Getenv("TMUX"))
	if socket == "" {
		return tmuxSession{}, false
	}
	info, err := tty.Stat()
	if err != nil {
		return tmuxSession{}, false
	}
	return inspectTmuxSession(os.Getenv("TMUX_PANE"), func(path string) bool {
		other, err := os.Stat(path)
		return err == nil && os.SameFile(info, other)
	}, func(args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		//nolint:gosec // The socket is absolute. Read-only commands use fixed arguments and a verified numeric pane ID.
		out, err := exec.CommandContext(ctx, "tmux", append([]string{"-N", "-S", socket}, args...)...).Output()
		return string(out), err
	})
}

// inspectTmuxSession selects the clients attached to any session containing
// the pane's window. This includes linked windows and clients that can switch
// to the pane later, but excludes clients on unrelated sessions.
func inspectTmuxSession(paneID string, ttyMatches func(string) bool, query func(...string) (string, error)) (tmuxSession, bool) {
	if !tmuxID(paneID, "%") {
		return tmuxSession{}, false
	}
	const paneFormat = "#{pane_id}\t#{pane_tty}\t#{window_id}"
	pane, err := query("display-message", "-p", "-t", paneID, paneFormat)
	fields := strings.Split(strings.TrimSuffix(pane, "\n"), "\t")
	if err != nil || len(fields) != 3 || fields[0] != paneID || !tmuxID(fields[2], "@") || !ttyMatches(fields[1]) {
		return tmuxSession{}, false
	}
	windowID := fields[2]

	const clientFormat = "#{client_control_mode}\t#{client_termname}\t#{client_termfeatures}\t#{client_utf8}\t#{W:#{window_id} }"
	list, err := query("list-clients", "-F", clientFormat)
	if err != nil || list == "" {
		return tmuxSession{}, false
	}
	session := tmuxSession{paneID: paneID, query: query}
	for line := range strings.SplitSeq(strings.TrimSuffix(list, "\n"), "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) != 5 || (fields[0] != "0" && fields[0] != "1") {
			return tmuxSession{}, false
		}
		if fields[0] != "0" {
			continue
		}
		windows := strings.Fields(fields[4])
		if len(windows) == 0 {
			return tmuxSession{}, false
		}
		if !slices.Contains(windows, windowID) {
			continue
		}
		session.clients = append(session.clients, tmuxClient{termName: fields[1], features: fields[2], utf8: fields[3] == "1"})
	}
	return session, len(session.clients) > 0
}

// tmuxSocket takes the two suffix fields from the right because a socket
// path can itself contain a comma.
func tmuxSocket(env string) string {
	sessionSep := strings.LastIndexByte(env, ',')
	if sessionSep < 0 || !tmuxID(env[sessionSep+1:], "") {
		return ""
	}
	server := env[:sessionSep]
	pidSep := strings.LastIndexByte(server, ',')
	if pidSep < 0 || !tmuxID(server[pidSep+1:], "") || !filepath.IsAbs(server[:pidSep]) {
		return ""
	}
	return server[:pidSep]
}

func tmuxID(value, prefix string) bool {
	id, ok := strings.CutPrefix(value, prefix)
	return ok && id != "" && leadingDigits(id) == id
}
