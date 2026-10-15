"""Publication and deletion boundaries with separate stem identities."""
import fcntl
import uuid
from datetime import timedelta
from sqlalchemy import select
from app.cli import init_admin
from app.config import get_config
from app.db import transaction
from app.models import Job, User, Artifact, now
from app.jobs import purge
from app.storage import job_dir
from app.worker import claim, publish


def make_job():
    uid = init_admin('lifecycle', 'long-password-for-tests')
    with transaction(write=True) as db:
        job = Job(user_id=uid, title='fixture', kind='audio', engine='baseline', config_snapshot={'retention_days': 7})
        db.add(job)
        db.flush()
        return uid, job.id


def test_stem_id_is_independent_from_score_track():
    user, jid = make_job()
    token = claim(jid, 1)
    work = job_dir(user, jid)/'work'/'1'/token
    work.mkdir(parents=True)
    (work/'score.xml').write_text('<score-partwise/>')
    (work/'stem.wav').write_bytes(b'fixture')
    stem, track = str(uuid.uuid4()), str(uuid.uuid4())
    result = {'tracks': [{'id': track, 'label': '未知', 'program': None, 'confidence': None,
                         'label_source': 'unknown', 'is_drum': False, 'source_stem_ids': [stem]}],
              'artifacts': [{'kind': 'score', 'label': '谱', 'path': 'score.xml', 'mime': 'application/xml', 'status': 'ready'},
                            {'kind': 'stem', 'track_id': stem, 'label': '混合音轨', 'path': 'stem.wav', 'mime': 'audio/wav', 'status': 'ready'},
                            {'kind': 'pdf', 'label': 'PDF', 'mime': 'application/pdf', 'status': 'failed', 'error': '本地工具不可用'}]}
    publish(jid, 1, token, work, result)
    with transaction() as db:
        job = db.get(Job, jid)
        assert job.status == 'partial'
        assert timedelta(days=7) == job.expires_at-job.completed_at
        assert db.scalar(select(Artifact).where(Artifact.kind == 'stem')).track_id == stem
        assert db.scalar(select(Artifact).where(Artifact.kind == 'pdf')).error == '本地工具不可用'


def test_purge_waits_for_executing_process_lock():
    user, jid = make_job()
    directory = job_dir(user, jid)
    directory.mkdir(parents=True)
    (directory/'input.wav').write_bytes(b'fixture')
    lock = (get_config().data_dir/'locks'/(jid+'.lock')).open('a')
    fcntl.flock(lock, fcntl.LOCK_EX)
    with transaction(write=True) as db:
        job = db.get(Job, jid)
        job.status = 'cancelled'
        job.delete_requested = True
        assert purge(db, job) is False
        assert directory.exists() and job.deleted_at is None
    lock.close()
    with transaction(write=True) as db:
        assert purge(db, db.get(Job, jid)) is True
    assert not directory.exists()
