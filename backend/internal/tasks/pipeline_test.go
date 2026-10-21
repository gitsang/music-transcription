package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeRunner builds a pythonRunner that executes the testdata fake pipeline
// instead of `python -m pipeline.cli`. The fake script entry point is injected
// through PYTHONSTARTUP-free means: the production command line is preserved in
// every other respect (argv shape, env, process group, stdio).
func fakeRunner(t *testing.T, mode string, timeout time.Duration) (*pythonRunner, map[string]string) {
	t.Helper()
	script := testdataPath(t, "fake_pipeline.py")
	if overridden := os.Getenv(envTestFakePython); overridden != "" {
		script = overridden
	}
	state := map[string]string{"FAKE_MODE": mode}
	env := []string{
		"FAKE_MODE=" + mode,
		"PATH=" + os.Getenv("PATH"),
		"PYTHONPATH=" + filepath.Dir(script),
	}
	runner := newPythonRunner("python3", env, timeout, nil)
	// Replace the module invocation with the fixture path so the child is the
	// real interpreter running real Python.
	runner.args = []string{script}
	return runner, state
}

// runFakeThread executes the fake in the current process group of the test, and
// must always be cleaned up by the runner.
func TestPipelineSuccessStreamsProgressAndResult(t *testing.T) {
	runner, _ := fakeRunner(t, "success", 30*time.Second)
	workDir := workspaceDir(t)
	var stages []string
	result, err := runner.run(context.Background(), 0, pipelineRequest{
		InputPath: "/inputs/song.wav", WorkDir: workDir, Kind: "audio",
		Config: map[string]any{"retention_days": nil},
	}, func(stage string, fraction *float64) error {
		stages = append(stages, stage)
		return nil
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(result.Tracks) != 1 {
		t.Fatalf("tracks = %+v", result.Tracks)
	}
	if len(result.Artifacts) != 1 || result.Artifacts[0].Path != "score.musicxml" {
		t.Fatalf("artifacts = %+v", result.Artifacts)
	}
	if result.Engine != "baseline" || result.ActualDevice != "cpu" {
		t.Fatalf("engine/device = %q/%q", result.Engine, result.ActualDevice)
	}
	if len(result.Warnings) != 1 {
		t.Fatalf("warnings = %+v", result.Warnings)
	}
	want := []string{"validating", "preparing", "analyzing", "separating",
		"transcribing", "scoring", "exporting"}
	if strings.Join(stages, ",") != strings.Join(want, ",") {
		t.Fatalf("stages = %v, want %v", stages, want)
	}
	if _, err := os.Stat(filepath.Join(workDir, "score.musicxml")); err != nil {
		t.Fatalf("fixture artifact missing: %v", err)
	}
	// stderr stays out of the public output but is captured for operators.
	if _, err := os.Stat(filepath.Join(workDir, "pipeline.stderr.log")); err != nil {
		t.Fatalf("stderr log missing: %v", err)
	}
}

func TestPipelineErrorIsSanitized(t *testing.T) {
	runner, _ := fakeRunner(t, "error", 30*time.Second)
	_, err := runner.run(context.Background(), 0, pipelineRequest{
		InputPath: "/x", WorkDir: workspaceDir(t), Kind: "audio", Config: map[string]any{},
	}, func(string, *float64) error { return nil })
	var pipelineFailure *PipelineError
	if !errors.As(err, &pipelineFailure) {
		t.Fatalf("err = %v, want a PipelineError", err)
	}
	if pipelineFailure.Code != "UNSUPPORTED_AUDIO" || pipelineFailure.Message != "无法解码该音频" {
		t.Fatalf("sanitized failure = %+v", pipelineFailure)
	}
}

func TestPipelineProtocolViolationsFail(t *testing.T) {
	cases := map[string]string{
		"garbage":               "PROCESSING_FAILED",
		"unknown_type":          "PROCESSING_FAILED",
		"bad_stage":             "PROCESSING_FAILED",
		"bad_fraction":          "PROCESSING_FAILED",
		"progress_after_result": "PROCESSING_FAILED",
		"duplicate_result":      "PROCESSING_FAILED",
		"bad_result":            "PROCESSING_FAILED",
		"silent":                "PROCESSING_FAILED",
		"exit_without_result":   "PROCESSING_FAILED",
	}
	for mode, code := range cases {
		t.Run(mode, func(t *testing.T) {
			runner, _ := fakeRunner(t, mode, 30*time.Second)
			_, err := runner.run(context.Background(), 0, pipelineRequest{
				InputPath: "/x", WorkDir: workspaceDir(t), Kind: "audio", Config: map[string]any{},
			}, func(string, *float64) error { return nil })
			var pipelineFailure *PipelineError
			if !errors.As(err, &pipelineFailure) {
				t.Fatalf("mode %s: err = %v", mode, err)
			}
			if pipelineFailure.Code != code {
				t.Fatalf("mode %s: code = %q, want %q", mode, pipelineFailure.Code, code)
			}
		})
	}
}

func TestPipelineCancelKillsProcessGroup(t *testing.T) {
	runner, _ := fakeRunner(t, "hang", 5*time.Minute)
	workDir := workspaceDir(t)
	pidFile := filepath.Join(workDir, "pids.json")
	runner.env = append(runner.env, "FAKE_PID_FILE="+pidFile)

	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	go func() {
		<-started
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()

	_, err := runner.run(ctx, 0, pipelineRequest{
		InputPath: "/x", WorkDir: workDir, Kind: "audio", Config: map[string]any{},
	}, func(stage string, fraction *float64) error {
		select {
		case <-started:
		default:
			close(started)
		}
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	assertProcessGone(t, pidFile)
}

func TestPipelineTimeoutReportsStageTimeout(t *testing.T) {
	runner, _ := fakeRunner(t, "hang", 300*time.Millisecond)
	workDir := workspaceDir(t)
	pidFile := filepath.Join(workDir, "pids.json")
	runner.env = append(runner.env, "FAKE_PID_FILE="+pidFile)

	start := time.Now()
	_, err := runner.run(context.Background(), 0, pipelineRequest{
		InputPath: "/x", WorkDir: workDir, Kind: "audio", Config: map[string]any{},
	}, func(string, *float64) error { return nil })
	elapsed := time.Since(start)

	var pipelineFailure *PipelineError
	if !errors.As(err, &pipelineFailure) || pipelineFailure.Code != "STAGE_TIMEOUT" {
		t.Fatalf("err = %v, want STAGE_TIMEOUT", err)
	}
	if elapsed > 60*time.Second {
		t.Fatalf("timeout enforcement took %s", elapsed)
	}
	assertProcessGone(t, pidFile)
}

func TestPipelineProgressErrorAbortsChild(t *testing.T) {
	runner, _ := fakeRunner(t, "hang", 5*time.Minute)
	workDir := workspaceDir(t)
	pidFile := filepath.Join(workDir, "pids.json")
	runner.env = append(runner.env, "FAKE_PID_FILE="+pidFile)

	leaseLost := errors.New("lease lost")
	_, err := runner.run(context.Background(), 0, pipelineRequest{
		InputPath: "/x", WorkDir: workDir, Kind: "audio", Config: map[string]any{},
	}, func(string, *float64) error { return leaseLost })
	if !errors.Is(err, leaseLost) {
		t.Fatalf("err = %v, want the progress error", err)
	}
	assertProcessGone(t, pidFile)
}

func TestPipelineMissingInterpreterIsDeploymentError(t *testing.T) {
	runner := newPythonRunner("/nonexistent/python3", []string{"PATH=/usr/bin"}, 5*time.Second, nil)
	runner.args = []string{testdataPath(t, "fake_pipeline.py")}
	_, err := runner.run(context.Background(), 0, pipelineRequest{
		InputPath: "/x", WorkDir: workspaceDir(t), Kind: "audio", Config: map[string]any{},
	}, func(string, *float64) error { return nil })
	var pipelineFailure *PipelineError
	if err == nil || errors.As(err, &pipelineFailure) {
		t.Fatalf("err = %v, want a raw start error (never a user-facing verdict)", err)
	}
}

func TestPipelineTimeoutResolution(t *testing.T) {
	t.Setenv("PIPELINE_TIMEOUT_SECONDS", "")
	if got := pipelineTimeout(nil, 60); got != 12*time.Minute {
		t.Fatalf("default = %s", got)
	}
	if got := pipelineTimeout(nil, 0); got != 12*time.Hour {
		t.Fatalf("zero stage default = %s", got)
	}
	if got := pipelineTimeout(map[string]any{"job_timeout_seconds": float64(90)}, 60); got != 90*time.Second {
		t.Fatalf("snapshot float = %s", got)
	}
	if got := pipelineTimeout(map[string]any{"job_timeout_seconds": json.Number("120")}, 60); got != 120*time.Second {
		t.Fatalf("snapshot json.Number = %s", got)
	}
	if got := pipelineTimeout(map[string]any{"job_timeout_seconds": 45}, 60); got != 45*time.Second {
		t.Fatalf("snapshot int = %s", got)
	}
	if got := pipelineTimeout(map[string]any{"job_timeout_seconds": "nonsense"}, 60); got != 12*time.Minute {
		t.Fatalf("invalid snapshot = %s", got)
	}
	t.Setenv("PIPELINE_TIMEOUT_SECONDS", "300")
	if got := pipelineTimeout(nil, 60); got != 5*time.Minute {
		t.Fatalf("environment = %s", got)
	}
}

// assertProcessGone verifies the child and its grandchild were terminated.
func assertProcessGone(t *testing.T, pidFile string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(pidFile)
		if err != nil {
			// No pid file: the fake never started its grandchild.
			return
		}
		var pids struct {
			PID        int `json:"pid"`
			Grandchild int `json:"grandchild"`
		}
		if err := json.Unmarshal(raw, &pids); err != nil {
			t.Fatalf("decode pids: %v", err)
		}
		if !processAlive(pids.PID) && !processAlive(pids.Grandchild) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	raw, _ := os.ReadFile(pidFile)
	t.Fatalf("pipeline process tree still alive after cancellation: %s", raw)
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	// signal 0 probes existence without delivering anything.
	if err := syscall.Kill(pid, 0); err != nil {
		return false
	}
	// A zombie still answers kill(0); treat it as gone once reaped.
	stat, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return false
	}
	fields := strings.Fields(string(stat))
	return len(fields) < 3 || fields[2] != "Z"
}

// TestRealPipelineCLIProtocol runs the retained `python -m pipeline.cli` (not the
// fake) to confirm the wire contract: one request on stdin, JSON Lines progress
// and exactly one terminal message. It is opt-in because it needs the pipeline
// dependencies installed; MUSIC_TEST_PIPELINE_PYTHON points at that interpreter
// and the CLI is resolved from the repository root, so PYTHONPATH must contain
// the backend directory (the worker sets PYTHONPATH=/app in production).
func TestRealPipelineCLIProtocol(t *testing.T) {
	python := os.Getenv(envTestPipelinePython)
	if python == "" {
		t.Skipf("%s is not set; skipping real pipeline CLI test", envTestPipelinePython)
	}
	// packageDir is backend/internal/tasks; the pipeline package lives two levels up.
	backendDir := filepath.Dir(filepath.Dir(packageDir(t)))
	workDir := workspaceDir(t)
	input := writeFile(t, workDir, "input.wav", "not really audio")

	// Sanitized, user-facing failures are expected without models: what matters
	// is that the child speaks the protocol and never leaks a traceback.
	// The child runs with the attempt directory as cwd (as in production), so the
	// pipeline package must be found through PYTHONPATH.
	runner := newPythonRunner(python, []string{
		"PATH=" + os.Getenv("PATH"),
		"PYTHONPATH=" + backendDir,
		"PYTHONUNBUFFERED=1",
	}, 3*time.Minute, nil)

	var stages []string
	result, err := runner.run(context.Background(), 0, pipelineRequest{
		InputPath: input, WorkDir: filepath.Join(workDir, "work"), Kind: "audio",
		Config: map[string]any{},
	}, func(stage string, fraction *float64) error {
		stages = append(stages, stage)
		return nil
	})
	if err == nil {
		// With models present the run must produce a valid, publishable result.
		if len(result.Artifacts) == 0 {
			t.Fatalf("successful run published no artifacts: %+v", result)
		}
		return
	}
	var pipelineFailure *PipelineError
	if !errors.As(err, &pipelineFailure) {
		t.Fatalf("err = %v, want a sanitized PipelineError", err)
	}
	if pipelineFailure.Code == "PROCESSING_FAILED" && pipelineFailure.Message == "" {
		t.Fatalf("failure without a message: %+v", pipelineFailure)
	}
	if strings.Contains(pipelineFailure.Message, "Traceback") ||
		strings.Contains(pipelineFailure.Message, "/opt/") {
		t.Fatalf("failure leaked diagnostics: %q", pipelineFailure.Message)
	}
	if len(stages) == 0 {
		t.Fatal("the real CLI emitted no progress before failing")
	}
}
