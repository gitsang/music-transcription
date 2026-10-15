"""Deployment operations: python -m app.cli migrate|init-admin|dispatcher|health."""
import argparse
import getpass
import json
import logging
import os
from pathlib import Path
import sys
from sqlalchemy import select
from .config import get_config
from .db import transaction
from .models import User
from .security import PASSWORDS, normalize_username, validate_password, audit
from .storage import initialize_storage


def migrate():
    from alembic import command
    from alembic.config import Config
    initialize_storage()
    root = Path(__file__).resolve().parents[1]
    config = Config(str(root / 'alembic.ini'))
    config.set_main_option('script_location', str(root / 'alembic'))
    command.upgrade(config, 'head')


def init_admin(username, password):
    username = normalize_username(username)
    validate_password(password)
    with transaction(write=True) as db:
        if db.scalar(select(User).where(User.username == username)):
            raise ValueError('Username already exists; no account was changed')
        user = User(username=username, password_hash=PASSWORDS.hash(password), role='admin', status='active')
        db.add(user)
        db.flush()
        audit(db, user.id, 'cli.admin.created', user.id)
    return user.id


def main():
    logging.basicConfig(level=logging.INFO)
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest='command', required=True)
    sub.add_parser('migrate')
    admin = sub.add_parser('init-admin')
    admin.add_argument('--username', required=True)
    admin.add_argument('--password-stdin', action='store_true', help='Read one line from stdin; never pass passwords as arguments')
    sub.add_parser('dispatcher')
    health_parser = sub.add_parser('health')
    health_parser.add_argument('--role', choices=('api', 'worker', 'dispatcher'), default='api')
    args = parser.parse_args()
    try:
        if args.command == 'migrate':
            migrate()
        elif args.command == 'init-admin':
            password = sys.stdin.readline().rstrip('\r\n') if args.password_stdin else getpass.getpass('Admin password (minimum 12 characters): ')
            if not args.password_stdin and password != getpass.getpass('Confirm password: '):
                raise ValueError('Password confirmation does not match')
            init_admin(args.username, password)
            print('Administrator created')
        elif args.command == 'dispatcher':
            from .dispatcher import run
            run()
        else:
            from .readiness import health
            result = health(args.role)
            print(json.dumps(result))
            return 0 if result['status'] == 'ready' else 1
    except (ValueError, OSError) as exc:
        print(str(exc), file=sys.stderr)
        return 1
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
