package tokencmd

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
)

// The fake command is this test binary, re-run with ICBFS_FAKE_TOKEN_COMMAND
// set to a script of replies: one word per request, in order, "ok" (a
// token good for an hour), "short" (a token with a minute left),
// "interaction_required", "failed", "old" (a version 0 reply), "die" (exit
// without answering), "garbage" (a line that isn't JSON). After the
// script runs out it answers ok.
func TestMain(m *testing.M) {
	if script := os.Getenv("ICBFS_FAKE_TOKEN_COMMAND"); script != "" {
		fakeCommand(strings.Fields(script))
		return
	}
	os.Exit(m.Run())
}

func fakeCommand(script []string) {
	in := bufio.NewScanner(os.Stdin)
	n := 0
	for in.Scan() {
		var req Request
		if err := json.Unmarshal(in.Bytes(), &req); err != nil {
			fmt.Fprintln(os.Stderr, "fake token command: bad request:", err)
			os.Exit(3)
		}
		if req.Version != Version {
			reply(Reply{Version: Version, Error: fmt.Sprintf("version %d", req.Version), Code: CodeUnsupportedVersion})
			continue
		}
		step := "ok"
		if n < len(script) {
			step = script[n]
		}
		n++
		switch step {
		case "ok":
			reply(Reply{Version: Version, Token: fmt.Sprintf("tok-%d-%v", n, req.Interactive), ExpiresOn: time.Now().Add(time.Hour).UTC().Format(time.RFC3339), Account: "who@example.com"})
		case "short":
			reply(Reply{Version: Version, Token: fmt.Sprintf("short-%d", n), ExpiresOn: time.Now().Add(time.Minute).UTC().Format(time.RFC3339), Account: "who@example.com"})
		case "interaction_required":
			reply(Reply{Version: Version, Error: "sign in", Code: CodeInteractionRequired})
		case "failed":
			reply(Reply{Version: Version, Error: "no can do", Code: CodeFailed})
		case "old":
			reply(Reply{Version: 0, Token: "old", ExpiresOn: time.Now().Add(time.Hour).UTC().Format(time.RFC3339)})
		case "die":
			os.Exit(0)
		case "garbage":
			fmt.Println("not json")
		}
	}
}

func reply(r Reply) {
	data, _ := json.Marshal(r)
	fmt.Println(string(data))
}

// fake is the fake command's path, with script as its replies.
func fake(t *testing.T, script string) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICBFS_FAKE_TOKEN_COMMAND", script)
	return exe
}

func TestCredential(t *testing.T) {
	ctx := context.Background()

	t.Run("first token is interactive, then kept", func(t *testing.T) {
		c, err := New(ctx, fake(t, "ok ok"))
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		if c.Account() != "who@example.com" {
			t.Errorf("account %q", c.Account())
		}
		tok, err := c.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{StorageScope}})
		if err != nil {
			t.Fatal(err)
		}
		if tok.Token != "tok-1-true" {
			t.Errorf("token %q: want the first, interactive answer, kept", tok.Token)
		}
	})

	t.Run("a token near expiry is refreshed silently", func(t *testing.T) {
		c, err := New(ctx, fake(t, "short ok"))
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		tok, err := c.GetToken(ctx, policy.TokenRequestOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if tok.Token != "tok-2-false" {
			t.Errorf("token %q: want a second, silent answer", tok.Token)
		}
	})

	t.Run("a dead command is started once more", func(t *testing.T) {
		c, err := New(ctx, fake(t, "short die ok"))
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		tok, err := c.GetToken(ctx, policy.TokenRequestOptions{})
		if err != nil {
			t.Fatal(err)
		}
		// The restarted command runs the script from the top: "short".
		if !strings.HasPrefix(tok.Token, "short-1") {
			t.Errorf("token %q: want the restarted command's first answer", tok.Token)
		}
	})

	t.Run("sign-in needed is an error, not a prompt", func(t *testing.T) {
		c, err := New(ctx, fake(t, "ok interaction_required"))
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		c.token.ExpiresOn = time.Now() // force a refresh
		if _, err := c.GetToken(ctx, policy.TokenRequestOptions{}); !errors.Is(err, ErrInteractionRequired) {
			t.Fatalf("%v, want ErrInteractionRequired", err)
		}
	})

	t.Run("a failure is its message", func(t *testing.T) {
		_, err := New(ctx, fake(t, "failed"))
		if err == nil || !strings.Contains(err.Error(), "no can do") {
			t.Fatalf("%v, want the command's message", err)
		}
	})

	t.Run("another version is an error naming both", func(t *testing.T) {
		_, err := New(ctx, fake(t, "old"))
		if err == nil || !strings.Contains(err.Error(), "version 0") || !strings.Contains(err.Error(), "version 1") {
			t.Fatalf("%v, want both versions named", err)
		}
	})

	t.Run("garbage is an error", func(t *testing.T) {
		_, err := New(ctx, fake(t, "garbage"))
		if err == nil || !strings.Contains(err.Error(), "reply") {
			t.Fatalf("%v, want a reply error", err)
		}
	})

	t.Run("a hanging command is bounded by ctx", func(t *testing.T) {
		c, err := New(ctx, fake(t, "ok"))
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		c.token.ExpiresOn = time.Now()
		short, cancel := context.WithTimeout(ctx, 0)
		defer cancel()
		if _, err := c.GetToken(short, policy.TokenRequestOptions{}); err == nil {
			t.Fatal("expired ctx: got a token, want an error")
		}
	})

	t.Run("no command, or one that doesn't start, is an error", func(t *testing.T) {
		if _, err := New(ctx, ""); err == nil {
			t.Fatal("no path: want an error")
		}
		_, err := New(ctx, t.TempDir()+"/no-such-command")
		if err == nil || !strings.Contains(err.Error(), "starting") {
			t.Fatalf("%v, want a starting error", err)
		}
	})
}
