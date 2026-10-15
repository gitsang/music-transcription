"""Deployment configuration; database settings are snapshotted separately per job."""
from dataclasses import dataclass
from functools import lru_cache
import os
from pathlib import Path


@dataclass(frozen=True)
class Config:
    database_url: str
    redis_url: str
    data_dir: Path
    public_origin: str
    cookie_secure: bool
    session_seconds: int
    lease_seconds: int
    disk_min_free_bytes: int
    stage_timeout_seconds: int
    model_python: Path
    demucs_repo: Path


@lru_cache
def get_config() -> Config:
    return Config(
        database_url=os.getenv('DATABASE_URL', 'postgresql+psycopg://music:music@postgres/music'),
        redis_url=os.getenv('REDIS_URL', 'redis://redis:6379/0'),
        data_dir=Path(os.getenv('DATA_DIR', '/data')).resolve(),
        public_origin=os.getenv('PUBLIC_ORIGIN', 'http://localhost:8080').rstrip('/'),
        cookie_secure=os.getenv('COOKIE_SECURE', 'true').lower() == 'true',
        session_seconds=int(os.getenv('SESSION_SECONDS', '86400')),
        lease_seconds=int(os.getenv('LEASE_SECONDS', '120')),
        disk_min_free_bytes=int(os.getenv('DISK_MIN_FREE_BYTES', '1073741824')),
        stage_timeout_seconds=int(os.getenv('STAGE_TIMEOUT_SECONDS', '3600')),
        model_python=Path(os.getenv('MODEL_PYTHON', '/opt/models-venv/bin/python')),
        demucs_repo=Path(os.getenv('DEMUCS_REPO', '/models/demucs')),
    )


DEFAULT_SETTINGS = {
    'max_audio_duration_seconds': 600,
    'max_unfinished_jobs_per_user': 3,
    'retention_days': None,
    'max_upload_bytes': 209715200,
    'default_engine': 'baseline',
}
