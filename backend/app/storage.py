import os
from pathlib import Path
import shutil
from .config import get_config
from .errors import ApiError


def initialize_storage():
    root = get_config().data_dir
    root.mkdir(parents=True, exist_ok=True, mode=0o700)
    for name in ('users', 'uploads', 'locks'):
        (root / name).mkdir(exist_ok=True, mode=0o700)


def ensure_space():
    initialize_storage()
    if shutil.disk_usage(get_config().data_dir).free < get_config().disk_min_free_bytes:
        raise ApiError('DISK_LOW', '存储空间不足，暂时无法接受新任务', 503)


def job_dir(user_id, job_id):
    return get_config().data_dir / 'users' / user_id / 'jobs' / job_id


def private_path(key):
    root = get_config().data_dir
    path = (root / key).resolve()
    if not path.is_relative_to(root) or Path(key).is_absolute():
        raise ValueError('Invalid storage key')
    return path


def storage_key(path):
    return str(path.resolve().relative_to(get_config().data_dir))


def sync_file(path):
    with path.open('rb') as file:
        os.fsync(file.fileno())


def sync_dir(path):
    fd = os.open(path, os.O_RDONLY | os.O_DIRECTORY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)
