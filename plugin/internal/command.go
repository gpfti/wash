// Package internal contains utility classes and helpers that are used
// by the plugin package. Its purpose is to modularize the plugin package's
// code without exporting its implementation.
package internal

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/kballard/go-shellquote"
	"github.com/puppetlabs/wash/activity"
)

// Command is a wrapper to exec.Cmd. It handles context-cancellation cleanup
// and defines a String() method to make logging easier.
type Command interface {
	Start() error
	Run() error
	Terminate()
	Wait() error
	SetStdout(stdout io.Writer)
	SetStderr(stderr io.Writer)
	SetStdin(stdin io.Reader)
	StdoutPipe() (io.ReadCloser, error)
	StderrPipe() (io.ReadCloser, error)
	ExitCode() int
}

type command struct {
	*exec.Cmd
	ctx         context.Context
	pgid        int
	terminateCh chan struct{}
	waitResult  error
	waitDoneCh  chan struct{}
	waitOnce    sync.Once
}

// NewCommand creates a new command object that's tied to the passed-in
// context. When cmd.Start() is invoked, the command will run in its
// own process group. When the context is cancelled, a SIGTERM signal will
// be sent to the command's process group. If after five seconds the command's
// process has not been terminated, then a SIGKILL signal is sent to the
// command's process group.
func NewCommand(ctx context.Context, cmd string, args ...string) Command {
	if ctx == nil {
		panic("plugin.newCommand called with a nil context")
	}
	cmdObj := &command{
		Cmd:         exec.Command(cmd, args...),
		ctx:         ctx,
		pgid:        -1,
		terminateCh: make(chan struct{}),
		waitDoneCh:  make(chan struct{}),
	}
	cmdObj.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}
	return cmdObj
}

// Start is a wrapper to exec.Cmd#Start
func (cmd *command) Start() error {
	err := cmd.Cmd.Start()
	if err != nil {
		return err
	}
	// Get the command's PGID for logging. If this fails, we'll try
	// again in cmd.signal() when it is needed.
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		activity.Record(cmd.ctx, "%v: could not get pgid: %v", cmd, err)
	} else {
		cmd.pgid = pgid
	}
	// Setup the context-cancellation cleanup
	go func() {
		var desc string
		select {
		case <-cmd.waitDoneCh:
			return
		case <-cmd.terminateCh:
			desc = "Command terminated"
		case <-cmd.ctx.Done():
			desc = "Context cancelled"
		}
		activity.Record(cmd.ctx, "%v: %s. Sending SIGTERM signal", cmd, desc)
		if err := cmd.signal(syscall.SIGTERM); err != nil {
			activity.Record(cmd.ctx, "%v: Failed to send SIGTERM signal: %v", cmd, err)
		} else {
			// SIGTERM was sent. Send SIGKILL after five seconds if the command failed
			// to terminate.
			time.AfterFunc(5*time.Second, func() {
				select {
				case <-cmd.waitDoneCh:
					return
				default:
					// Pass-thru
				}
				activity.Record(cmd.ctx, "%v: Did not terminate after five seconds. Sending SIGKILL signal", cmd)
				if err := cmd.signal(syscall.SIGKILL); err != nil {
					activity.Record(cmd.ctx, "%v: Failed to send SIGKILL signal: %v", cmd, err)
				}
			})
		}
		// Call Wait() to release cmd's resources. Leave error-logging up to the
		// callers
		_ = cmd.Wait()
	}()
	return nil
}

// String returns a stringified version of the command
// that's useful for logging
func (cmd *command) String() string {
	str := ""
	if cmd.Process != nil {
		str += fmt.Sprintf("(PID %v) ", cmd.Process.Pid)
	}
	if cmd.pgid >= 0 {
		str += fmt.Sprintf("(PGID %v) ", cmd.pgid)
	}
	str += shellquote.Join(cmd.Args...)
	return str
}

// Run is a wrapper to exec.Cmd#Run
func (cmd *command) Run() error {
	// Copied from exec.Cmd#Run
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Wait()
}

// Terminate signals the command to stop. It will initiate SIGTERM,
// followed by SIGKILL if it doesn't shutdown within 5 seconds.
// Call Wait after to wait for the command to exit.
func (cmd *command) Terminate() {
	select {
	case <-cmd.terminateCh:
		return
	default:
		close(cmd.terminateCh)
	}
}

// Wait is a thread-safe wrapper to exec.Cmd#Wait
func (cmd *command) Wait() error {
	// According to https://github.com/golang/go/issues/28461,
	// exec.Cmd#Wait is not thread-safe, so we need to implement
	// our own version.
	cmd.waitOnce.Do(func() {
		cmd.waitResult = cmd.Cmd.Wait()
		close(cmd.waitDoneCh)
	})
	return cmd.waitResult
}

func (cmd *command) signal(sig syscall.Signal) error {
	if cmd.Process == nil {
		panic("cmd.signal called with cmd.Process == nil")
	}
	if cmd.pgid < 0 {
		// We failed to get the pgid in cmd.Start(), so try again
		pgid, err := syscall.Getpgid(cmd.Process.Pid)
		if err != nil {
			return fmt.Errorf("could not get pgid: %v", err)
		}
		cmd.pgid = pgid
	}
	err := syscall.Kill(-cmd.pgid, sig)
	if err != nil {
		return err
	}
	return nil
}

func (cmd *command) SetStdout(stdout io.Writer) {
	cmd.Stdout = stdout
}

func (cmd *command) SetStderr(stderr io.Writer) {
	cmd.Stderr = stderr
}

func (cmd *command) SetStdin(stdin io.Reader) {
	cmd.Stdin = stdin
}

func (cmd *command) ExitCode() int {
	return cmd.Cmd.ProcessState.ExitCode()
}
