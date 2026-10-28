// Package tokencmd is a token credential whose source is another
// program: icbfs's credential-helper interface. The mount runs the
// command, asks it for a token on its stdin, reads the answer on its
// stdout, and keeps the token until shortly before it expires. How the
// command gets the token (a sign-in, a broker, a managed identity) is
// its own business, and its configuration is its own: it inherits the
// mount's environment.
//
// The protocol is one JSON line each way, Version in both:
//
//	> {"version": 1, "scopes": ["https://storage.azure.com/.default"], "interactive": true}
//	< {"version": 1, "token": "...", "expiresOn": "2026-10-27T12:34:56Z", "account": "who@tenant"}
//	< {"version": 1, "error": "message", "code": "interaction_required|unsupported_version|failed"}
//
// The command answers each request in turn and exits when its stdin
// closes. Its stderr is the mount's. "interactive" false means answer
// silently, or reply interaction_required; true, the first ask, lets it
// show a sign-in. "account" is only shown: whose identity the mount acts
// as. A reply with another version is an error that names both.
package tokencmd

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
)

// Version is the protocol's, sent in every request and expected in
// every reply.
const Version = 1

// StorageScope is the scope asked for when the caller names none:
// Azure Storage's.
const StorageScope = "https://storage.azure.com/.default"

// Request is what the command reads, one per line.
type Request struct {
	Version     int      `json:"version"`
	Scopes      []string `json:"scopes"`
	Interactive bool     `json:"interactive"`
}

// Reply is what the command writes, one per request: a token, or an
// error with a code.
type Reply struct {
	Version   int    `json:"version"`
	Token     string `json:"token,omitempty"`
	ExpiresOn string `json:"expiresOn,omitempty"`
	Account   string `json:"account,omitempty"`
	Error     string `json:"error,omitempty"`
	Code      string `json:"code,omitempty"`
}

// The reply codes.
const (
	CodeInteractionRequired = "interaction_required" // a silent ask needs a sign-in
	CodeUnsupportedVersion  = "unsupported_version"  // the command doesn't speak the request's version
	CodeFailed              = "failed"               // anything else
)

// ErrInteractionRequired is wrapped in GetToken's error when the
// command's silent ask needed a sign-in.
var ErrInteractionRequired = errors.New("sign-in needed")

// Credential is an azcore.TokenCredential backed by the command: started
// on first use, asked for a token, the answer kept until five minutes
// before it expires, started again if it dies.
type Credential struct {
	path string

	mu      sync.Mutex
	proc    *exec.Cmd
	in      io.WriteCloser
	out     *bufio.Scanner
	token   azcore.AccessToken
	account string
}

var _ azcore.TokenCredential = (*Credential)(nil)

// New starts the command at path and asks it for a first token,
// interactively: the one ask that may show a sign-in. ctx bounds the
// wait. No path, a command that doesn't start, or one that answers with
// an error is an error.
func New(ctx context.Context, path string) (*Credential, error) {
	if path == "" {
		return nil, errors.New("no token command")
	}
	c := &Credential{path: path}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := c.ask(ctx, []string{StorageScope}, true); err != nil {
		c.stop()
		return nil, err
	}
	return c, nil
}

// GetToken is azcore.TokenCredential's: the kept token while it has five
// minutes left, else a fresh one from the command, silently.
func (c *Credential) GetToken(ctx context.Context, opts policy.TokenRequestOptions) (azcore.AccessToken, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token.Token != "" && time.Until(c.token.ExpiresOn) > 5*time.Minute {
		return c.token, nil
	}
	scopes := opts.Scopes
	if len(scopes) == 0 {
		scopes = []string{StorageScope}
	}
	return c.ask(ctx, scopes, false)
}

// Account is whose identity the token is, as the command's last answer
// said; empty if it didn't say.
func (c *Credential) Account() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.account
}

// Close stops the command. The credential is unusable after.
func (c *Credential) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stop()
	return nil
}

// ask sends one request and reads one reply, starting (or restarting)
// the command as needed. Called with mu held.
func (c *Credential) ask(ctx context.Context, scopes []string, interactive bool) (azcore.AccessToken, error) {
	req := Request{Version: Version, Scopes: scopes, Interactive: interactive}
	reply, err := c.exchange(ctx, req)
	if err != nil {
		// A dead command gets one restart.
		c.stop()
		reply, err = c.exchange(ctx, req)
		if err != nil {
			return azcore.AccessToken{}, fmt.Errorf("token command: %w", err)
		}
	}
	if reply.Version != Version {
		return azcore.AccessToken{}, fmt.Errorf("token command: its reply is protocol version %d, this is version %d", reply.Version, Version)
	}
	if reply.Error != "" {
		if reply.Code == CodeInteractionRequired {
			return azcore.AccessToken{}, fmt.Errorf("token command: %w: %s", ErrInteractionRequired, reply.Error)
		}
		return azcore.AccessToken{}, fmt.Errorf("token command: %s", reply.Error)
	}
	exp, err := time.Parse(time.RFC3339, reply.ExpiresOn)
	if err != nil {
		return azcore.AccessToken{}, fmt.Errorf("token command: expiry %q: %w", reply.ExpiresOn, err)
	}
	c.token = azcore.AccessToken{Token: reply.Token, ExpiresOn: exp}
	c.account = reply.Account
	return c.token, nil
}

// exchange writes a request and reads the reply line, with ctx bounding
// the wait. An error means the command is gone or not answering.
func (c *Credential) exchange(ctx context.Context, req Request) (Reply, error) {
	if err := c.start(); err != nil {
		return Reply{}, err
	}
	data, err := json.Marshal(req)
	if err != nil {
		return Reply{}, err
	}
	if _, err := c.in.Write(append(data, '\n')); err != nil {
		return Reply{}, fmt.Errorf("writing to it: %w", err)
	}
	type lineErr struct {
		line string
		err  error
	}
	// The reader takes the scanner by value: on a cancelled ctx, stop()
	// below drops the fields while the reader may still be in Scan; it
	// then returns on its own when the killed command's pipe closes.
	ch := make(chan lineErr, 1)
	out := c.out
	go func() {
		if out.Scan() {
			ch <- lineErr{line: out.Text()}
			return
		}
		err := out.Err()
		if err == nil {
			err = errors.New("it closed its output")
		}
		ch <- lineErr{err: err}
	}()
	select {
	case <-ctx.Done():
		c.stop()
		return Reply{}, ctx.Err()
	case r := <-ch:
		if r.err != nil {
			return Reply{}, r.err
		}
		var reply Reply
		if err := json.Unmarshal([]byte(r.line), &reply); err != nil {
			return Reply{}, fmt.Errorf("its reply %q: %w", r.line, err)
		}
		return reply, nil
	}
}

func (c *Credential) start() error {
	if c.proc != nil {
		return nil
	}
	cmd := exec.Command(c.path)
	cmd.Stderr = os.Stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting %s: %w", c.path, err)
	}
	c.proc, c.in = cmd, in
	c.out = bufio.NewScanner(out)
	c.out.Buffer(make([]byte, 0, 64<<10), 1<<20)
	return nil
}

// stop closes the command's stdin, which tells it to exit, waits two
// seconds, then kills it.
func (c *Credential) stop() {
	if c.proc == nil {
		return
	}
	c.in.Close()
	done := make(chan struct{})
	go func() { c.proc.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		c.proc.Process.Kill()
		<-done
	}
	c.proc, c.in, c.out = nil, nil, nil
}
