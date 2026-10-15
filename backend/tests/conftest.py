"""Isolated SQLite database and temporary data root for the whole test session.

The environment must be configured before importing app modules because
app.config.get_config and app.db.engine are both lru_cached.
"""
import os
from pathlib import Path
import sys
import tempfile

BACKEND_ROOT = Path(__file__).resolve().parents[1]
if str(BACKEND_ROOT) not in sys.path:
    sys.path.insert(0, str(BACKEND_ROOT))

_SESSION_TMP = Path(tempfile.mkdtemp(prefix='music-transcription-tests-'))
# Never inherit a production database: these tests deliberately drop tables.
os.environ['DATABASE_URL'] = f'sqlite:///{_SESSION_TMP / "test.db"}'
os.environ['DATA_DIR'] = str(_SESSION_TMP / 'data')
os.environ.setdefault('DISK_MIN_FREE_BYTES', '0')
os.environ.setdefault('PUBLIC_ORIGIN', 'http://localhost:8080')
os.environ.setdefault('COOKIE_SECURE', 'false')
os.environ.setdefault('LEASE_SECONDS', '120')
os.environ.setdefault('SESSION_SECONDS', '3600')
os.environ.setdefault('REDIS_URL', 'redis://127.0.0.1:6399/0')

import pytest  # noqa: E402
from app.db import engine  # noqa: E402
from app.models import Base  # noqa: E402
from app.storage import initialize_storage  # noqa: E402


@pytest.fixture(autouse=True)
def isolated_database():
    Base.metadata.drop_all(engine())
    Base.metadata.create_all(engine())
    initialize_storage()
    yield
    Base.metadata.drop_all(engine())


@pytest.fixture
def client():
    from fastapi.testclient import TestClient
    from app.main import app
    with TestClient(app) as test_client:
        yield test_client


@pytest.fixture
def data_dir():
    from app.config import get_config
    return get_config().data_dir
