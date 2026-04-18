package exec

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func resetWhitelist() {
	mu.Lock()
	defer mu.Unlock()
	whitelist = map[string]Entry{}
}

func TestRunPanicsOnUnknownBin(t *testing.T) {
	resetWhitelist()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for unknown bin")
		}
	}()
	_, _ = Run(context.Background(), "rm", []string{"-rf", "/"})
}

func TestRunPanicsOnBadArgs(t *testing.T) {
	resetWhitelist()
	noMeta := func(s string) error {
		if strings.ContainsAny(s, ";|&`$<>") {
			return errors.New("shell metachars")
		}
		return nil
	}
	Register(Entry{
		Bin: "/bin/echo",
		Patterns: [][]ArgSpec{
			{{Flag: "-n"}, {Value: noMeta}},
		},
	})
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for shell metacharacters")
		}
	}()
	_, _ = Run(context.Background(), "/bin/echo", []string{"-n", "hi; rm -rf /"})
}

func TestRunSuccess(t *testing.T) {
	resetWhitelist()
	Register(Entry{
		Bin: "/bin/echo",
		Patterns: [][]ArgSpec{
			{{Flag: "-n"}, {Value: func(string) error { return nil }}},
		},
	})
	res, err := Run(context.Background(), "/bin/echo", []string{"-n", "hello"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if string(res.Stdout) != "hello" {
		t.Fatalf("stdout=%q", res.Stdout)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d", res.ExitCode)
	}
}

func TestDuplicateRegistrationPanics(t *testing.T) {
	resetWhitelist()
	Register(Entry{Bin: "x"})
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on duplicate Register")
		}
	}()
	Register(Entry{Bin: "x"})
}
