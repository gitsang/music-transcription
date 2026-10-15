"""API, authorization, worker-fencing and dispatcher recovery tests.

Uses the isolated SQLite database and temporary data root from conftest.py.
Pipeline is stubbed: these tests never import torch or run real inference.
"""
from datetime import timedelta
import sys
import types
import uuid

import pytest
from sqlalchemy import select

from app.cli import init_admin
from app.db import transaction
from app.models import Artifact, Job, QueueOutbox, Track, User, now
from app.storage import job_dir, private_path

PASSWORD = 'correct-horse-battery'
XML = (b'<?xml version="1.0" encoding="UTF-8"?>\n'
       b'<!DOCTYPE score-partwise PUBLIC "-//Recordare//DTD MusicXML 3.1 Partwise//EN" '
       b'"http://www.musicxml.org/dtds/partwise.dtd">\n'
       b'<score-partwise version="3.1"><part-list><score-part id="P1">'
       b'<part-name>Piano</part-name></score-part></part-list>'
       b'<part id="P1"><measure number="1"/></part></score-partwise>')


def make_user(username, role='user', status='active'):
    from app.security import PASSWORDS
    with transaction(write=True) as db:
        user = User(username=username, password_hash=PASSWORDS.hash(PASSWORD),
                    role=role, status=status)
        db.add(user)
        db.flush()
        return user.id


def login(client, username):
    response = client.post('/api/v1/auth/login', json={'username': username, 'password': PASSWORD})
    assert response.status_code == 200, response.text
    return response.json()


def upload(client, kind='musicxml', filename='song.musicxml', content=XML):
    return client.post('/api/v1/jobs', data={'kind': kind},
                       files={'file': (filename, content, 'application/xml')})


# --------------------------------------------------------------------------- auth

def test_register_login_and_admin_approval(client):
    created = client.post('/api/v1/auth/register',
                          json={'username': 'Alice', 'password': PASSWORD})
    assert created.status_code == 201
    assert created.json() == {'status': 'pending'}
    # Username is normalized (casefold) and a pending account cannot log in.
    blocked = client.post('/api/v1/auth/login', json={'username': 'alice', 'password': PASSWORD})
    assert blocked.status_code == 403
    assert blocked.json()['error']['code'] == 'ACCOUNT_PENDING'

    admin_id = init_admin('root', PASSWORD)
    login(client, 'root')
    with transaction() as db:
        alice_id = db.scalar(select(User.id).where(User.username == 'alice'))
        assert alice_id and admin_id
    approved = client.post(f'/api/v1/admin/users/{alice_id}/approve')
    assert approved.status_code == 200 and approved.json()['status'] == 'active'
    client.post('/api/v1/auth/logout')
    me = login(client, 'alice')
    assert me['role'] == 'user'
    assert client.get('/api/v1/auth/me').json()['username'] == 'alice'


def test_duplicate_registration_and_bad_origin(client):
    body = {'username': 'bob', 'password': PASSWORD}
    assert client.post('/api/v1/auth/register', json=body).status_code == 201
    assert client.post('/api/v1/auth/register', json=body).status_code == 409
    assert client.post('/api/v1/auth/register',
                       json={'username': 'x', 'password': PASSWORD}).status_code == 400
    make_user('carol')
    forbidden = client.post('/api/v1/auth/login', json={'username': 'carol', 'password': PASSWORD},
                            headers={'Origin': 'http://evil.example'})
    assert forbidden.status_code == 403
    assert forbidden.json()['error']['code'] == 'ORIGIN_FORBIDDEN'


# --------------------------------------------------------------------------- jobs

def test_capabilities_shape(client):
    body = client.get('/api/v1/capabilities').json()
    assert isinstance(body['engines'], list) and body['engines']
    engine = body['engines'][0]
    assert set(engine) == {'id', 'label', 'ready', 'limitations'}
    assert isinstance(engine['ready'], bool)
    for key in ('max_upload_bytes', 'max_audio_duration_seconds', 'max_unfinished_jobs_per_user'):
        assert isinstance(body[key], int)


def test_upload_job_creates_outbox_and_original_artifact(client):
    make_user('dave')
    login(client, 'dave')
    response = upload(client)
    assert response.status_code == 202, response.text
    job = response.json()
    assert job['status'] == 'queued' and job['attempt'] == 1 and job['kind'] == 'musicxml'
    originals = [a for a in job['artifacts'] if a['kind'] == 'original']
    assert len(originals) == 1 and originals[0]['status'] == 'ready'
    assert originals[0]['bytes'] == len(XML)
    with transaction() as db:
        outbox = db.scalars(select(QueueOutbox).where(QueueOutbox.job_id == job['id'])).all()
        assert len(outbox) == 1 and outbox[0].attempt == 1 and outbox[0].delivered_at is None
    listed = client.get('/api/v1/jobs').json()
    assert [j['id'] for j in listed] == [job['id']]


def test_upload_rejects_bad_kind_and_empty_file(client):
    make_user('erin')
    login(client, 'erin')
    assert upload(client, filename='song.txt').status_code == 400
    assert upload(client, kind='audio', filename='song.musicxml').status_code == 400
    assert upload(client, content=b'').status_code == 400


def test_quota_blocks_concurrent_unfinished_jobs(client):
    make_user('frank')
    login(client, 'frank')
    assert upload(client).status_code == 202
    assert upload(client).status_code == 202
    assert upload(client).status_code == 202
    limited = upload(client)
    assert limited.status_code == 409
    assert limited.json()['error']['code'] == 'JOB_LIMIT_REACHED'


def test_cross_user_access_hides_existence(client):
    first = make_user('grace')
    login(client, 'grace')
    job = upload(client).json()
    artifact = next(a for a in job['artifacts'] if a['kind'] == 'original')
    client.post('/api/v1/auth/logout')
    make_user('heidi')
    login(client, 'heidi')
    assert client.get(f'/api/v1/jobs/{job["id"]}').status_code == 404
    assert client.get(f'/api/v1/jobs/{job["id"]}/tracks').status_code == 404
    assert client.get(f'/api/v1/artifacts/{artifact["id"]}/content').status_code == 404
    assert first


def test_admin_can_read_any_job(client):
    make_user('ivan')
    login(client, 'ivan')
    job = upload(client).json()
    client.post('/api/v1/auth/logout')
    init_admin('root', PASSWORD)
    login(client, 'root')
    assert client.get(f'/api/v1/jobs/{job["id"]}').status_code == 200
    assert any(j['id'] == job['id'] for j in client.get('/api/v1/admin/jobs').json())


def test_artifact_range_and_download(client):
    make_user('judy')
    login(client, 'judy')
    job = upload(client).json()
    artifact = next(a for a in job['artifacts'] if a['kind'] == 'original')

    full = client.get(f'/api/v1/artifacts/{artifact["id"]}/content')
    assert full.status_code == 200 and full.content == XML
    assert full.headers['accept-ranges'] == 'bytes'

    partial = client.get(f'/api/v1/artifacts/{artifact["id"]}/content',
                         headers={'Range': 'bytes=0-8'})
    assert partial.status_code == 206
    assert partial.content == XML[:9]
    assert partial.headers['content-range'] == f'bytes 0-8/{len(XML)}'

    assert client.get(f'/api/v1/artifacts/{artifact["id"]}/content',
                      headers={'Range': 'bytes=999999-'}).status_code == 416
    download = client.get(f'/api/v1/artifacts/{artifact["id"]}/download')
    assert download.status_code == 200
    assert download.headers['content-disposition'].startswith('attachment')


def test_queued_cancel_and_delete_remove_files(client):
    user_id = make_user('karl')
    login(client, 'karl')
    job = upload(client).json()
    directory = job_dir(user_id, job['id'])
    assert directory.exists()

    cancelled = client.post(f'/api/v1/jobs/{job["id"]}/cancel')
    assert cancelled.status_code == 200 and cancelled.json()['status'] == 'cancelled'
    # Cancelled jobs stay visible until deleted; only deleted rows disappear.
    assert [j['status'] for j in client.get('/api/v1/jobs').json()] == ['cancelled']
    # A cancelled job is retryable and rotating the attempt resets the outbox.
    retried = client.post(f'/api/v1/jobs/{job["id"]}/retry')
    assert retried.status_code == 202 and retried.json()['attempt'] == 2
    with transaction() as db:
        attempts = {row.attempt for row in db.scalars(
            select(QueueOutbox).where(QueueOutbox.job_id == job['id']))}
    assert attempts == {1, 2}

    deleted = client.delete(f'/api/v1/jobs/{job["id"]}')
    assert deleted.status_code == 202 and deleted.json()['status'] == 'deleted'
    assert not directory.exists()
    assert client.get('/api/v1/jobs').json() == []
    with transaction() as db:
        assert db.get(Job, job['id']).deleted_at is not None
        assert db.scalars(select(Artifact).where(Artifact.job_id == job['id'])).all() == []


def test_admin_settings_snapshot_and_validation(client):
    init_admin('root', PASSWORD)
    login(client, 'root')
    assert client.get('/api/v1/admin/settings').json()['default_engine'] == 'baseline'
    updated = client.patch('/api/v1/admin/settings',
                           json={'max_unfinished_jobs_per_user': 1, 'retention_days': 7})
    assert updated.status_code == 200
    assert updated.json()['max_unfinished_jobs_per_user'] == 1
    assert client.patch('/api/v1/admin/settings',
                        json={'max_unfinished_jobs_per_user': 0}).status_code == 422
    make_user('liam')
    login(client, 'liam')
    job = upload(client).json()
    assert job['config_snapshot']['max_unfinished_jobs_per_user'] == 1
    assert job['config_snapshot']['retention_days'] == 7
    assert upload(client).status_code == 409


def test_upload_size_limit_uses_admin_setting(client):
    init_admin('root', PASSWORD)
    login(client, 'root')
    client.patch('/api/v1/admin/settings', json={'max_upload_bytes': 1024})
    make_user('mia')
    login(client, 'mia')
    assert upload(client, content=b'x' * 2048).status_code == 413


# ----------------------------------------------------------------- worker fencing

def queued_job(user_id, config=None):
    with transaction(write=True) as db:
        job = Job(user_id=user_id, title='t', kind='musicxml', engine='baseline',
                  config_snapshot=config or {'engine': 'baseline'})
        db.add(job)
        db.flush()
        db.add(QueueOutbox(job_id=job.id, attempt=job.attempt))
        return job.id


def pipeline_result(work):
    work.mkdir(parents=True, exist_ok=True)
    (work / 'score.xml').write_bytes(XML)
    track_id = str(uuid.uuid4())
    return {'tracks': [{'id': track_id, 'label': 'Piano', 'program': 0, 'confidence': 0.9,
                        'label_source': 'model', 'is_drum': False, 'source_stem_ids': []}],
            'artifacts': [{'track_id': None, 'kind': 'score', 'label': '总谱',
                           'path': 'score.xml', 'mime': 'application/vnd.recordare.musicxml+xml',
                           'status': 'ready'}],
            'warnings': ['incomplete'], 'actual_device': 'cpu', 'engine': 'baseline'}


def test_claim_is_exclusive_and_publish_is_fenced():
    from app import worker
    user_id = make_user('nina')
    job_id = queued_job(user_id)
    token = worker.claim(job_id, 1)
    assert token is not None
    # A duplicate delivery of the same attempt must not steal the lease.
    assert worker.claim(job_id, 1) is None
    worker.renew(job_id, 1, token, 'separating', 0.5)

    with transaction() as db:
        assert db.get(Job, job_id).stage == 'separating'

    work = job_dir(user_id, job_id) / 'work' / '1' / token
    worker.publish(job_id, 1, token, work, pipeline_result(work))
    with transaction() as db:
        job = db.get(Job, job_id)
        assert job.status == 'succeeded' and job.lease_token is None
        assert job.actual_device == 'cpu' and job.warnings == ['incomplete']
        tracks = db.scalars(select(Track).where(Track.job_id == job_id)).all()
        assert len(tracks) == 1
        artifacts = db.scalars(select(Artifact).where(Artifact.job_id == job_id,
                              Artifact.kind == 'score')).all()
        assert len(artifacts) == 1 and artifacts[0].sha256
        stored = artifacts[0].storage_key
    assert private_path(stored).is_file()
    assert stored.startswith(f'users/{user_id}/jobs/{job_id}/results/1/')

    # A stale process holding the old token must not write back.
    with pytest.raises(worker.JobStopped):
        worker.publish(job_id, 1, token, work, pipeline_result(work))


def test_cancelled_running_job_is_not_published():
    from app import worker
    user_id = make_user('omar')
    job_id = queued_job(user_id)
    token = worker.claim(job_id, 1)
    with transaction(write=True) as db:
        db.get(Job, job_id).cancel_requested = True
    with pytest.raises(worker.JobStopped):
        worker.renew(job_id, 1, token)
    work = job_dir(user_id, job_id) / 'work' / '1' / token
    worker.fail(job_id, 1, token, worker.JobStopped('cancelled'))
    with transaction() as db:
        assert db.get(Job, job_id).status == 'cancelled'


def install_pipeline(monkeypatch, fn):
    """Inject a stand-in pipeline.service so the worker runs without torch."""
    module = types.ModuleType('pipeline.service')
    module.process = fn

    class PipelineError(Exception):
        def __init__(self, code, message):
            super().__init__(message)
            self.code, self.message = code, message
    module.PipelineError = PipelineError
    monkeypatch.setitem(sys.modules, 'pipeline.service', module)
    return PipelineError


def test_process_job_publishes_and_cleans_work(client, monkeypatch):
    from app import worker
    user_id = make_user('tina')
    login(client, 'tina')
    uploaded = upload(client).json()

    def process(input_path, work, kind, config, progress):
        assert input_path.is_file()
        progress('analyzing', 0.25)
        return pipeline_result(work)

    install_pipeline(monkeypatch, process)
    worker.process_job(uploaded['id'], 1)
    with transaction() as db:
        job = db.get(Job, uploaded['id'])
        assert job.status == 'succeeded' and job.lease_token is None
        score = db.scalars(select(Artifact).where(Artifact.job_id == uploaded['id'],
                           Artifact.kind == 'score')).all()
        assert len(score) == 1 and score[0].status == 'ready'
    # The per-attempt work directory is removed after the worker finishes.
    work_root = job_dir(user_id, uploaded['id']) / 'work'
    assert not any(work_root.glob('*/*')), list(work_root.glob('*/*'))


def test_process_job_cooperative_cancel_discards_work(client, monkeypatch):
    from app import worker
    user_id = make_user('uma')
    login(client, 'uma')
    uploaded = upload(client).json()

    def process(input_path, work, kind, config, progress):
        # A user cancel mid-run is only visible on the next progress() call,
        # which must raise and stop the pipeline before publication.
        with transaction(write=True) as db:
            db.get(Job, uploaded['id']).cancel_requested = True
        progress('analyzing', 0.5)
        raise AssertionError('progress should have raised')

    install_pipeline(monkeypatch, process)
    worker.process_job(uploaded['id'], 1)
    with transaction() as db:
        job = db.get(Job, uploaded['id'])
        assert job.status == 'cancelled'
        assert db.scalars(select(Artifact).where(Artifact.job_id == uploaded['id'],
                          Artifact.kind == 'score')).all() == []
    work_root = job_dir(user_id, uploaded['id']) / 'work'
    assert not any(work_root.glob('*/*'))


# --------------------------------------------------------------- dispatcher

def test_recover_expired_lease_requeues_next_attempt():
    from app.dispatcher import recover_leases
    user_id = make_user('pam')
    job_id = queued_job(user_id)
    with transaction(write=True) as db:
        job = db.get(Job, job_id)
        job.status = 'running'
        job.lease_token = str(uuid.uuid4())
        job.lease_expires_at = now() - timedelta(seconds=5)
    recover_leases()
    with transaction() as db:
        job = db.get(Job, job_id)
        assert job.status == 'queued' and job.attempt == 2 and job.lease_token is None
        attempts = {row.attempt for row in db.scalars(
            select(QueueOutbox).where(QueueOutbox.job_id == job_id))}
        assert attempts == {1, 2}
    # The fenced old worker must not be able to renew or publish attempt 1.
    from app import worker
    stale = str(uuid.uuid4())
    with pytest.raises(worker.JobStopped):
        worker.renew(job_id, 1, stale)
    # fail() intentionally swallows a lost lease: the dispatcher owns the new
    # attempt, so a crashed old worker must not overwrite its state.
    worker.fail(job_id, 1, stale, RuntimeError('crashed'))
    with transaction() as db:
        assert db.get(Job, job_id).status == 'queued'


def test_recover_stops_after_max_attempts():
    from app.dispatcher import MAX_RECOVERY_ATTEMPTS, recover_leases
    user_id = make_user('quinn')
    job_id = queued_job(user_id)
    with transaction(write=True) as db:
        job = db.get(Job, job_id)
        job.status = 'running'
        job.attempt = MAX_RECOVERY_ATTEMPTS
        job.lease_token = str(uuid.uuid4())
        job.lease_expires_at = now() - timedelta(seconds=5)
    recover_leases()
    with transaction() as db:
        job = db.get(Job, job_id)
        assert job.status == 'failed' and job.error_code == 'WORKER_LOST'


def test_outbox_retry_then_success():
    from app.dispatcher import dispatch_outbox
    user_id = make_user('rita')
    job_id = queued_job(user_id)
    sent = []

    def failing(job, attempt):
        sent.append((job, attempt))
        raise RuntimeError('broker down')

    assert dispatch_outbox(failing) == 0
    with transaction() as db:
        row = db.scalar(select(QueueOutbox).where(QueueOutbox.job_id == job_id))
        assert row.tries == 1 and row.delivered_at is None
        # Backoff must postpone the next attempt beyond "now".
        assert row.next_retry_at > now()
    sent.clear()
    assert dispatch_outbox(lambda job, attempt: sent.append((job, attempt))) == 0
    with transaction(write=True) as db:
        row = db.scalar(select(QueueOutbox).where(QueueOutbox.job_id == job_id))
        row.next_retry_at = now() - timedelta(seconds=1)
    assert dispatch_outbox(lambda job, attempt: sent.append((job, attempt))) == 1
    assert sent == [(job_id, 1)]
    with transaction() as db:
        row = db.scalar(select(QueueOutbox).where(QueueOutbox.job_id == job_id))
        assert row.delivered_at is not None


def test_cleanup_removes_deleted_job_directories():
    from app.dispatcher import cleanup
    user_id = make_user('sam')
    job_id = queued_job(user_id)
    directory = job_dir(user_id, job_id)
    directory.mkdir(parents=True, exist_ok=True)
    (directory / 'input').mkdir()
    (directory / 'input' / 'original.xml').write_bytes(XML)
    with transaction(write=True) as db:
        job = db.get(Job, job_id)
        job.status = 'cancelled'
        job.deleted_at = now()
        job.delete_requested = True
    cleanup()
    assert not directory.exists()
