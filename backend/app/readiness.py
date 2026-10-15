"""Readiness checks. The pipeline owns capability truth; the API only reads it."""
import json
import os
from pathlib import Path
import shutil
from sqlalchemy import text, select
from .config import get_config
from .db import transaction
from .models import ServiceHeartbeat, now


LIMITATIONS = ['自动扒谱初稿，可能漏音、错音及节奏错误',
               '分离器粗分类，未实现任意乐器识别',
               'other为未知/混合声部，无法区分同类乐器的多个演奏者']


def pipeline_capabilities():
    """Authoritative engine list from pipeline.service; None when unavailable.

    The API process does not install torch/models, so pipeline.service may fail
    to import. A missing pipeline must not silently claim an engine is ready.
    """
    try:
        from pipeline.service import capabilities as declared
        return declared()
    except Exception:
        return None


def baseline_ready():
    cfg = get_config()
    # The operator preparation script verifies hashes and writes manifest.json.
    # API checks shared files only; imports/inference live in the worker venv.
    root = cfg.demucs_repo.parent
    try:
        manifest = json.loads((root / 'manifest.json').read_text())
        models = {model['id']: model for model in manifest['models']}
        required = ('htdemucs_6s', 'basic_pitch_onnx')
        files_ready = all((root / models[name]['path']).is_file() and
                          (root / models[name]['path']).stat().st_size > 0 and
                          len(models[name]['sha256']) == 64 for name in required)
    except (OSError, ValueError, KeyError, TypeError):
        files_ready = False
    return (cfg.model_python.is_file() and os.access(cfg.model_python, os.X_OK)
            and (cfg.demucs_repo / 'htdemucs_6s.yaml').is_file() and files_ready
            and shutil.which('ffmpeg') is not None and shutil.which('ffprobe') is not None)


def capabilities():
    declared = pipeline_capabilities()
    if declared is not None:
        return declared
    return [{'id': 'baseline', 'label': 'Demucs 6类 + Basic Pitch（有限能力）',
             'ready': baseline_ready(), 'limitations': LIMITATIONS}]


def health(role='api'):
    checks = {}
    try:
        with transaction() as db:
            db.execute(text('SELECT 1'))
            if role in ('worker', 'dispatcher'):
                beat = db.get(ServiceHeartbeat, role)
                checks['heartbeat'] = bool(beat and (now() - beat.updated_at).total_seconds() < 90)
        checks['database'] = True
    except Exception:
        checks['database'] = False
    try:
        import redis
        checks['broker'] = bool(redis.Redis.from_url(get_config().redis_url,
                                socket_connect_timeout=2, socket_timeout=2).ping())
    except Exception:
        checks['broker'] = False
    root = get_config().data_dir
    checks['storage'] = root.is_dir() and os.access(root, os.R_OK | os.W_OK)
    # A missing model must not disable account and MusicXML functionality.
    return {'status': 'ready' if all(checks.values()) else 'not_ready',
            'checks': checks, 'audio_ready': any(e['id'] == 'baseline' and e['ready'] for e in capabilities())}
