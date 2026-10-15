from contextlib import asynccontextmanager
from datetime import timedelta
import hashlib
import logging
import os
from pathlib import Path
import secrets
import shutil
from urllib.parse import quote

from fastapi import FastAPI, Request, Response, Depends, UploadFile, File, Form
from fastapi.exceptions import RequestValidationError
from fastapi.responses import JSONResponse, StreamingResponse
from sqlalchemy import select, update
from sqlalchemy.exc import IntegrityError
from starlette.exceptions import HTTPException

from .config import get_config
from .db import transaction, settings, lock_user
from .errors import ApiError
from .jobs import (authorized_job, check_quota, complete, clear_derived, purge,
                   safe_title, extension)
from .models import User, Session, Job, Artifact, Track, Setting, QueueOutbox, now, uid
from .readiness import capabilities, health
from .schemas import (Credentials, SettingsPatch, user_json, job_json, track_json, artifact_json)
from .security import (current_user, admin_user, normalize_username, validate_password,
                       PASSWORDS, DUMMY_HASH, verify, digest, rate_limit, COOKIE, audit, revoke_sessions)
from .storage import (initialize_storage, ensure_space, job_dir, private_path,
                      storage_key, sync_file, sync_dir)

log = logging.getLogger(__name__)


def error_response(code, message, status):
    return JSONResponse({'error': {'code': code, 'message': message}}, status_code=status)


class BodyTooLarge(Exception):
    pass


class SecurityMiddleware:
    """Count body bytes before multipart spooling, including chunked uploads."""
    def __init__(self, app):
        self.app = app

    async def __call__(self, scope, receive, send):
        if scope['type'] != 'http':
            return await self.app(scope, receive, send)
        headers = dict(scope['headers'])
        if scope['method'] in ('POST', 'PUT', 'PATCH', 'DELETE'):
            origin = headers.get(b'origin')
            if ((origin is not None and origin.decode('latin1') != get_config().public_origin)
                    or headers.get(b'sec-fetch-site') == b'cross-site'):
                return await error_response('ORIGIN_FORBIDDEN', '拒绝跨站写入请求', 403)(scope, receive, send)
        maximum = 16384
        if scope['path'] == '/api/v1/jobs' and scope['method'] == 'POST':
            with transaction() as db:
                maximum = settings(db)['max_upload_bytes'] + 1024 * 1024
        count = 0

        async def limited_receive():
            nonlocal count
            event = await receive()
            if event['type'] == 'http.request':
                count += len(event.get('body', b''))
                if count > maximum:
                    raise BodyTooLarge()
            return event

        try:
            length = int(headers.get(b'content-length', b'0'))
        except ValueError:
            length = maximum + 1
        if length > maximum:
            return await error_response('UPLOAD_TOO_LARGE', '请求体超过大小限制', 413)(scope, receive, send)
        await self.app(scope, limited_receive, send)


@asynccontextmanager
async def lifespan(app):
    initialize_storage()
    yield


app = FastAPI(title='私有扒谱服务', lifespan=lifespan)
app.add_middleware(SecurityMiddleware)
PREFIX = '/api/v1'


@app.exception_handler(ApiError)
async def api_error(_, exc):
    return error_response(exc.code, exc.message, exc.status)


@app.exception_handler(BodyTooLarge)
async def body_error(_, exc):
    return error_response('UPLOAD_TOO_LARGE', '请求体超过大小限制', 413)


@app.exception_handler(RequestValidationError)
async def validation_error(_, exc):
    return error_response('INVALID_REQUEST', '请求参数格式错误', 422)


@app.exception_handler(HTTPException)
async def http_error(_, exc):
    # Starlette turns multipart read errors into HTTPException(400).
    if isinstance(exc.__cause__, BodyTooLarge):
        return error_response('UPLOAD_TOO_LARGE', '请求体超过大小限制', 413)
    return error_response('NOT_FOUND' if exc.status_code == 404 else 'INVALID_REQUEST',
                          '资源不存在' if exc.status_code == 404 else '请求无法处理', exc.status_code)


@app.exception_handler(Exception)
async def unexpected_error(_, exc):
    log.exception('API request failed')
    return error_response('INTERNAL_ERROR', '服务暂时不可用', 500)


@app.post(PREFIX + '/auth/register', status_code=201)
def register(body: Credentials, request: Request):
    name = normalize_username(body.username)
    rate_limit(request, 'register')
    validate_password(body.password)
    password_hash = PASSWORDS.hash(body.password)
    try:
        with transaction(write=True) as db:
            user = User(username=name, password_hash=password_hash)
            db.add(user)
            db.flush()
            audit(db, user.id, 'account.register', user.id)
    except IntegrityError:
        raise ApiError('USERNAME_TAKEN', '用户名已被注册', 409)
    return {'status': 'pending'}


@app.post(PREFIX + '/auth/login')
def login(body: Credentials, request: Request, response: Response):
    name = normalize_username(body.username)
    rate_limit(request, 'login', name)
    with transaction() as db:
        user = db.scalar(select(User).where(User.username == name))
        valid = verify(body.password, user.password_hash if user else DUMMY_HASH)
    if not valid or user is None:
        raise ApiError('INVALID_CREDENTIALS', '用户名或密码错误', 401)
    with transaction(write=True) as db:
        user = lock_user(db, user.id)
        if user.status != 'active':
            codes = {'pending': 'ACCOUNT_PENDING', 'rejected': 'ACCOUNT_REJECTED', 'disabled': 'ACCOUNT_DISABLED'}
            raise ApiError(codes.get(user.status, 'ACCOUNT_DISABLED'), '账号尚未获准登录', 403)
        token = secrets.token_urlsafe(48)
        db.add(Session(token_hash=digest(token), user_id=user.id,
                       expires_at=now() + timedelta(seconds=get_config().session_seconds)))
        audit(db, user.id, 'account.login', user.id)
        result = user_json(user)
    response.set_cookie(COOKIE, token, httponly=True, secure=get_config().cookie_secure,
                        samesite='lax', max_age=get_config().session_seconds, path='/')
    return result


@app.get(PREFIX + '/auth/me')
def me(user: User = Depends(current_user)):
    return user_json(user)


@app.post(PREFIX + '/auth/logout')
def logout(request: Request, response: Response):
    with transaction(write=True) as db:
        db.execute(update(Session).where(Session.token_hash == digest(request.cookies.get(COOKIE, '')),
                                         Session.revoked_at.is_(None)).values(revoked_at=now()))
    response.delete_cookie(COOKIE, path='/', secure=get_config().cookie_secure, httponly=True, samesite='lax')
    return {'status': 'logged_out'}


@app.get(PREFIX + '/capabilities')
def get_capabilities():
    with transaction() as db:
        config = settings(db)
    return {'engines': capabilities(), **{k: config[k] for k in (
        'max_upload_bytes', 'max_audio_duration_seconds', 'max_unfinished_jobs_per_user')}}


@app.post(PREFIX + '/jobs', status_code=202)
async def upload_job(file: UploadFile = File(...), kind: str = Form(...),
                     engine: str | None = Form(None), user: User = Depends(current_user)):
    ensure_space()
    title = safe_title(file.filename)
    suffix = extension(kind, title)
    with transaction() as db:
        snapshot = settings(db)
    selected = engine or snapshot['default_engine']
    if selected != 'baseline':
        raise ApiError('ENGINE_NOT_AVAILABLE', '该引擎尚未验证可用')
    if kind == 'audio' and not any(c['id'] == selected and c['ready'] for c in capabilities()):
        raise ApiError('MODEL_NOT_READY', '本地模型未准备就绪，仍可导入MusicXML', 503)
    snapshot['engine'] = selected
    snapshot['stage_timeout_seconds'] = get_config().stage_timeout_seconds
    temp = get_config().data_dir / 'uploads' / (uid() + '.upload')
    directory = None
    committed = False
    size, hasher = 0, hashlib.sha256()
    try:
        with temp.open('xb') as output:
            os.chmod(temp, 0o600)
            while chunk := await file.read(1024 * 1024):
                size += len(chunk)
                if size > snapshot['max_upload_bytes']:
                    raise ApiError('UPLOAD_TOO_LARGE', '文件超过大小限制', 413)
                output.write(chunk)
                hasher.update(chunk)
            output.flush()
            os.fsync(output.fileno())
        if size == 0:
            raise ApiError('INVALID_INPUT', '上传文件为空')
        job_id = uid()
        directory = job_dir(user.id, job_id)
        # No network await while the user row is locked. DB + outbox commit
        # happens only after the durable private input file has been installed.
        with transaction(write=True) as db:
            check_quota(db, user.id, snapshot)
            job = Job(id=job_id, user_id=user.id, title=title, kind=kind,
                      engine=selected, config_snapshot=snapshot)
            db.add(job)
            db.flush()
            input_dir = directory / 'input'
            input_dir.mkdir(parents=True, mode=0o700)
            target = input_dir / ('original' + suffix)
            os.replace(temp, target)
            sync_dir(input_dir)
            mime = {'.xml': 'application/vnd.recordare.musicxml+xml',
                    '.musicxml': 'application/vnd.recordare.musicxml+xml',
                    '.mxl': 'application/vnd.recordare.musicxml'} .get(suffix)
            if mime is None:
                import mimetypes
                mime = mimetypes.guess_type(target.name)[0] or 'application/octet-stream'
            db.add(Artifact(job_id=job.id, kind='original', label=title, storage_key=storage_key(target),
                            mime=mime, bytes=size, sha256=hasher.hexdigest(), status='ready'))
            db.add(QueueOutbox(job_id=job.id, attempt=job.attempt))
            audit(db, user.id, 'job.create', job.id, {'kind': kind})
            db.flush()
            result = job_json(db, job)
        committed = True
        return result
    finally:
        await file.close()
        temp.unlink(missing_ok=True)
        if directory is not None and not committed:
            shutil.rmtree(directory, ignore_errors=True)


@app.get(PREFIX + '/jobs')
def list_jobs(user: User = Depends(current_user)):
    with transaction() as db:
        jobs = db.scalars(select(Job).where(Job.user_id == user.id, Job.deleted_at.is_(None),
                            Job.delete_requested.is_(False)).order_by(Job.created_at.desc()).limit(200))
        return [job_json(db, job) for job in jobs]


@app.get(PREFIX + '/jobs/{job_id}')
def get_job(job_id: str, user: User = Depends(current_user)):
    with transaction(write=user.role == 'admin') as db:
        job = authorized_job(db, job_id, user)
        if user.role == 'admin':
            audit(db, user.id, 'admin.job.read', job.id)
        return job_json(db, job)


@app.get(PREFIX + '/jobs/{job_id}/tracks')
def get_tracks(job_id: str, user: User = Depends(current_user)):
    with transaction() as db:
        job = authorized_job(db, job_id, user)
        return [track_json(t) for t in db.scalars(select(Track).where(Track.job_id == job.id))]


@app.get(PREFIX + '/jobs/{job_id}/artifacts')
def get_artifacts(job_id: str, user: User = Depends(current_user)):
    with transaction() as db:
        job = authorized_job(db, job_id, user)
        return [artifact_json(a) for a in db.scalars(select(Artifact).where(Artifact.job_id == job.id))]


@app.post(PREFIX + '/jobs/{job_id}/cancel')
def cancel_job(job_id: str, user: User = Depends(current_user)):
    with transaction(write=True) as db:
        job = authorized_job(db, job_id, user, lock=True)
        if job.status == 'queued':
            job.cancel_requested = True
            complete(job, 'cancelled')
        elif job.status == 'running':
            job.cancel_requested = True
        audit(db, user.id, 'job.cancel', job.id)
        return job_json(db, job)


@app.post(PREFIX + '/jobs/{job_id}/retry', status_code=202)
def retry_job(job_id: str, user: User = Depends(current_user)):
    ensure_space()
    with transaction(write=True) as db:
        initial = authorized_job(db, job_id, user)
        snapshot = dict(initial.config_snapshot)
        check_quota(db, initial.user_id, settings(db))
        job = authorized_job(db, job_id, user, lock=True)
        if job.status not in ('failed', 'partial', 'cancelled') or job.lease_token:
            raise ApiError('JOB_NOT_RETRYABLE', '任务当前不可重试', 409)
        if job.kind == 'audio' and not any(c['id'] == job.engine and c['ready'] for c in capabilities()):
            raise ApiError('MODEL_NOT_READY', '本地模型未准备就绪', 503)
        original = db.scalar(select(Artifact).where(Artifact.job_id == job.id, Artifact.kind == 'original'))
        if not original or not private_path(original.storage_key).is_file():
            raise ApiError('INPUT_NOT_AVAILABLE', '原始文件已不存在', 409)
        clear_derived(db, job)
        job.attempt += 1
        job.status, job.stage, job.stage_progress = 'queued', None, None
        job.cancel_requested = False
        job.warnings = []
        job.error_code, job.error_message = None, None
        job.started_at, job.completed_at, job.expires_at = None, None, None
        job.actual_device = None
        db.add(QueueOutbox(job_id=job.id, attempt=job.attempt))
        audit(db, user.id, 'job.retry', job.id, {'attempt': job.attempt})
        db.flush()
        return job_json(db, job)


@app.delete(PREFIX + '/jobs/{job_id}', status_code=202)
def delete_job(job_id: str, user: User = Depends(current_user)):
    with transaction(write=True) as db:
        job = authorized_job(db, job_id, user, lock=True)
        job.delete_requested = True
        job.cancel_requested = True
        if job.status == 'queued':
            complete(job, 'cancelled')
        if job.status != 'running':
            purge(db, job)
        audit(db, user.id, 'job.delete', job.id)
    return {'status': 'deleted' if job.deleted_at else 'deleting'}


def range_bounds(value, size):
    if not value:
        return 0, size - 1, False
    import re
    match = re.fullmatch(r'bytes=(\d*)-(\d*)', value)
    if not match or not any(match.groups()) or size == 0:
        raise ApiError('INVALID_RANGE', '无效字节范围', 416)
    first, last = match.groups()
    if first:
        start, end = int(first), min(int(last) if last else size - 1, size - 1)
    else:
        suffix = int(last)
        start, end = max(0, size - suffix), size - 1
    if start >= size or start > end or (not first and int(last) == 0):
        raise ApiError('INVALID_RANGE', '无效字节范围', 416)
    return start, end, True


def file_response(artifact_id, request, user, download):
    with transaction(write=user.role == 'admin') as db:
        artifact = db.get(Artifact, artifact_id)
        if artifact is None:
            raise ApiError('NOT_FOUND', '文件不存在', 404)
        job = authorized_job(db, artifact.job_id, user)
        if artifact.status != 'ready' or not artifact.storage_key:
            raise ApiError('ARTIFACT_NOT_AVAILABLE', '该产物未能生成', 409)
        try:
            path = private_path(artifact.storage_key)
            file = path.open('rb')
        except (OSError, ValueError):
            raise ApiError('ARTIFACT_NOT_AVAILABLE', '文件暂不可用', 404)
        if user.role == 'admin':
            audit(db, user.id, 'admin.artifact.download' if download else 'admin.artifact.read', artifact.id,
                  {'job_id': job.id})
    size = os.fstat(file.fileno()).st_size
    try:
        start, end, partial = range_bounds(request.headers.get('range'), size)
    except ApiError as exc:
        file.close()
        response = error_response(exc.code, exc.message, exc.status)
        response.headers['Content-Range'] = f'bytes */{size}'
        return response
    filename = safe_title(artifact.label)
    if artifact.kind != 'original' and path.suffix and not filename.lower().endswith(path.suffix.lower()):
        filename += path.suffix
    headers = {'Accept-Ranges': 'bytes', 'Content-Length': str(max(0, end - start + 1)),
               'Cache-Control': 'private, no-store', 'X-Content-Type-Options': 'nosniff',
               'Content-Security-Policy': "default-src 'none'; sandbox",
               'Content-Disposition': ('attachment' if download else 'inline') +
                    "; filename=\"artifact\"; filename*=UTF-8''" + quote(filename, safe='')}
    if partial:
        headers['Content-Range'] = f'bytes {start}-{end}/{size}'
    def chunks():
        try:
            file.seek(start)
            remaining = max(0, end - start + 1)
            while remaining:
                chunk = file.read(min(1024 * 1024, remaining))
                if not chunk:
                    break
                remaining -= len(chunk)
                yield chunk
        finally:
            file.close()
    return StreamingResponse(chunks(), status_code=206 if partial else 200,
                             media_type=artifact.mime, headers=headers)


@app.get(PREFIX + '/artifacts/{artifact_id}/content')
def content(artifact_id: str, request: Request, user: User = Depends(current_user)):
    return file_response(artifact_id, request, user, False)


@app.get(PREFIX + '/artifacts/{artifact_id}/download')
def download(artifact_id: str, request: Request, user: User = Depends(current_user)):
    return file_response(artifact_id, request, user, True)


@app.get(PREFIX + '/admin/users')
def users(user: User = Depends(admin_user)):
    with transaction(write=True) as db:
        audit(db, user.id, 'admin.users.list')
        return [user_json(u) for u in db.scalars(select(User).order_by(User.created_at.desc()).limit(200))]


def moderate(user_id, action, admin):
    with transaction(write=True) as db:
        user = lock_user(db, user_id)
        if user is None:
            raise ApiError('NOT_FOUND', '账号不存在', 404)
        if user.role == 'admin':
            raise ApiError('ADMIN_PROTECTED', '管理员账号只能通过运维流程管理', 409)
        user.status = {'approve': 'active', 'reject': 'rejected', 'disable': 'disabled'}[action]
        user.reviewed_at, user.reviewed_by = now(), admin.id
        if user.status != 'active':
            revoke_sessions(db, user.id)
        audit(db, admin.id, 'admin.user.' + action, user.id)
        return user_json(user)


@app.post(PREFIX + '/admin/users/{user_id}/approve')
def approve(user_id: str, user: User = Depends(admin_user)):
    return moderate(user_id, 'approve', user)


@app.post(PREFIX + '/admin/users/{user_id}/reject')
def reject(user_id: str, user: User = Depends(admin_user)):
    return moderate(user_id, 'reject', user)


@app.post(PREFIX + '/admin/users/{user_id}/disable')
def disable(user_id: str, user: User = Depends(admin_user)):
    return moderate(user_id, 'disable', user)


@app.get(PREFIX + '/admin/jobs')
def admin_jobs(user: User = Depends(admin_user)):
    with transaction(write=True) as db:
        audit(db, user.id, 'admin.jobs.list')
        jobs = db.scalars(select(Job).where(Job.deleted_at.is_(None), Job.delete_requested.is_(False))
                         .order_by(Job.created_at.desc()).limit(200))
        return [job_json(db, j) for j in jobs]


@app.get(PREFIX + '/admin/settings')
def get_settings(user: User = Depends(admin_user)):
    with transaction() as db:
        return settings(db)


@app.patch(PREFIX + '/admin/settings')
def patch_settings(body: SettingsPatch, user: User = Depends(admin_user)):
    patch = body.model_dump(exclude_unset=True)
    with transaction(write=True) as db:
        lock_user(db, user.id)
        for key, value in patch.items():
            row = db.get(Setting, key)
            if row is None:
                row = Setting(key=key, value=value)
                db.add(row)
            row.value, row.updated_by, row.updated_at = value, user.id, now()
        audit(db, user.id, 'admin.settings.update', details={'changes': patch})
        db.flush()
        return settings(db)


@app.get(PREFIX + '/health/live')
def live():
    return {'status': 'ok'}


@app.get(PREFIX + '/health/ready')
def ready():
    result = health()
    return JSONResponse(result, status_code=200 if result['status'] == 'ready' else 503)
