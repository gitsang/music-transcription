from datetime import timedelta
import mimetypes
import os
import re
import shutil
from pathlib import Path
from sqlalchemy import select, func, delete
from .config import get_config
from .db import lock_user
from .errors import ApiError
from .models import Job, Artifact, Track, QueueOutbox, now
from .security import audit
from .storage import job_dir, private_path

UNFINISHED = ('queued', 'running')


def authorized_job(db, job_id, user, lock=False):
    query = select(Job).where(Job.id == job_id, Job.deleted_at.is_(None),
                             Job.delete_requested.is_(False))
    if user.role != 'admin':
        query = query.where(Job.user_id == user.id)
    if lock:
        query = query.with_for_update()
    job = db.scalar(query)
    if job is None:
        raise ApiError('NOT_FOUND', '任务不存在', 404)
    return job


def check_quota(db, user_id, config):
    user = lock_user(db, user_id)
    if user is None or user.status != 'active':
        raise ApiError('AUTH_REQUIRED', '会话已失效', 401)
    count = db.scalar(select(func.count()).select_from(Job).where(
        Job.user_id == user_id, Job.status.in_(UNFINISHED), Job.deleted_at.is_(None)))
    if count >= config['max_unfinished_jobs_per_user']:
        raise ApiError('JOB_LIMIT_REACHED', '未完成任务数量已达上限', 409)


def complete(job, status):
    job.status = status
    job.completed_at = now()
    days = job.config_snapshot.get('retention_days')
    job.expires_at = job.completed_at + timedelta(days=days) if days else None
    job.lease_token, job.lease_expires_at = None, None


def clear_derived(db, job):
    db.execute(delete(Artifact).where(Artifact.job_id == job.id, Artifact.kind != 'original'))
    db.execute(delete(Track).where(Track.job_id == job.id))


def purge(db, job):
    # Caller must hold the job lock and have already fenced any executing lease.
    if job.status in UNFINISHED or job.lease_token is not None:
        raise ValueError('Cannot purge an executing lease')
    # A lease can expire while the old process is still unwinding. Wait for
    # the executor's filesystem lock before removing files, even after fencing.
    import fcntl
    lock_path = get_config().data_dir / 'locks' / (job.id + '.lock')
    with lock_path.open('a') as lock:
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            return False
        shutil.rmtree(job_dir(job.user_id, job.id), ignore_errors=False) if job_dir(job.user_id, job.id).exists() else None
    db.execute(delete(Artifact).where(Artifact.job_id == job.id))
    db.execute(delete(Track).where(Track.job_id == job.id))
    db.execute(delete(QueueOutbox).where(QueueOutbox.job_id == job.id))
    job.deleted_at = now()
    job.delete_requested = True
    job.config_snapshot = {}
    job.warnings = []
    job.title = '已删除任务'
    job.error_message = None
    return True


def safe_title(filename):
    filename = (filename or 'upload').replace('\\', '/').rsplit('/', 1)[-1]
    return re.sub(r'[\x00-\x1f\x7f]', '', filename)[:255] or 'upload'


def extension(kind, title):
    suffix = Path(title).suffix.lower()
    allowed = { 'audio': {'.wav', '.mp3', '.flac', '.m4a', '.ogg'},
                'musicxml': {'.xml', '.musicxml', '.mxl'}}
    if kind not in allowed or suffix not in allowed[kind]:
        raise ApiError('INVALID_INPUT', '不支持的上传类型')
    return suffix
