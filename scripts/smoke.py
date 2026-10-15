#!/usr/bin/env python3
"""Real service acceptance: approvals, quota races, private Range, XML/audio worker.

Requires an existing administrator. Password is read from stdin. Creates accounts
and jobs whose names start smoke_; it does not modify production settings.
"""
import argparse
from concurrent.futures import ThreadPoolExecutor
import json
from pathlib import Path
import secrets
import sys
import time
import httpx


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--url', default='http://localhost:8080')
    parser.add_argument('--admin', required=True)
    parser.add_argument('--xml', type=Path, required=True)
    parser.add_argument('--audio', type=Path)
    parser.add_argument('--timeout', type=int, default=600)
    args = parser.parse_args()
    password = sys.stdin.readline().rstrip('\r\n')
    admin = httpx.Client(base_url=args.url, timeout=120)
    response = admin.post('/api/v1/auth/login', json={'username': args.admin, 'password': password})
    response.raise_for_status()
    suffix = secrets.token_hex(5)
    user_password = secrets.token_urlsafe(24)
    clients, ids = [], []
    for index in range(2):
        name = f'smoke_{suffix}_{index}'
        client = httpx.Client(base_url=args.url, timeout=120)
        creds = {'username': name, 'password': user_password}
        client.post('/api/v1/auth/register', json=creds).raise_for_status()
        assert client.post('/api/v1/auth/login', json=creds).json()['error']['code'] == 'ACCOUNT_PENDING'
        uid = next(u['id'] for u in admin.get('/api/v1/admin/users').json() if u['username'] == name)
        admin.post(f'/api/v1/admin/users/{uid}/approve').raise_for_status()
        client.post('/api/v1/auth/login', json=creds).raise_for_status()
        clients.append(client)
        ids.append(uid)
    first, second = clients

    def upload(kind, path):
        with path.open('rb') as file:
            return first.post('/api/v1/jobs', data={'kind': kind}, files={'file': (path.name, file)})

    # Invalid XML is allowed into the asynchronous queue but never parsed by API.
    # For a deterministic quota race, use a stopped worker/dispatcher externally.
    jobs = []
    with ThreadPoolExecutor(max_workers=5) as pool:
        responses = list(pool.map(lambda _: upload('musicxml', args.xml), range(5)))
    for r in responses:
        assert r.status_code in (202, 409), r.text
        if r.status_code == 202:
            jobs.append(r.json())
        else:
            assert r.json()['error']['code'] == 'JOB_LIMIT_REACHED'
    assert jobs
    job = jobs[0]
    original = next(a for a in job['artifacts'] if a['kind'] == 'original')
    for path in [f'/api/v1/jobs/{job["id"]}', f'/api/v1/jobs/{job["id"]}/tracks',
                 f'/api/v1/artifacts/{original["id"]}/content', f'/api/v1/artifacts/{original["id"]}/download']:
        assert second.get(path, headers={'Range': 'bytes=0-8'}).status_code == 404
    assert first.get(f'/api/v1/artifacts/{original["id"]}/content', headers={'Range': 'bytes=0-8'}).status_code == 206
    assert admin.get(f'/api/v1/artifacts/{original["id"]}/content', headers={'Range': 'bytes=0-8'}).status_code == 206
    for extra in jobs[1:]:
        first.post(f'/api/v1/jobs/{extra["id"]}/cancel').raise_for_status()
        first.delete(f'/api/v1/jobs/{extra["id"]}').raise_for_status()

    def finished(jid):
        end = time.monotonic()+args.timeout
        while time.monotonic() < end:
            r = first.get(f'/api/v1/jobs/{jid}')
            r.raise_for_status()
            value = r.json()
            if value['status'] not in ('queued', 'running'):
                assert value['status'] in ('succeeded', 'partial'), value
                assert any(a['kind'] == 'score' and a['status'] == 'ready' for a in value['artifacts'])
                for artifact in value['artifacts']:
                    if artifact['status'] == 'ready':
                        # Download the actual payload, including PDF/MIDI/audio/timeline.
                        downloaded = first.get(f'/api/v1/artifacts/{artifact["id"]}/download')
                        downloaded.raise_for_status()
                        assert len(downloaded.content) == artifact['bytes']
                print(json.dumps({'id': jid, 'kind': value['kind'], 'status': value['status'],
                                  'device': value['actual_device'], 'tracks': len(value['tracks']),
                                  'artifacts': len(value['artifacts'])}), flush=True)
                return value
            time.sleep(2)
        raise TimeoutError('Worker acceptance timed out')

    imported = finished(job['id'])
    assert not any(a['kind'] == 'stem' for a in imported['artifacts'])
    if args.audio:
        response = upload('audio', args.audio)
        response.raise_for_status()
        audio = finished(response.json()['id'])
        assert any(a['kind'] == 'stem' for a in audio['artifacts'])
    admin.post(f'/api/v1/admin/users/{ids[0]}/disable').raise_for_status()
    assert first.get('/api/v1/auth/me').status_code == 401
    assert first.get(f'/api/v1/artifacts/{original["id"]}/content').status_code == 401
    print('Real-service smoke passed: approval, concurrent upload, ownership/Range, downloads, disable revocation.')


if __name__ == '__main__':
    main()
