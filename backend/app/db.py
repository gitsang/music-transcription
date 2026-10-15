from contextlib import contextmanager
from functools import lru_cache
from sqlalchemy import create_engine, event, select
from sqlalchemy.orm import sessionmaker
from .config import get_config, DEFAULT_SETTINGS
from .models import Setting, User


@lru_cache
def engine():
    url = get_config().database_url
    kwargs = {'pool_pre_ping': True}
    if url.startswith('sqlite'):
        kwargs['connect_args'] = {'check_same_thread': False, 'timeout': 30}
    result = create_engine(url, **kwargs)
    if url.startswith('sqlite'):
        @event.listens_for(result, 'connect')
        def sqlite_config(connection, _):
            connection.execute('PRAGMA foreign_keys=ON')
            connection.execute('PRAGMA busy_timeout=30000')
    return result


@contextmanager
def transaction(write=False):
    with sessionmaker(bind=engine(), expire_on_commit=False)() as db:
        if write and engine().dialect.name == 'sqlite':
            # SQLite lacks row locks: reserve its write lock before reading quota.
            db.connection().exec_driver_sql('BEGIN IMMEDIATE')
        try:
            yield db
            db.commit()
        except BaseException:
            db.rollback()
            raise


def lock_user(db, user_id):
    return db.scalar(select(User).where(User.id == user_id).with_for_update())


def settings(db):
    result = dict(DEFAULT_SETTINGS)
    result.update({row.key: row.value for row in db.scalars(select(Setting))})
    return result
