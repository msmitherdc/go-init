/*
Copyright 2017 Pablo RUTH
Copyright 2024 go-init Contributors

SPDX-License-Identifier: MIT
*/
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

var (
	versionString = "undefined"
)

func main() {
	var preStartCmd string
	var mainCmd string
	var postStopCmd string
	var version bool

	flag.StringVar(&preStartCmd, "pre", "", "Pre-start command")
	flag.StringVar(&mainCmd, "main", "", "Main command")
	flag.StringVar(&postStopCmd, "post", "", "Post-stop command")
	flag.BoolVar(&version, "version", false, "Display go-init version")
	flag.Parse()

	if version {
		fmt.Println(versionString)
		os.Exit(0)
	}

	if mainCmd == "" {
		log.Fatal("[go-init] No main command defined, exiting")
	}

	// Routine to reap zombies (it's the job of init)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	var child childTracker
	wg.Add(1)
	go removeZombies(ctx, &wg, &child)

	// Launch pre-start command
	if preStartCmd == "" {
		log.Println("[go-init] No pre-start command defined, skip")
	} else {
		log.Printf("[go-init] Pre-start command launched : %s\n", preStartCmd)
		code, err := run(preStartCmd, &child)
		if err != nil {
			log.Println("[go-init] Pre-start command failed")
			log.Printf("[go-init] %s\n", err)
			cleanQuit(cancel, &wg, code)
		} else {
			log.Printf("[go-init] Pre-start command exited")
		}
	}

	// Launch main command
	log.Printf("[go-init] Main command launched : %s\n", mainCmd)
	mainRC, err := run(mainCmd, &child)
	if err != nil {
		log.Println("[go-init] Main command failed")
		log.Printf("[go-init] %s\n", err)
	} else {
		log.Printf("[go-init] Main command exited")
	}

	// Launch post-stop command
	if postStopCmd == "" {
		log.Println("[go-init] No post-stop command defined, skip")
	} else {
		log.Printf("[go-init] Post-stop command launched : %s\n", postStopCmd)
		code, err := run(postStopCmd, &child)
		if err != nil {
			log.Println("[go-init] Post-stop command failed")
			log.Printf("[go-init] %s\n", err)
			cleanQuit(cancel, &wg, code)
		} else {
			log.Printf("[go-init] Post-stop command exited")
		}
	}

	// Wait removeZombies goroutine
	cleanQuit(cancel, &wg, mainRC)
}

// childTracker records the PID of the command currently in the foreground so
// that the zombie reaping goroutine can hand back its wait status. Both
// goroutines call syscall wait functions on the same process, and whichever
// one gets there first is the only one that can observe the exit status.
type childTracker struct {
	mu     sync.Mutex
	pid    int
	status syscall.WaitStatus
	reaped bool
}

// watch starts tracking pid as the foreground command.
func (c *childTracker) watch(pid int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pid = pid
	c.reaped = false
}

// forget stops tracking the foreground command.
func (c *childTracker) forget() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pid = 0
	c.reaped = false
}

// record notes the wait status of a reaped process, keeping it only if it
// belongs to the foreground command.
func (c *childTracker) record(pid int, status syscall.WaitStatus) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pid == 0 || pid != c.pid {
		return
	}
	c.status = status
	c.reaped = true
}

// stolen reports the wait status of the foreground command when the reaping
// goroutine collected it before (*exec.Cmd).Wait could.
func (c *childTracker) stolen() (syscall.WaitStatus, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status, c.reaped
}

func removeZombies(ctx context.Context, wg *sync.WaitGroup, child *childTracker) {
	defer wg.Done()

	for {
		// Stop as soon as the context is done, even while children are
		// still being reaped back to back.
		select {
		case <-ctx.Done():
			return
		default:
		}

		var status syscall.WaitStatus

		// Wait for orphaned zombie process
		pid, err := syscall.Wait4(-1, &status, syscall.WNOHANG, nil)

		if err == syscall.EINTR {
			continue
		}

		if pid > 0 {
			// PID is > 0 if a child was reaped. Keep its status in case it
			// was the foreground command, so run can still report the real
			// exit code, then immediately check if another one is waiting.
			child.record(pid, status)
			continue
		}

		// PID is 0 or -1 if no child waiting, so we wait
		// for 1 second before the next check, unless we
		// are asked to stop first.
		select {
		case <-ctx.Done():
			return
		case <-time.After(1 * time.Second):
		}
	}
}

// run executes command, forwarding every signal it receives to the command's
// process group, and returns the command's exit code.
func run(command string, child *childTracker) (int, error) {

	var commandStr string
	var argsSlice []string

	// Split cmd and args
	commandSlice := strings.Fields(command)
	if len(commandSlice) == 0 {
		return 1, errors.New("no command to run")
	}
	commandStr = commandSlice[0]
	// if there is args
	if len(commandSlice) > 1 {
		argsSlice = commandSlice[1:]
	}

	// Register chan to receive system signals. This is done before the
	// command starts so that signals arriving during startup are buffered
	// and forwarded rather than lost.
	sigs := make(chan os.Signal, 64)
	signal.Notify(sigs)
	defer signal.Stop(sigs)

	// Define command and rebind
	// stdout and stdin
	cmd := exec.Command(commandStr, argsSlice...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	// Create a dedicated pidgroup
	// used to forward signals to
	// main process and all children
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// Start defined command
	err := cmd.Start()
	if err != nil {
		return 1, err
	}

	// Read the PID once, here, rather than from the forwarding goroutine:
	// cmd.Process is written by Start and reading it concurrently is a
	// data race.
	pid := cmd.Process.Pid
	child.watch(pid)
	defer child.forget()

	// Goroutine for signals forwarding, stopped once the command exits so
	// it cannot outlive the process group it signals.
	done := make(chan struct{})
	var forwarding sync.WaitGroup
	forwarding.Add(1)
	go func() {
		defer forwarding.Done()
		for {
			select {
			case sig := <-sigs:
				// Ignore SIGCHLD signals since they are only
				// useful for go-init, and SIGURG which the Go
				// runtime raises constantly for goroutine
				// preemption inside this process.
				if sig == syscall.SIGCHLD || sig == syscall.SIGURG {
					continue
				}
				// Forward signal to main process and all children
				syscall.Kill(-pid, sig.(syscall.Signal))
			case <-done:
				return
			}
		}
	}()

	// Wait for command to exit
	waitErr := cmd.Wait()
	close(done)
	forwarding.Wait()

	return exitCode(child, waitErr)
}

// exitCode derives the exit code of a finished command. (*exec.Cmd).Wait fails
// with ECHILD when the zombie reaper collected the process first, in which case
// the status the reaper recorded is the authoritative one.
func exitCode(child *childTracker, waitErr error) (int, error) {
	if waitErr == nil {
		return 0, nil
	}

	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok {
			return statusCode(status), waitErr
		}
		return exitErr.ExitCode(), waitErr
	}

	if errors.Is(waitErr, syscall.ECHILD) {
		if status, ok := child.stolen(); ok {
			code := statusCode(status)
			if code == 0 {
				return 0, nil
			}
			return code, fmt.Errorf("exit status %d", code)
		}
	}

	return 1, waitErr
}

// statusCode converts a wait status into a shell style exit code, reporting
// death by signal as 128+signum the way POSIX shells do.
func statusCode(status syscall.WaitStatus) int {
	if status.Signaled() {
		return 128 + int(status.Signal())
	}
	return status.ExitStatus()
}

func cleanQuit(cancel context.CancelFunc, wg *sync.WaitGroup, code int) {
	// Signal zombie goroutine to stop
	// and wait for it to release waitgroup
	cancel()
	wg.Wait()

	os.Exit(code)
}
