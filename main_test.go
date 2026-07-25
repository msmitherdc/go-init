/*
Copyright 2024 go-init Contributors

SPDX-License-Identifier: MIT
*/
package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"context"
	"sync"
)

// script writes an executable shell script and returns its path. Commands are
// split on whitespace, so a temporary directory containing a space would be
// parsed as several arguments and the test would be meaningless.
func script(t *testing.T, body string) string {
	t.Helper()

	dir := t.TempDir()
	if strings.ContainsAny(dir, " \t") {
		t.Skipf("temporary directory %q contains whitespace", dir)
	}

	path := filepath.Join(dir, "script.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatalf("write script: %v", err)
	}
	return path
}

// TestRunEmptyCommand covers a command that contains no fields at all. This
// used to index into an empty slice and panic, which in a container takes PID 1
// down with it.
func TestRunEmptyCommand(t *testing.T) {
	for _, command := range []string{"", " ", "   \t  "} {
		t.Run("command="+strings.ReplaceAll(command, "\t", "\\t"), func(t *testing.T) {
			var child childTracker

			code, err := run(command, &child)
			if err == nil {
				t.Fatalf("run(%q) = %d, nil; want an error", command, code)
			}
			if code != 1 {
				t.Errorf("run(%q) code = %d; want 1", command, code)
			}
		})
	}
}

func TestRunSuccess(t *testing.T) {
	var child childTracker

	code, err := run(script(t, "exit 0\n"), &child)
	if err != nil {
		t.Fatalf("run() error = %v; want nil", err)
	}
	if code != 0 {
		t.Errorf("run() code = %d; want 0", code)
	}
}

// TestRunExitStatus checks that the command's exit code reaches the caller
// instead of being flattened to 1.
func TestRunExitStatus(t *testing.T) {
	var child childTracker

	code, err := run(script(t, "exit 3\n"), &child)
	if err == nil {
		t.Fatal("run() error = nil; want a non-nil error")
	}
	if code != 3 {
		t.Errorf("run() code = %d; want 3", code)
	}
}

// TestRunSignaled checks the POSIX shell convention for a command killed by a
// signal.
func TestRunSignaled(t *testing.T) {
	var child childTracker

	code, err := run(script(t, "kill -9 $$\n"), &child)
	if err == nil {
		t.Fatal("run() error = nil; want a non-nil error")
	}
	if want := 128 + int(syscall.SIGKILL); code != want {
		t.Errorf("run() code = %d; want %d", code, want)
	}
}

func TestRunCommandNotFound(t *testing.T) {
	var child childTracker

	code, err := run(filepath.Join(t.TempDir(), "does-not-exist"), &child)
	if err == nil {
		t.Fatal("run() error = nil; want a non-nil error")
	}
	if code != 1 {
		t.Errorf("run() code = %d; want 1", code)
	}
}

// TestRunArguments checks that arguments after the command name are passed
// through.
func TestRunArguments(t *testing.T) {
	var child childTracker

	path := script(t, `[ "$1" = "one" ] && [ "$2" = "two" ] && exit 0
exit 9
`)

	code, err := run(path+" one two", &child)
	if err != nil {
		t.Fatalf("run() error = %v; want nil", err)
	}
	if code != 0 {
		t.Errorf("run() code = %d; want 0", code)
	}
}

// TestRunForwardsSignals sends a signal to this process and checks that it
// reaches the command's process group. signal.Notify is active for the duration
// of run, so the signal cannot terminate the test binary itself.
func TestRunForwardsSignals(t *testing.T) {
	dir := t.TempDir()
	if strings.ContainsAny(dir, " \t") {
		t.Skipf("temporary directory %q contains whitespace", dir)
	}

	ready := filepath.Join(dir, "ready")
	path := filepath.Join(dir, "script.sh")
	body := "#!/bin/sh\ntrap 'exit 42' TERM\n: > " + ready + "\ni=0\nwhile [ $i -lt 400 ]; do\n  sleep 0.05\n  i=$((i+1))\ndone\nexit 7\n"
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatalf("write script: %v", err)
	}

	type result struct {
		code int
		err  error
	}
	done := make(chan result, 1)

	var child childTracker
	go func() {
		code, err := run(path, &child)
		done <- result{code, err}
	}()

	// Wait for the command to install its signal handler.
	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("command never signalled readiness")
		}
		time.Sleep(10 * time.Millisecond)
	}

	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("kill: %v", err)
	}

	select {
	case got := <-done:
		if got.code != 42 {
			t.Errorf("run() code = %d, err = %v; want 42 (command's SIGTERM handler)", got.code, got.err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("run() did not return after SIGTERM was forwarded")
	}
}

func TestStatusCode(t *testing.T) {
	tests := []struct {
		name   string
		status syscall.WaitStatus
		want   int
	}{
		// The exit status lives in bits 8-15 of the wait status.
		{"exit 0", syscall.WaitStatus(0), 0},
		{"exit 3", syscall.WaitStatus(3 << 8), 3},
		{"exit 255", syscall.WaitStatus(255 << 8), 255},
		// A process killed by a signal carries the signal number in the
		// low 7 bits.
		{"SIGKILL", syscall.WaitStatus(syscall.SIGKILL), 128 + int(syscall.SIGKILL)},
		{"SIGTERM", syscall.WaitStatus(syscall.SIGTERM), 128 + int(syscall.SIGTERM)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := statusCode(tc.status); got != tc.want {
				t.Errorf("statusCode(%#x) = %d; want %d", uint32(tc.status), got, tc.want)
			}
		})
	}
}

// TestExitCodeReaperWonTheRace covers the case where removeZombies collects the
// foreground command before Wait does. Wait then fails with ECHILD and the
// status recorded by the reaper is the only source of the real exit code.
func TestExitCodeReaperWonTheRace(t *testing.T) {
	t.Run("non-zero exit", func(t *testing.T) {
		var child childTracker
		child.watch(4242)
		child.record(4242, syscall.WaitStatus(3<<8))

		code, err := exitCode(&child, syscall.ECHILD)
		if code != 3 {
			t.Errorf("exitCode() code = %d; want 3", code)
		}
		if err == nil {
			t.Error("exitCode() error = nil; want a non-nil error")
		}
	})

	t.Run("clean exit", func(t *testing.T) {
		var child childTracker
		child.watch(4242)
		child.record(4242, syscall.WaitStatus(0))

		code, err := exitCode(&child, syscall.ECHILD)
		if code != 0 || err != nil {
			t.Errorf("exitCode() = %d, %v; want 0, nil", code, err)
		}
	})

	t.Run("nothing recorded", func(t *testing.T) {
		var child childTracker
		child.watch(4242)

		code, err := exitCode(&child, syscall.ECHILD)
		if code != 1 || err == nil {
			t.Errorf("exitCode() = %d, %v; want 1 and an error", code, err)
		}
	})
}

// TestChildTrackerIgnoresOtherPids makes sure an unrelated orphan being reaped
// is not mistaken for the foreground command's exit status.
func TestChildTrackerIgnoresOtherPids(t *testing.T) {
	var child childTracker

	// Nothing is being tracked yet.
	child.record(111, syscall.WaitStatus(5<<8))
	if _, ok := child.stolen(); ok {
		t.Error("stolen() reported a status while no command was tracked")
	}

	child.watch(222)
	child.record(111, syscall.WaitStatus(5<<8))
	if _, ok := child.stolen(); ok {
		t.Error("stolen() reported the status of an unrelated PID")
	}

	child.record(222, syscall.WaitStatus(6<<8))
	status, ok := child.stolen()
	if !ok {
		t.Fatal("stolen() reported no status for the tracked PID")
	}
	if got := statusCode(status); got != 6 {
		t.Errorf("statusCode() = %d; want 6", got)
	}

	child.forget()
	if _, ok := child.stolen(); ok {
		t.Error("stolen() still reported a status after forget()")
	}
}

// TestRemoveZombiesStopsPromptly guards the shutdown path: cleanQuit blocks on
// this goroutine, so it has to notice a cancelled context without waiting out
// its polling interval.
func TestRemoveZombiesStopsPromptly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	var child childTracker

	wg.Add(1)
	go removeZombies(ctx, &wg, &child)

	// Let it settle into the polling sleep before cancelling.
	time.Sleep(100 * time.Millisecond)
	cancel()

	stopped := make(chan struct{})
	go func() {
		wg.Wait()
		close(stopped)
	}()

	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("removeZombies did not return after the context was cancelled")
	}
}
