from pydantic import BaseModel, ConfigDict, Field
from sqlalchemy import select
from .models import Artifact, Track, User


class Credentials(BaseModel):
    model_config = ConfigDict(extra='forbid')
    username: str = Field(min_length=1, max_length=128)
    password: str = Field(min_length=1, max_length=128)


class SettingsPatch(BaseModel):
    model_config = ConfigDict(extra='forbid', strict=True)
    max_audio_duration_seconds: int = Field(default=600, ge=1, le=86400)
    max_unfinished_jobs_per_user: int = Field(default=3, ge=1, le=100)
    retention_days: int | None = Field(default=None, ge=1, le=36500)
    max_upload_bytes: int = Field(default=209715200, ge=1024, le=2147483647)
    default_engine: str = Field(default='baseline', pattern='^baseline$')


def user_json(user):
    return {key: getattr(user, key) for key in ('id', 'username', 'role', 'status')}


def track_json(track):
    return {key: getattr(track, key) for key in (
        'id', 'label', 'program', 'confidence', 'label_source', 'is_drum', 'source_stem_ids')}


def artifact_json(artifact):
    return {key: getattr(artifact, key) for key in (
        'id', 'track_id', 'kind', 'label', 'mime', 'bytes', 'status', 'error')}


def job_json(db, job):
    result = {key: getattr(job, key) for key in (
        'id', 'user_id', 'title', 'kind', 'status', 'stage', 'stage_progress',
        'engine', 'actual_device', 'config_snapshot', 'warnings', 'error_code',
        'error_message', 'attempt', 'created_at', 'started_at', 'completed_at', 'expires_at')}
    owner = db.get(User, job.user_id)
    result['username'] = owner.username if owner else None
    result['tracks'] = [track_json(t) for t in db.scalars(select(Track).where(Track.job_id == job.id))]
    result['artifacts'] = [artifact_json(a) for a in db.scalars(select(Artifact).where(Artifact.job_id == job.id))]
    return result
