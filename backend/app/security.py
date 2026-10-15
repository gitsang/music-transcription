from datetime import timedelta
import hashlib
import re
import secrets
import unicodedata
from argon2 import PasswordHasher, Type
from argon2.exceptions import VerificationError, InvalidHashError
from fastapi import Request
from sqlalchemy import select, update
from sqlalchemy.exc import IntegrityError
from .config import get_config
from .db import transaction
from .errors import ApiError
from .models import User, Session, RateLimit, AuditLog, now

PASSWORDS = PasswordHasher(type=Type.ID)
DUMMY_HASH = PASSWORDS.hash(secrets.token_urlsafe(32))
COOKIE = 'music_session'


def digest(token):
    return hashlib.sha256(token.encode()).hexdigest()


def normalize_username(username):
    username = unicodedata.normalize('NFKC', username).strip().casefold()
    if not re.fullmatch(r'[\w.-]{3,64}', username, re.UNICODE):
        raise ApiError('INVALID_USERNAME', '用户名需为3–64个字母、数字、下划线、点或连字符')
    return username


def validate_password(password):
    if not 12 <= len(password) <= 128:
        raise ApiError('INVALID_PASSWORD', '密码长度须为12–128个字符')


def verify(password, password_hash):
    try:
        return PASSWORDS.verify(password_hash, password)
    except (VerificationError, InvalidHashError):
        return False


def audit(db, actor_id, action, resource_id=None, details=None):
    db.add(AuditLog(actor_id=actor_id, action=action,
                    resource_id=resource_id, details=details or {}))


def rate_limit(request, action, username=''):
    # Do not trust a client-provided forwarding header. A shared DB enforces
    # the same window across API processes and restarts; IP is stored hashed.
    host = request.client.host if request.client else 'local'
    keys = [(digest(f'{action}:ip:{host}'), 40 if action == 'login' else 10)]
    if username:
        keys.append((digest(f'{action}:account:{username}'), 10))
    for key, maximum in keys:
        for attempt in range(3):
            try:
                with transaction(write=True) as db:
                    row = db.scalar(select(RateLimit).where(RateLimit.key == key).with_for_update())
                    if row is None:
                        row = RateLimit(key=key, count=0, reset_at=now() + timedelta(minutes=15))
                        db.add(row)
                    elif row.reset_at <= now():
                        row.count, row.reset_at = 0, now() + timedelta(minutes=15)
                    if row.count >= maximum:
                        raise ApiError('RATE_LIMITED', '请求过于频繁，请稍后重试', 429)
                    row.count += 1
                break
            except IntegrityError:
                if attempt == 2:
                    raise ApiError('RATE_LIMITED', '请求过于频繁，请稍后重试', 429)


def current_user(request: Request):
    token = request.cookies.get(COOKIE, '')
    if not token or len(token) > 256:
        raise ApiError('AUTH_REQUIRED', '请先登录', 401)
    with transaction() as db:
        user = db.scalar(select(User).join(Session).where(
            Session.token_hash == digest(token), Session.revoked_at.is_(None),
            Session.expires_at > now(), User.status == 'active'))
        if user is None:
            raise ApiError('AUTH_REQUIRED', '会话已失效，请重新登录', 401)
        return user


def admin_user(request: Request):
    user = current_user(request)
    if user.role != 'admin':
        raise ApiError('FORBIDDEN', '需要管理员权限', 403)
    return user


def revoke_sessions(db, user_id):
    db.execute(update(Session).where(Session.user_id == user_id, Session.revoked_at.is_(None)).values(revoked_at=now()))
