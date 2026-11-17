package extract

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// HelperArg is the first argument that turns the worker binary (or a test
// binary) into the extraction helper. Callers check it at the top of main
// or TestMain and call HelperMain.
const HelperArg = "__extract-helper"

// Exit codes of the helper process.
const (
	exitOK          = 0
	exitLimit       = 3
	exitMalformed   = 4
	exitUnsupported = 5
)

const (
	envMemory = "EXTRACT_MEMORY_BYTES"
	envCPU    = "EXTRACT_CPU_SECONDS"
)

// Isolated runs each extraction in a child process (ING-REQ-002) with:
// a minimal environment (no database or cloud credentials), Linux rlimits
// on address space, CPU time, open files and file writes (set by the child
// itself before parsing), a wall-clock timeout, and caps on input, output
// and stderr. A parser crash, hang or memory blow-up kills only the child.
type Isolated struct {
	// Command starts the helper, e.g. {os.Executable(), HelperArg}.
	Command []string
	// ExtraEnv is appended to the child's minimal environment.
	ExtraEnv []string

	Timeout     time.Duration // default 2 min
	MemoryBytes uint64        // default 1 GiB
	CPUSeconds  uint64        // default 60
	Limits      Limits        // zero value = DefaultLimits
}

// SelfCommand returns the command that re-executes the current binary as
// the helper.
func SelfCommand() ([]string, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return []string{exe, HelperArg}, nil
}

func (x *Isolated) defaults() {
	if x.Timeout <= 0 {
		x.Timeout = 2 * time.Minute
	}
	if x.MemoryBytes == 0 {
		x.MemoryBytes = 1 << 30
	}
	if x.CPUSeconds == 0 {
		x.CPUSeconds = 60
	}
	if x.Limits == (Limits{}) {
		x.Limits = DefaultLimits
	}
}

// Extract streams r into the helper and returns its sections.
func (x *Isolated) Extract(ctx context.Context, contentType string, r io.Reader) ([]Section, error) {
	x.defaults()
	if len(x.Command) == 0 {
		return nil, errors.New("extract: no helper command configured")
	}
	ctx, cancel := context.WithTimeout(ctx, x.Timeout)
	defer cancel()

	args := append(append([]string{}, x.Command[1:]...), contentType)
	cmd := exec.CommandContext(ctx, x.Command[0], args...) //nolint:gosec // fixed helper binary
	cmd.Env = append([]string{
		envMemory + "=" + strconv.FormatUint(x.MemoryBytes, 10),
		envCPU + "=" + strconv.FormatUint(x.CPUSeconds, 10),
	}, x.ExtraEnv...)
	if v := os.Getenv("SYSTEMROOT"); v != "" { // Windows development only
		cmd.Env = append(cmd.Env, "SYSTEMROOT="+v)
	}
	cmd.WaitDelay = 5 * time.Second

	in := &capReader{r: r, remaining: x.Limits.MaxInputBytes + 1}
	cmd.Stdin = in
	// JSON escaping can roughly double text; allow headroom over the text cap.
	stdout := &capBuffer{max: 2*x.Limits.MaxOutputBytes + 64<<10, onFull: cancel}
	stderr := &capBuffer{max: 4 << 10}
	cmd.Stdout, cmd.Stderr = stdout, stderr

	err := cmd.Run()
	switch {
	case in.exceeded || in.remaining <= 0:
		return nil, fmt.Errorf("%w: input larger than %d bytes", ErrLimit, x.Limits.MaxInputBytes)
	case stdout.full:
		return nil, fmt.Errorf("%w: helper output over cap", ErrLimit)
	case ctx.Err() != nil && errors.Is(ctx.Err(), context.DeadlineExceeded):
		return nil, fmt.Errorf("%w: parser timed out after %s", ErrLimit, x.Timeout)
	case ctx.Err() != nil:
		return nil, ctx.Err()
	}
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			return nil, fmt.Errorf("extract: start helper: %w", err)
		}
		msg := strings.TrimSpace(stderr.String())
		switch ee.ExitCode() {
		case exitMalformed:
			return nil, fmt.Errorf("%w: %s", ErrMalformed, msg)
		case exitUnsupported:
			return nil, fmt.Errorf("%w: %s", ErrUnsupported, msg)
		default:
			// exitLimit, a signal from an rlimit (SIGKILL/SIGXCPU), or a
			// runtime crash such as out-of-memory.
			return nil, fmt.Errorf("%w: helper exited (%v) %s", ErrLimit, ee, msg)
		}
	}
	var secs []Section
	if err := json.Unmarshal(stdout.Bytes(), &secs); err != nil {
		return nil, fmt.Errorf("extract: helper output: %w", err)
	}
	return secs, nil
}

// HelperMain is the child side: os.Args = [exe, HelperArg, contentType].
// It never returns.
func HelperMain() {
	mem, _ := strconv.ParseUint(os.Getenv(envMemory), 10, 64)
	cpu, _ := strconv.ParseUint(os.Getenv(envCPU), 10, 64)
	if err := applyLimits(mem, cpu); err != nil {
		fmt.Fprintln(os.Stderr, "apply limits:", err)
		os.Exit(exitLimit)
	}
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: <exe>", HelperArg, "<content-type>")
		os.Exit(exitUnsupported)
	}
	lim := DefaultLimits
	data, err := io.ReadAll(io.LimitReader(os.Stdin, lim.MaxInputBytes+1))
	if err != nil {
		fmt.Fprintln(os.Stderr, "read input:", err)
		os.Exit(exitMalformed)
	}
	secs, err := Extract(os.Args[2], data, lim)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		switch {
		case errors.Is(err, ErrLimit):
			os.Exit(exitLimit)
		case errors.Is(err, ErrUnsupported):
			os.Exit(exitUnsupported)
		default:
			os.Exit(exitMalformed)
		}
	}
	if secs == nil {
		secs = []Section{}
	}
	if err := json.NewEncoder(os.Stdout).Encode(secs); err != nil {
		os.Exit(exitLimit)
	}
	os.Exit(exitOK)
}

// capBuffer keeps at most max bytes and reports when the cap is hit. The
// buffer is a named field, not embedded: embedding would promote
// bytes.Buffer.ReadFrom, and io.Copy would use it and bypass Write's cap.
type capBuffer struct {
	buf    bytes.Buffer
	max    int64
	full   bool
	onFull func()
}

func (c *capBuffer) Write(p []byte) (int, error) {
	if int64(c.buf.Len()+len(p)) > c.max {
		if !c.full {
			c.full = true
			if c.onFull != nil {
				c.onFull()
			}
		}
		return 0, ErrLimit
	}
	return c.buf.Write(p)
}

func (c *capBuffer) Bytes() []byte  { return c.buf.Bytes() }
func (c *capBuffer) String() string { return c.buf.String() }
