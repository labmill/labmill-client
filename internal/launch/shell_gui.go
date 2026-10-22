package launch

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"golang.org/x/crypto/ssh"

	"labmill-client/internal/sshx"
)

// ErrTerminalSignInLater is returned by RunTerminal when the helper holds no
// login for the host and signing in isn't allowed now (a chat turn runs and
// owns the sign-in dialogs).
var ErrTerminalSignInLater = errors.New("Sign-in is possible once the assistant has finished; try again then")

// termConnectKey names one serialized connect: a terminal connects to one
// host at a time per runtime.
type termConnectKey struct {
	cc    *connCache
	alias string
}

var termConnects sync.Map // termConnectKey -> chan struct{} (capacity 1)

// acquireTermConnect takes the connect slot of alias; waited says it had to
// wait for another terminal's connect. It returns a release func, or an
// error when ctx ended first.
func acquireTermConnect(ctx context.Context, k termConnectKey) (release func(), waited bool, err error) {
	v, _ := termConnects.LoadOrStore(k, make(chan struct{}, 1))
	sem := v.(chan struct{})
	select {
	case sem <- struct{}{}:
	default:
		waited = true
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			return nil, true, ctx.Err()
		}
	}
	return func() { <-sem }, waited, nil
}

// terminalClient returns a connection to alias for a terminal.
//
// With allowSignIn false (a chat turn runs and owns the sign-in dialogs) it
// opens no dialog and clears no refusal: it uses a login the helper already
// holds, else ErrTerminalSignInLater.
//
// Otherwise connects are serialized per host. A user's request clears an
// earlier "password rejected" refusal of the host (and its institution's
// other hosts) like /shell does, but a request that had to wait for another
// connect doesn't: if that one's password was rejected meanwhile, this one
// fails with the refusal instead of prompting again (M58: one attempt).
func (cc *connCache) terminalClient(ctx context.Context, alias string, allowSignIn bool) (*ssh.Client, error) {
	if !allowSignIn {
		cl, err := connectCached(ctx, alias)
		if err != nil {
			return nil, ErrTerminalSignInLater
		}
		return cl, nil
	}
	release, waited, err := acquireTermConnect(ctx, termConnectKey{cc, alias})
	if err != nil {
		return nil, err
	}
	defer release()
	if waited {
		if msg := cc.deniedMsg(alias); msg != "" {
			return nil, errors.New(msg)
		}
	} else {
		cc.forgetDenied(alias)
	}
	cl, err := cc.connect(ctx, alias)
	if err != nil {
		if errors.Is(err, sshx.ErrPasswordRejected) {
			cc.markDenied(alias)
		}
		return nil, err
	}
	return cl, nil
}

// RunTerminal is the desktop app's terminal (M62.2): it connects to the
// cluster's login node through the helper like OpenShell does (the existing
// sign-in is reused, otherwise the usual login dialog, see terminalClient
// for when none is allowed), calls connected once the connection is up, runs
// the shell with opts (RunShell: a pseudo-terminal on a new session of the
// helper's connection) and closes the connection when it ends. The
// connection to the helper stays open meanwhile, so its idle timer doesn't
// fire. It returns the remote exit status. Nothing typed or shown in the
// shell is logged: labmill.log gets "terminal opened/closed on <alias>" only.
func (r *Runtime) RunTerminal(ctx context.Context, cluster string, opts sshx.ShellOptions, connected func(), allowSignIn bool) (int, error) {
	if r.Env == nil || r.conns == nil {
		return 0, errors.New("the terminal isn't available here")
	}
	if strings.ContainsAny(cluster+opts.Dir, "\n\r\x00") {
		return 0, errors.New("the cluster and folder can't contain line breaks or NUL")
	}
	alias, err := ShellAlias(r.Env.Config, cluster)
	if err != nil {
		return 0, err
	}
	cl, err := r.conns.terminalClient(ctx, alias, allowSignIn)
	if err != nil {
		return 0, err
	}
	defer cl.Close()
	logPath := r.LogPath
	appendLog(logPath, "shell", []string{fmt.Sprintf("terminal opened on %s", alias)})
	defer appendLog(logPath, "shell", []string{fmt.Sprintf("terminal closed on %s", alias)})
	if connected != nil {
		connected()
	}
	return sshx.RunShell(ctx, cl, opts)
}

// ClusterRef is a cluster name and its login host alias.
type ClusterRef struct{ Name, Alias string }

// TerminalClusters returns the clusters the desktop terminal can open, in
// config order (clusters without an ssh_host are left out).
func (r *Runtime) TerminalClusters() []ClusterRef {
	if r.Env == nil || r.Env.Config == nil {
		return nil
	}
	var out []ClusterRef
	for _, c := range r.Env.Config.List() {
		if c.SSHHost != "" {
			out = append(out, ClusterRef{Name: c.Name, Alias: c.SSHHost})
		}
	}
	return out
}
