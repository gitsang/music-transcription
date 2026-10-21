package tasks

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// pipelineRequest is the single JSON object written to the CLI stdin.
type pipelineRequest struct {
	InputPath string         `json:"input_path"`
	WorkDir   string         `json:"work_dir"`
	Kind      string         `json:"kind"`
	Config    map[string]any `json:"config"`
}

// PipelineError is a sanitized pipeline failure. Code and Message are the only
// values ever exposed to the job row; local diagnostics stay in the log.
type PipelineError struct {
	Code    string
	Message string
}

func (e *PipelineError) Error() string { return e.Code + ": " + e.Message }

type progressEvent struct {
	Type          string   `json:"type"`
	Stage         string   `json:"stage"`
	StageProgress *float64 `json:"stage_progress"`
}

type terminalEvent struct {
	Type    string          `json:"type"`
	Result  json.RawMessage `json:"result"`
	Code    string          `json:"code"`
	Message string          `json:"message"`
}

// progressFunc renews the lease and validates stage reporting. An error means
// the attempt lost ownership (or was cancelled) and the subprocess must die.
type progressFunc func(stage string, fraction *float64) error

// protocolError marks a contract violation by the child process. These are
// sanitized before they reach the job row; any other error returned by the
// protocol reader (for example a lease loss raised by the progress callback) is
// propagated unchanged so the caller can react to it.
type protocolError struct{ err error }

func (e *protocolError) Error() string { return e.err.Error() }
func (e *protocolError) Unwrap() error { return e.err }

func protocolViolation(format string, args ...any) error {
	return &protocolError{err: fmt.Errorf(format, args...)}
}

// pythonRunner executes the retained Python pipeline as a shell-less child in
// its own process group so the whole tree can be terminated.
type pythonRunner struct {
	python  string
	args    []string
	env     []string
	logger  *slog.Logger
	timeout time.Duration
}

func newPythonRunner(python string, env []string, timeout time.Duration, logger *slog.Logger) *pythonRunner {
	if strings.TrimSpace(python) == "" {
		python = "python3"
	}
	if timeout <= 0 {
		timeout = 12 * time.Hour
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &pythonRunner{
		python: python, args: []string{"-m", "pipeline.cli"}, env: env,
		timeout: timeout, logger: logger,
	}
}

// run executes `python -m pipeline.cli`, streams JSON lines to progress and
// returns the validated terminal result. Cancellation kills the process group
// with TERM, then KILL after a bounded grace period.
//
// The overall budget is passed per call (not stored on the runner) because one
// worker process can run several jobs concurrently with different snapshots.
func (r *pythonRunner) run(ctx context.Context, timeout time.Duration,
	request pipelineRequest, progress progressFunc) (pipelineResult, error) {
	var result pipelineResult
	payload, err := json.Marshal(request)
	if err != nil {
		return result, err
	}
	if err := os.MkdirAll(request.WorkDir, 0o700); err != nil {
		return result, fmt.Errorf("create work directory: %w", err)
	}
	logPath := filepath.Join(request.WorkDir, "pipeline.stderr.log")
	stderrFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return result, fmt.Errorf("open pipeline log: %w", err)
	}
	defer stderrFile.Close()

	if timeout <= 0 {
		timeout = r.timeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	command := exec.Command(r.python, r.args...)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Dir = request.WorkDir
	command.Env = r.env
	command.Stdin = bytes.NewReader(payload)
	command.Stderr = stderrFile

	// stdout is our own pipe, not command.StdoutPipe(): that lets Wait run
	// concurrently with the reader, which is what makes a bounded TERM/KILL
	// escalation possible. os/exec's copy pipe would require reading to EOF
	// before Wait and could deadlock against a child that ignores signals.
	readPipe, writePipe, err := os.Pipe()
	if err != nil {
		return result, fmt.Errorf("pipeline stdout: %w", err)
	}
	defer readPipe.Close()
	command.Stdout = writePipe

	if err := command.Start(); err != nil {
		// A missing interpreter is deployment breakage, not a pipeline verdict.
		_ = writePipe.Close()
		return result, fmt.Errorf("start pipeline: %w", err)
	}
	_ = writePipe.Close()

	exited := make(chan error, 1)
	childDone := make(chan struct{})
	go func() {
		err := command.Wait()
		exited <- err
		close(childDone)
	}()

	// Watch for cancellation (user cancel, lost lease, overall timeout) and
	// terminate the whole process tree. The watcher also exits when the child
	// finishes on its own, so no goroutine outlives the run.
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		select {
		case <-runCtx.Done():
			r.terminate(command.Process.Pid, 5*time.Second)
		case <-childDone:
		}
	}()

	lineErr := r.consume(runCtx, readPipe, progress, &result)
	if lineErr != nil && runCtx.Err() == nil {
		// A protocol violation or a rejected progress update: the child is
		// still running and its output is untrustworthy.
		r.terminate(command.Process.Pid, 2*time.Second)
	}
	// Drain whatever the child already wrote so it is never blocked on a full
	// pipe while we wait for it. Termination above closes every writer, so this
	// reaches EOF even when the child had to be killed.
	_, _ = io.Copy(io.Discard, readPipe)
	waitErr := <-exited
	<-watchDone

	if runCtx.Err() != nil && ctx.Err() == nil {
		return result, &PipelineError{Code: "STAGE_TIMEOUT", Message: "处理阶段超时，请联系管理员调整资源或超时配置"}
	}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	if lineErr != nil {
		var pipelineFailure *PipelineError
		if errors.As(lineErr, &pipelineFailure) {
			return result, pipelineFailure
		}
		var violation *protocolError
		if errors.As(lineErr, &violation) {
			// Broken protocol: never publish, never expose raw output.
			r.logger.Error("pipeline protocol error", slog.Any("err", violation.err))
			return result, &PipelineError{Code: "PROCESSING_FAILED", Message: "处理失败，请联系管理员或重试"}
		}
		// A rejected progress update (lost lease, cancellation): the caller
		// decides the outcome, so propagate it unchanged.
		return result, lineErr
	}
	if result.Tracks == nil && result.Artifacts == nil {
		exitCode := -1
		if command.ProcessState != nil {
			exitCode = command.ProcessState.ExitCode()
		}
		r.logger.Error("pipeline exited without a terminal message",
			slog.Int("exit_code", exitCode), slog.Any("err", waitErr))
		return result, &PipelineError{Code: "PROCESSING_FAILED", Message: "处理失败，请联系管理员或重试"}
	}
	return result, nil
}

// consume reads JSON Lines from stdout. It stops at the first terminal message
// and rejects anything that follows it, so a misbehaving child is terminated
// rather than silently accepted.
func (r *pythonRunner) consume(ctx context.Context, stdout io.Reader, progress progressFunc, result *pipelineResult) error {
	reader := bufio.NewReaderSize(stdout, 64*1024)
	terminal := false
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		line, readErr := readLine(reader)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return &protocolError{err: readErr}
		}
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) > 0 {
			if terminal {
				return protocolViolation("pipeline message after terminal message")
			}
			var envelope struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(trimmed, &envelope); err != nil {
				return protocolViolation("invalid pipeline output: %w", err)
			}
			switch envelope.Type {
			case "progress":
				if err := r.handleProgress(trimmed, progress); err != nil {
					return err
				}
			case "result":
				var event terminalEvent
				if err := json.Unmarshal(trimmed, &event); err != nil {
					return protocolViolation("invalid result message: %w", err)
				}
				parsed, err := parsePipelineResult(event.Result)
				if err != nil {
					return &PipelineError{Code: "PROCESSING_FAILED", Message: "处理失败，请联系管理员或重试"}
				}
				*result = parsed
				terminal = true
			case "error":
				var event terminalEvent
				if err := json.Unmarshal(trimmed, &event); err != nil {
					return protocolViolation("invalid error message: %w", err)
				}
				code := strings.TrimSpace(event.Code)
				if code == "" || len(code) > 64 {
					code = "PROCESSING_FAILED"
				}
				message := truncate(strings.TrimSpace(event.Message), maxErrorRunes)
				if message == "" {
					message = "处理失败，请联系管理员或重试"
				}
				return &PipelineError{Code: code, Message: message}
			default:
				return protocolViolation("unknown pipeline message type %q", envelope.Type)
			}
		}
		if readErr != nil {
			// io.EOF: the caller decides whether a terminal message was required.
			return nil
		}
	}
}

func (r *pythonRunner) handleProgress(line []byte, progress progressFunc) error {
	if len(line) > maxProgressLine {
		return protocolViolation("progress line too large")
	}
	var event progressEvent
	if err := json.Unmarshal(line, &event); err != nil {
		return protocolViolation("invalid progress message: %w", err)
	}
	if !JobStages[event.Stage] {
		return protocolViolation("invalid pipeline stage %q", event.Stage)
	}
	fraction := event.StageProgress
	if fraction != nil {
		value := *fraction
		if value != value || value < 0 || value > 1 { // NaN or out of range
			return protocolViolation("invalid pipeline progress")
		}
	}
	return progress(event.Stage, fraction)
}

// readLine reads one newline-delimited record with a hard size bound so a
// runaway child cannot exhaust worker memory before the result cap applies.
func readLine(reader *bufio.Reader) ([]byte, error) {
	var buffer bytes.Buffer
	for {
		chunk, err := reader.ReadSlice('\n')
		if buffer.Len()+len(chunk) > maxResultBytes+maxProgressLine {
			return nil, protocolViolation("pipeline output exceeds size limit")
		}
		buffer.Write(chunk)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return buffer.Bytes(), err
	}
}

// terminate signals the child process group with TERM and escalates to KILL
// when the group has not exited within the grace period. It waits for the
// direct child to be reaped and for any remaining group member to disappear,
// so callers can rely on "the tree is gone" after it returns.
func (r *pythonRunner) terminate(pid int, grace time.Duration) {
	if pid <= 0 {
		return
	}
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	if waitGone(pid, grace) {
		return
	}
	r.logger.Warn("pipeline did not exit after SIGTERM; killing process group", slog.Int("pid", pid))
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	if !waitGone(pid, grace) {
		r.logger.Error("pipeline process group survived SIGKILL", slog.Int("pid", pid))
	}
}

// waitGone polls until no live (non-zombie) process remains in the group.
// Zombies are ignored: they only wait for reaping, which is the caller's Wait.
func waitGone(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if !processGroupAlive(pid) {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// processGroupAlive reports whether the group still has a live member.
func processGroupAlive(pid int) bool {
	err := syscall.Kill(-pid, 0)
	if err == nil {
		// The group exists, but it may only contain zombies awaiting Wait.
		return groupHasLiveMember(pid)
	}
	return errors.Is(err, syscall.EPERM)
}

// groupHasLiveMember scans /proc for a non-zombie process in the group. When
// /proc is unavailable the conservative answer is "alive".
func groupHasLiveMember(pid int) bool {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return true
	}
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name()[0] < '0' || entry.Name()[0] > '9' {
			continue
		}
		stat, readErr := os.ReadFile(filepath.Join("/proc", entry.Name(), "stat"))
		if readErr != nil {
			continue
		}
		fields := strings.Fields(string(stat))
		if len(fields) < 5 {
			continue
		}
		// fields[4] is the process group id; fields[2] is the state.
		if fields[4] != strconv.Itoa(pid) || fields[2] == "Z" {
			continue
		}
		return true
	}
	return false
}

// pipelineTimeout resolves the overall pipeline budget. A job snapshot may pin
// a longer timeout than the deployment default.
func pipelineTimeout(snapshot map[string]any, stageTimeoutSeconds int) time.Duration {
	if snapshot != nil {
		if raw, ok := snapshot["job_timeout_seconds"]; ok {
			switch value := raw.(type) {
			case float64:
				if value > 0 {
					return time.Duration(value) * time.Second
				}
			case json.Number:
				if parsed, err := value.Int64(); err == nil && parsed > 0 {
					return time.Duration(parsed) * time.Second
				}
			case int:
				if value > 0 {
					return time.Duration(value) * time.Second
				}
			}
		}
	}
	if environment := os.Getenv("PIPELINE_TIMEOUT_SECONDS"); environment != "" {
		if parsed, err := strconv.Atoi(environment); err == nil && parsed > 0 {
			return time.Duration(parsed) * time.Second
		}
	}
	stages := stageTimeoutSeconds
	if stages <= 0 {
		stages = 3600
	}
	// Several stages plus exports may each consume the stage budget.
	return time.Duration(stages) * 12 * time.Second
}

// close releases runner resources. The pipeline runner holds no long-lived
// handle today; the method exists so callers never depend on that detail.
func (r *pythonRunner) close() error { return nil }
