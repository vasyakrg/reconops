// Package exec is the single chokepoint through which collectors invoke
// external binaries. It enforces layer 3 of the read-only guarantee
// (PROJECT.md §3.4): every external command must be on a whitelist with an
// explicit shape for its arguments, and there is no shell interpolation.
//
// The whitelist is intentionally empty in week 1 — collectors only read
// /proc and /etc directly. Week 2 populates it with journalctl, ss, ip, etc.
package exec

import (
	"context"
	"errors"
	"fmt"
	exec_ "os/exec"
	"sync"
	"time"
)

var ErrNotAllowed = errors.New("exec: binary not in readonly whitelist")

// ArgSpec describes the allowed shape of a single positional or flag argument.
// A whitelist entry matches if every positional/flag pair satisfies its spec.
type ArgSpec struct {
	// Flag, if non-empty, requires the argument to be exactly this string
	// (e.g. "-u", "--since", "-o").
	Flag string
	// Value, if non-nil, validates the value following Flag (or a positional
	// argument when Flag is empty). It must reject any shell metacharacters or
	// path traversal — collectors never pass user-controlled strings unescaped.
	Value func(string) error
}

// Entry is a single allowed binary with the exact patterns of arguments it may
// receive. An empty Patterns slice means the binary may only be invoked with
// zero arguments.
type Entry struct {
	Bin      string
	Patterns [][]ArgSpec
	// Timeout caps how long the binary may run regardless of caller request.
	Timeout time.Duration
}

var (
	mu        sync.RWMutex
	whitelist = map[string]Entry{}
)

// Register adds an entry to the whitelist. Must be called from init() of
// collector packages or the agent main; calling Register at runtime from a
// collector body would itself violate the guarantee and is detected by the
// linter (forbidden import of os/exec inside collectors/).
func Register(e Entry) {
	mu.Lock()
	defer mu.Unlock()
	if _, dup := whitelist[e.Bin]; dup {
		panic(fmt.Sprintf("exec: duplicate whitelist entry for %q", e.Bin))
	}
	whitelist[e.Bin] = e
}

// Result captures stdout, stderr, and exit code of a permitted invocation.
type Result struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
	Duration time.Duration
}

// Run executes bin with args if and only if the (bin, args) pair matches a
// registered entry. Any other invocation panics — this is by design: a
// disallowed exec attempt is a programming error, not a runtime input.
func Run(ctx context.Context, bin string, args []string) (Result, error) {
	mu.RLock()
	entry, ok := whitelist[bin]
	mu.RUnlock()
	if !ok {
		panic(fmt.Sprintf("exec: %q is not in the readonly whitelist (PROJECT.md §3.4)", bin))
	}

	if err := validateArgs(entry, args); err != nil {
		panic(fmt.Sprintf("exec: argument shape rejected for %q: %v", bin, err))
	}

	if entry.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, entry.Timeout)
		defer cancel()
	}

	start := time.Now()
	cmd := exec_.CommandContext(ctx, bin, args...)
	stdout, err := cmd.Output()
	res := Result{
		Stdout:   stdout,
		ExitCode: cmd.ProcessState.ExitCode(),
		Duration: time.Since(start),
	}
	var ee *exec_.ExitError
	if errors.As(err, &ee) {
		res.Stderr = ee.Stderr
		// Non-zero exit code is data for the caller, not an error here.
		return res, nil
	}
	return res, err
}

func validateArgs(e Entry, args []string) error {
	if len(e.Patterns) == 0 {
		if len(args) != 0 {
			return fmt.Errorf("entry %q allows no arguments, got %d", e.Bin, len(args))
		}
		return nil
	}
	var lastErr error
	for _, pattern := range e.Patterns {
		if err := matchPattern(pattern, args); err == nil {
			return nil
		} else {
			lastErr = err
		}
	}
	return fmt.Errorf("no matching pattern: %w", lastErr)
}

func matchPattern(pattern []ArgSpec, args []string) error {
	i := 0
	for _, spec := range pattern {
		if spec.Flag != "" {
			if i >= len(args) || args[i] != spec.Flag {
				return fmt.Errorf("expected flag %q at position %d", spec.Flag, i)
			}
			i++
		}
		if spec.Value != nil {
			if i >= len(args) {
				return fmt.Errorf("expected value at position %d", i)
			}
			if err := spec.Value(args[i]); err != nil {
				return fmt.Errorf("value at position %d rejected: %w", i, err)
			}
			i++
		}
	}
	if i != len(args) {
		return fmt.Errorf("trailing %d unexpected args", len(args)-i)
	}
	return nil
}
