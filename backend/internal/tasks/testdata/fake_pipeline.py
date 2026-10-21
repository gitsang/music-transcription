#!/usr/bin/env python3
"""Test-only stand-in for `python -m pipeline.cli`.

The Go worker spawns a real child process, so the fake is a real process too:
it reads the JSON request from stdin, then emits JSON Lines exactly like the
contracted pipeline protocol.

Selected by the `pipe_fake.py` base name (the Go side may build
["<python>", "-m", "pipeline.cli"] OR a launcher), and by FAKE_* environment
variables:

  FAKE_MODE            success | error | garbage | silent | big | hang |
                       cancel | exit_without_result | fail_artifact | escape_path
  FAKE_PID_FILE        where the child writes its pid (and its grandchild's)
  FAKE_SLEEP_GRACE     seconds the child sleeps on SIGTERM before exiting
  FAKE_BIG_ARTIFACT    bytes for the oversized artifact in `big` mode
"""
import json
import os
import signal
import subprocess
import sys
import time
import uuid
from pathlib import Path


def emit(payload):
    sys.stdout.write(json.dumps(payload, separators=(",", ":")) + "\n")
    sys.stdout.flush()


def progress(stage, fraction=None):
    emit({"type": "progress", "stage": stage, "stage_progress": fraction})


def track(label="Piano", program=0, confidence=0.9, source="model"):
    return {
        "id": str(uuid.uuid4()),
        "label": label,
        "program": program,
        "confidence": confidence,
        "label_source": source,
        "is_drum": False,
        "source_stem_ids": [],
    }


def artifact(work, name, content, kind="score", mime="application/xml",
             track_id=None, status="ready"):
    path = Path(work) / name
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(content, encoding="utf-8")
    return {
        "track_id": track_id,
        "kind": kind,
        "label": name,
        "mime": mime,
        "status": status,
        "error": "",
        "path": name,
    }


def child_writer(pid_file):
    """Write the pid file, and keep a grandchild alive to prove tree cleanup."""
    grandchild = subprocess.Popen(
        [sys.executable, "-c", "import time; time.sleep(120)"],
        start_new_session=False,
    )
    Path(pid_file).write_text(
        json.dumps({"pid": os.getpid(), "grandchild": grandchild.pid}),
        encoding="utf-8",
    )


def main():
    raw = sys.stdin.read()
    request = json.loads(raw)
    work = request["work_dir"]
    mode = os.environ.get("FAKE_MODE", "success")
    pid_file = os.environ.get("FAKE_PID_FILE", "")
    grace = float(os.environ.get("FAKE_SLEEP_GRACE", "0"))

    def on_term(signum, frame):
        if grace > 0:
            time.sleep(grace)
        sys.exit(143)

    signal.signal(signal.SIGTERM, on_term)

    if pid_file:
        child_writer(pid_file)

    progress("validating", None)
    progress("preparing", 0.5)

    if mode == "error":
        emit({"type": "error", "code": "UNSUPPORTED_AUDIO",
              "message": "无法解码该音频"})
        return 0
    if mode == "garbage":
        sys.stdout.write("this is not json\n")
        sys.stdout.flush()
        return 0
    if mode == "unknown_type":
        emit({"type": "telemetry", "stage": "scoring"})
        return 0
    if mode == "bad_stage":
        emit({"type": "progress", "stage": "downloading", "stage_progress": 0.5})
        emit({"type": "result", "result": {"tracks": [], "artifacts": []}})
        return 0
    if mode == "bad_fraction":
        emit({"type": "progress", "stage": "scoring", "stage_progress": 1.5})
        return 0
    if mode == "progress_after_result":
        emit({"type": "result", "result": {"tracks": [], "artifacts": []}})
        # Keep streaming: the Go side must stop reading after the terminal
        # message and never let the child stall the worker.
        for _ in range(200):
            progress("scoring", 0.5)
            time.sleep(0.01)
        return 0
    if mode == "duplicate_result":
        emit({"type": "result", "result": {"tracks": [], "artifacts": []}})
        emit({"type": "result", "result": {"tracks": [], "artifacts": []}})
        return 0
    if mode == "bad_result":
        emit({"type": "result", "result": {"tracks": "not-a-list"}})
        return 0
    if mode == "silent":
        return 0
    if mode == "exit_without_result":
        sys.stdout.write(json.dumps({"type": "progress", "stage": "scoring",
                                     "stage_progress": 0.3}) + "\n")
        sys.stdout.flush()
        return 0
    if mode == "hang":
        while True:
            time.sleep(0.2)
            progress("analyzing", 0.1)
    if mode == "cancel":
        for _ in range(600):
            time.sleep(0.05)
            progress("transcribing", 0.2)
        return 0

    tracks = [track()]
    artifacts = [artifact(work, "score.musicxml", "<score/>", track_id=tracks[0]["id"])]

    if mode == "fail_artifact":
        artifacts.append({
            "track_id": None, "kind": "pdf", "label": "score.pdf",
            "mime": "application/pdf", "status": "failed",
            "error": "musescore 不可用", "path": "",
        })
    if mode == "escape_path":
        outside = Path(work).parent / "escaped.txt"
        outside.write_text("escaped", encoding="utf-8")
        artifacts.append({
            "track_id": None, "kind": "midi", "label": "escaped.mid",
            "mime": "audio/midi", "status": "ready", "error": "",
            "path": "../escaped.txt",
        })
    if mode == "big":
        size = int(os.environ.get("FAKE_BIG_ARTIFACT", str(64 << 20)))
        big = Path(work) / "big.mid"
        with big.open("wb") as handle:
            handle.write(b"MThd" + b"\0" * (size - 4))
        artifacts.append({
            "track_id": None, "kind": "midi", "label": "big.mid",
            "mime": "audio/midi", "status": "ready", "error": "",
            "path": "big.mid",
        })

    for stage, fraction in (("analyzing", 0.25), ("separating", 0.4),
                            ("transcribing", 0.7), ("scoring", 0.9),
                            ("exporting", 1.0)):
        progress(stage, fraction)

    emit({
        "type": "result",
        "result": {
            "tracks": tracks,
            "artifacts": artifacts,
            "warnings": ["测试警告"],
            "engine": "baseline",
            "actual_device": "cpu",
        },
    })
    return 0


if __name__ == "__main__":
    sys.exit(main())
