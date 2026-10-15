"""Trusted local pipeline executor. DB lease tokens fence all mutations/publication."""
from datetime import timedelta
import hashlib
import fcntl
import logging
import math
import os
from pathlib import Path
import shutil
import threading
import uuid

from celery import Celery, signals
from sqlalchemy import select, update
from .config import get_config
from .db import transaction
from .jobs import complete, clear_derived
from .models import Job, Artifact, Track, ServiceHeartbeat, now, uid
from .security import audit
from .storage import job_dir, private_path, storage_key, sync_file, sync_dir

log = logging.getLogger(__name__)
celery_app = Celery('music', broker=get_config().redis_url)
celery_app.conf.update(task_serializer='json', accept_content=['json'],
                       worker_concurrency=int(os.getenv('WORKER_CONCURRENCY', '1')),
                       worker_prefetch_multiplier=1, task_acks_late=True,
                       task_reject_on_worker_lost=True, broker_connection_retry_on_startup=True,
                       broker_transport_options={'visibility_timeout': 86400,
                                                 'socket_connect_timeout': 5, 'socket_timeout': 5},
                       task_ignore_result=True, task_publish_retry=False)
STAGES = {'validating', 'preparing', 'analyzing', 'separating', 'transcribing',
          'scoring', 'exporting', 'finalizing'}
KINDS = {'original_preview', 'score', 'part', 'midi', 'stem', 'stem_preview', 'synth', 'synth_preview',
         'pdf', 'part_pdf', 'timeline', 'notes', 'manifest'}


class JobStopped(RuntimeError):
    pass


def beat_service(role):
    with transaction(write=True) as db:
        row = db.get(ServiceHeartbeat, role)
        if row is None:
            db.add(ServiceHeartbeat(role=role, updated_at=now()))
        else:
            row.updated_at = now()


_service_stop = threading.Event()


@signals.worker_ready.connect
def worker_ready(**kwargs):
    _service_stop.clear()
    def heartbeat():
        while not _service_stop.is_set():
            try:
                beat_service('worker')
            except Exception:
                log.exception('Worker service heartbeat failed')
            _service_stop.wait(20)
    threading.Thread(target=heartbeat, daemon=True).start()


@signals.worker_shutdown.connect
def worker_shutdown(**kwargs):
    _service_stop.set()


def claim(job_id, attempt):
    token = uid()
    current = now()
    with transaction(write=True) as db:
        result = db.execute(update(Job).where(
            Job.id == job_id, Job.attempt == attempt, Job.status == 'queued',
            Job.cancel_requested.is_(False), Job.delete_requested.is_(False),
            Job.deleted_at.is_(None)).values(
                status='running', stage='validating', stage_progress=None,
                lease_token=token, heartbeat_at=current, started_at=current,
                lease_expires_at=current + timedelta(seconds=get_config().lease_seconds)))
        if result.rowcount != 1:
            return None
    return token


def leased_job(db, job_id, attempt, token, allow_cancel=False):
    job = db.scalar(select(Job).where(Job.id == job_id).with_for_update())
    if (job is None or job.attempt != attempt or job.status != 'running'
            or job.lease_token != token or job.lease_expires_at is None
            or job.lease_expires_at <= now() or job.deleted_at is not None):
        raise JobStopped('Execution lease was lost')
    if not allow_cancel and (job.cancel_requested or job.delete_requested):
        raise JobStopped('Job cancelled')
    return job


def renew(job_id, attempt, token, stage=None, fraction=None):
    with transaction(write=True) as db:
        job = leased_job(db, job_id, attempt, token)
        job.heartbeat_at = now()
        job.lease_expires_at = now() + timedelta(seconds=get_config().lease_seconds)
        if stage is not None:
            if stage not in STAGES or (fraction is not None and
                    (not isinstance(fraction, (int, float)) or not math.isfinite(fraction)
                     or not 0 <= fraction <= 1)):
                raise ValueError('Invalid pipeline progress')
            job.stage, job.stage_progress = stage, fraction


def _prepare_result(result, work):
    if not isinstance(result, dict):
        raise ValueError('Invalid pipeline result')
    tracks = []
    seen = set()
    for source in result.get('tracks', []):
        track_id = str(uuid.UUID(source['id']))
        if track_id in seen:
            raise ValueError('Duplicate track ID')
        seen.add(track_id)
        program = source.get('program')
        confidence = source.get('confidence')
        if program is not None and (not isinstance(program, int) or not 0 <= program <= 127):
            raise ValueError('Invalid MIDI program')
        if confidence is not None and (not math.isfinite(confidence) or not 0 <= confidence <= 1):
            raise ValueError('Invalid confidence')
        if source['label_source'] not in ('model', 'separator_category', 'unknown', 'imported'):
            raise ValueError('Invalid instrument provenance')
        tracks.append(dict(id=track_id, label=str(source['label'])[:255], program=program,
                           confidence=confidence, label_source=source['label_source'],
                           is_drum=bool(source.get('is_drum', False)),
                           source_stem_ids=source.get('source_stem_ids', [])))
    artifacts = []
    for source in result.get('artifacts', []):
        if source['kind'] not in KINDS or source['status'] not in ('ready', 'failed'):
            raise ValueError('Invalid artifact kind/status')
        track_id = source.get('track_id')
        if track_id is not None and track_id not in seen and source['kind'] not in ('stem', 'stem_preview'):
            raise ValueError('Unknown artifact track')
        if track_id is not None:
            str(uuid.UUID(track_id))
        item = dict(id=uid(), track_id=track_id, kind=source['kind'],
                    label=str(source['label'])[:255], mime=source['mime'], status=source['status'],
                    error=str(source.get('error') or '该产物导出失败')[:500] if source['status'] == 'failed' else None)
        if source['status'] == 'ready':
            relative = Path(source['path'])
            path = (work / relative).resolve()
            if relative.is_absolute() or not path.is_relative_to(work.resolve()) or not path.is_file():
                raise ValueError('Pipeline artifact outside attempt directory')
            hasher = hashlib.sha256()
            with path.open('rb') as file:
                for block in iter(lambda: file.read(1024 * 1024), b''):
                    hasher.update(block)
            item.update(path=path, bytes=path.stat().st_size, sha256=hasher.hexdigest())
        artifacts.append(item)
    if not any(a['kind'] == 'score' and a['status'] == 'ready' for a in artifacts):
        raise ValueError('Missing core score artifact')
    return tracks, artifacts


def publish(job_id, attempt, token, work, result):
    tracks, artifacts = _prepare_result(result, work)
    with transaction(write=True) as db:
        job = leased_job(db, job_id, attempt, token)
        # results/<attempt> is never shared with another attempt; the fenced
        # token above is what authorizes publication, not the directory name.
        output = job_dir(job.user_id, job.id) / 'results' / str(attempt)
        output.mkdir(parents=True, mode=0o700)
        # Moves and metadata commit occur under the fenced row lock. A crash
        # leaves unreferenced files, never downloadable partially-published rows.
        clear_derived(db, job)
        for track in tracks:
            db.add(Track(job_id=job.id, **track))
        db.flush()
        for item in artifacts:
            values = dict(item)
            path = values.pop('path', None)
            if path is not None:
                # Multiple artifacts may intentionally refer to the same file.
                # Copy through a private temp, fsync, then atomically install.
                target = output / (values['id'] + path.suffix)
                temporary = output / (values['id'] + '.tmp')
                shutil.copyfile(path, temporary)
                sync_file(temporary)
                os.replace(temporary, target)
                values['storage_key'] = storage_key(target)
            db.add(Artifact(job_id=job.id, attempt=attempt, **values))
        sync_dir(output)
        # Moving large exports may exhaust a lease: never publish afterwards.
        if job.lease_expires_at <= now():
            raise JobStopped('Lease expired during publication')
        job.warnings = [str(w) for w in result.get('warnings', [])]
        job.actual_device = result.get('actual_device')
        job.engine = result.get('engine', job.engine)
        job.stage, job.stage_progress = 'finalizing', 1.0
        complete(job, 'partial' if any(a['status'] == 'failed' for a in artifacts) else 'succeeded')
        audit(db, None, 'job.completed', job.id, {'status': job.status, 'attempt': attempt})


def fail(job_id, attempt, token, exc):
    try:
        with transaction(write=True) as db:
            job = leased_job(db, job_id, attempt, token, allow_cancel=True)
            if job.cancel_requested or job.delete_requested or isinstance(exc, JobStopped):
                complete(job, 'cancelled')
            else:
                # Only PipelineError provides a public, sanitized message.
                try:
                    from pipeline.service import PipelineError
                except ImportError:
                    PipelineError = ()
                if isinstance(exc, PipelineError):
                    job.error_code, job.error_message = exc.code, exc.message
                else:
                    job.error_code, job.error_message = 'PROCESSING_FAILED', '处理失败，请联系管理员或重试'
                complete(job, 'failed')
            audit(db, None, 'job.stopped', job.id, {'status': job.status, 'attempt': attempt})
    except JobStopped:
        pass  # The dispatcher owns the new attempt; old processes cannot write.


@celery_app.task(name='app.worker.process_job')
def process_job(job_id, attempt):
    if not isinstance(job_id, str) or not isinstance(attempt, int):
        return
    token = claim(job_id, attempt)
    if token is None:
        return
    stopped, lost = threading.Event(), threading.Event()
    interval = max(1, min(20, get_config().lease_seconds / 3))
    def heartbeat():
        while not stopped.wait(interval):
            try:
                renew(job_id, attempt, token)
            except Exception:
                lost.set()
                return
    thread = threading.Thread(target=heartbeat, daemon=True)
    thread.start()
    work = None
    execution_lock = (get_config().data_dir / 'locks' / (job_id + '.lock')).open('a')
    try:
        # A stale attempt may still be shutting down. Never overlap its files.
        while True:
            try:
                fcntl.flock(execution_lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
                break
            except BlockingIOError:
                renew(job_id, attempt, token)
                if stopped.wait(.4):
                    raise JobStopped('Execution stopped')
        with transaction() as db:
            job = db.get(Job, job_id)
            original = db.scalar(select(Artifact).where(Artifact.job_id == job_id, Artifact.kind == 'original'))
            if original is None or not original.storage_key:
                raise ValueError('Original artifact missing')
            input_path = private_path(original.storage_key)
            work = job_dir(job.user_id, job.id) / 'work' / str(attempt) / token
            config, kind = dict(job.config_snapshot), job.kind
        with transaction(write=True) as db:
            leased_job(db, job_id, attempt, token)
            work.mkdir(parents=True, mode=0o700)
        def progress(stage, fraction=None):
            if lost.is_set():
                raise JobStopped('Execution stopped')
            renew(job_id, attempt, token, stage, fraction)
        from pipeline.service import process
        result = process(input_path, work, kind, config, progress)
        progress('finalizing', None)
        publish(job_id, attempt, token, work, result)
    except Exception as exc:
        if not isinstance(exc, JobStopped):
            log.exception('Job %s attempt %s failed', job_id, attempt)
        fail(job_id, attempt, token, exc)
    finally:
        stopped.set()
        thread.join(timeout=interval + 1)
        if work is not None:
            # Cleanup happens after the heartbeat stops, so a slow rmtree can
            # never be mistaken for a lost lease and republished by a peer.
            shutil.rmtree(work, ignore_errors=True)
        execution_lock.close()
