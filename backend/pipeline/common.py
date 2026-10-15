import os
from pathlib import Path
import signal
import subprocess
import time


class PipelineError(Exception):
    def __init__(self, code, message):
        super().__init__(message)
        self.code, self.message = code, message


def run(command, directory: Path, progress, stage, timeout=None):
    """No shell, bounded subprocess, cancellation check and process-group cleanup."""
    progress(stage, None)
    directory.mkdir(parents=True, exist_ok=True)
    log = directory / f'{stage}.log'
    env = {**os.environ, 'OMP_NUM_THREADS': os.getenv('OMP_NUM_THREADS', '2'),
           'OPENBLAS_NUM_THREADS': os.getenv('OPENBLAS_NUM_THREADS', '2'),
           'MKL_NUM_THREADS': os.getenv('MKL_NUM_THREADS', '2'),
           'HF_HUB_OFFLINE': '1', 'QT_QPA_PLATFORM': 'offscreen'}
    limit = timeout or int(os.getenv('STAGE_TIMEOUT_SECONDS', '3600'))
    with log.open('ab') as output:
        try:
            process = subprocess.Popen([str(x) for x in command], stdout=output,
                                       stderr=output, env=env, start_new_session=True)
        except OSError as exc:
            raise PipelineError('TOOL_NOT_READY', '本地处理工具不可用，请联系管理员') from exc
        start = time.monotonic()
        try:
            while process.poll() is None:
                progress(stage, None)
                if time.monotonic() - start > limit:
                    raise PipelineError('STAGE_TIMEOUT', '处理阶段超时，请联系管理员调整资源或超时配置')
                time.sleep(.4)
            if process.returncode:
                tail = log.read_bytes()[-16384:].decode('utf-8', errors='replace').lower()
                if 'out of memory' in tail or 'cannot allocate memory' in tail:
                    raise PipelineError('OUT_OF_MEMORY', '推理内存或显存不足；请管理员调整分块或设备后重试')
                raise PipelineError('PROCESSING_ERROR', '本地处理工具执行失败，请联系管理员检查日志')
        finally:
            if process.poll() is None:
                os.killpg(process.pid, signal.SIGTERM)
                try:
                    process.wait(timeout=3)
                except subprocess.TimeoutExpired:
                    os.killpg(process.pid, signal.SIGKILL)
                    process.wait()
