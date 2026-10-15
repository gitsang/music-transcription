"""Single maintenance process; database remains authoritative across broker loss."""
from datetime import timedelta
import logging
import shutil
import time
from sqlalchemy import select, delete
from .config import get_config
from .db import transaction
from .jobs import complete, purge, UNFINISHED
from .models import (Job, QueueOutbox, Artifact, Session, RateLimit, now)
from .security import audit
from .storage import initialize_storage, job_dir
from .worker import celery_app, beat_service

log = logging.getLogger(__name__)
MAX_RECOVERY_ATTEMPTS = 3


def recover_leases():
    with transaction(write=True) as db:
        jobs = db.scalars(select(Job).where(Job.status == 'running', Job.lease_expires_at <= now())
                          .with_for_update(skip_locked=True))
        for job in jobs:
            # Clearing the token while holding the row lock revokes ownership.
            # A late process can no longer renew or publish this attempt.
            job.lease_token, job.lease_expires_at = None, None
            if job.cancel_requested or job.delete_requested:
                complete(job, 'cancelled')
            elif job.attempt < MAX_RECOVERY_ATTEMPTS:
                job.attempt += 1
                job.status, job.stage, job.stage_progress = 'queued', None, None
                job.started_at = None
                job.warnings = list(job.warnings) + ['worker心跳中断，已恢复到队列']
                db.add(QueueOutbox(job_id=job.id, attempt=job.attempt))
            else:
                job.error_code, job.error_message = 'WORKER_LOST', '处理进程多次中断，请联系管理员'
                complete(job, 'failed')
            audit(db, None, 'job.lease_recovered', job.id, {'attempt': job.attempt, 'status': job.status})


def dispatch_outbox(sender=None):
    sender = sender or (lambda job_id, attempt: celery_app.send_task(
        'app.worker.process_job', args=[job_id, attempt]))
    delivered = 0
    # Resend queued jobs even when the previous send succeeded: Redis loss or
    # unacknowledged early worker failure must not strand a durable DB task.
    with transaction(write=True) as db:
        rows = db.scalars(select(QueueOutbox).join(Job).where(
            QueueOutbox.attempt == Job.attempt, Job.status == 'queued',
            Job.cancel_requested.is_(False), Job.delete_requested.is_(False),
            Job.deleted_at.is_(None), QueueOutbox.next_retry_at <= now())
            .order_by(QueueOutbox.created_at).limit(100).with_for_update(skip_locked=True, of=QueueOutbox))
        for row in rows:
            row.tries += 1
            try:
                sender(row.job_id, row.attempt)
            except Exception:
                log.warning('Outbox delivery failed for %s', row.job_id)
                row.next_retry_at = now() + timedelta(seconds=min(300, 2 ** min(row.tries, 8)))
            else:
                row.delivered_at = now()
                row.next_retry_at = now() + timedelta(seconds=60)
                delivered += 1
    return delivered


def cleanup():
    initialize_storage()
    with transaction(write=True) as db:
        jobs = db.scalars(select(Job).where(Job.status.not_in(UNFINISHED),
                         (Job.delete_requested.is_(True)) | (Job.expires_at <= now()))
                         .with_for_update(skip_locked=True))
        for job in jobs:
            # A terminal row should already have been fenced by complete() or
            # recover_leases(); clear any leftover token so purge cannot raise
            # and abort the whole sweep. A late worker then cannot write back.
            job.lease_token, job.lease_expires_at = None, None
            if job.deleted_at is None:
                audit(db, None, 'job.purged', job.id)
                purge(db, job)
            else:
                # Sweep a stale process's unreferenced work after lease fencing.
                shutil.rmtree(job_dir(job.user_id, job.id), ignore_errors=True)
        db.execute(delete(Session).where(Session.expires_at < now() - timedelta(days=1)))
        db.execute(delete(RateLimit).where(RateLimit.reset_at < now()))
        # Superseded attempts and terminal jobs keep no deliverable outbox rows.
        db.execute(delete(QueueOutbox).where(QueueOutbox.id.in_(select(QueueOutbox.id)
                     .join(Job, Job.id == QueueOutbox.job_id)
                     .where((QueueOutbox.attempt != Job.attempt) | Job.status.not_in(UNFINISHED)))))
        active = {j.id: j for j in db.scalars(select(Job).where(Job.deleted_at.is_(None)))}
        referenced = {a.storage_key for a in db.scalars(select(Artifact).where(Artifact.storage_key.is_not(None)))}
    cutoff = time.time() - 3600
    root = get_config().data_dir
    for temp in (root / 'uploads').glob('*'):
        try:
            if temp.is_file() and temp.stat().st_mtime < cutoff:
                temp.unlink(missing_ok=True)
        except OSError:
            log.warning('Could not remove stale upload %s', temp)
    # Uncommitted input/attempt publication left behind by a crash. Delay avoids
    # racing a currently uploading transaction that is not yet visible.
    for directory in (root / 'users').glob('*/jobs/*'):
        try:
            job = active.get(directory.name)
            if job is None:
                if directory.stat().st_mtime < cutoff:
                    shutil.rmtree(directory, ignore_errors=True)
                continue
            if job.status in UNFINISHED:
                continue
            work = directory / 'work'
            if work.exists() and work.stat().st_mtime < cutoff:
                shutil.rmtree(work, ignore_errors=True)
            results = directory / 'results'
            if results.exists():
                for file in results.rglob('*'):
                    if file.is_file() and file.stat().st_mtime < cutoff:
                        if str(file.relative_to(root)) not in referenced:
                            file.unlink(missing_ok=True)
        except OSError:
            # A concurrent delete/rename must not abort the rest of the sweep.
            log.warning('Cleanup skipped for %s', directory)


def tick(sender=None):
    beat_service('dispatcher')
    recover_leases()
    delivered = dispatch_outbox(sender)
    cleanup()
    return delivered


def run():
    initialize_storage()
    while True:
        try:
            tick()
        except Exception:
            log.exception('Dispatcher maintenance failed')
        time.sleep(5)
