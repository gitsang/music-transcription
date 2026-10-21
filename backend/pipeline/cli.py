"""Private JSON Lines bridge for the Go worker; stdout is protocol only."""
import contextlib
import json
import signal
import sys
import traceback
from pathlib import Path

MAX_REQUEST_BYTES = 1024 * 1024


class Cancelled(BaseException):
    """Unwinds subprocess finally blocks rather than interrupting cleanup twice."""


def main():
    protocol = sys.stdout
    terminal = False

    def emit(message):
        protocol.write(json.dumps(message, ensure_ascii=False, allow_nan=False) + '\n')
        protocol.flush()

    def stop(signum, frame):
        signal.signal(signal.SIGTERM, signal.SIG_IGN)
        signal.signal(signal.SIGINT, signal.SIG_IGN)
        raise Cancelled()

    signal.signal(signal.SIGTERM, stop)
    signal.signal(signal.SIGINT, stop)
    try:
        raw = sys.stdin.buffer.read(MAX_REQUEST_BYTES + 1)
        if len(raw) > MAX_REQUEST_BYTES:
            raise ValueError('request too large')
        request = json.loads(raw)
        if not isinstance(request, dict) or request.get('kind') not in ('audio', 'musicxml'):
            raise ValueError('invalid kind')
        config = request.get('config', {})
        if not isinstance(config, dict):
            raise ValueError('invalid configuration')
        input_path = Path(request['input_path'])
        work_dir = Path(request['work_dir'])
        if not input_path.is_absolute() or not work_dir.is_absolute():
            raise ValueError('paths must be absolute')
        with contextlib.redirect_stdout(sys.stderr):
            from .service import process, PipelineError
            result = process(input_path, work_dir, request['kind'], config,
                             lambda stage, fraction: emit({'type': 'progress', 'stage': stage,
                                                           'stage_progress': fraction}))
        emit({'type': 'result', 'result': result})
        terminal = True
        return 0
    except Cancelled:
        return 130
    except Exception as exc:
        from .common import PipelineError
        traceback.print_exc(file=sys.stderr)
        if not terminal:
            code = exc.code if isinstance(exc, PipelineError) else 'PROCESSING_FAILED'
            message = exc.message if isinstance(exc, PipelineError) else '处理失败，请联系管理员检查服务日志'
            emit({'type': 'error', 'code': code, 'message': message})
        return 1


if __name__ == '__main__':
    raise SystemExit(main())
