#!/usr/bin/env python3
"""Operator-only baseline preparation. Downloads public weights, never accepts audio."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import shutil
import tempfile
import urllib.request
import zipfile

ROOT = Path(__file__).resolve().parents[1]


def digest(path):
    h = hashlib.sha256()
    with path.open('rb') as f:
        for block in iter(lambda: f.read(1024 * 1024), b''):
            h.update(block)
    return h.hexdigest()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--output', type=Path, default=ROOT / 'models')
    parser.add_argument('--demucs-checkpoint', type=Path, help='Offline local checkpoint')
    parser.add_argument('--basic-pitch-onnx', type=Path, help='Offline local ONNX weight')
    parser.add_argument('--download', action='store_true', help='Explicitly allow public weight download')
    args = parser.parse_args()
    lock = json.loads((ROOT / 'models.lock.json').read_text())
    args.output.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(dir=args.output) as tmp:
        for model in lock['models']:
            target = args.output / model['path']
            source = args.demucs_checkpoint if model['id'] == 'htdemucs_6s' else args.basic_pitch_onnx
            if target.is_file() and digest(target) == model['sha256']:
                print(f"Verified {model['id']}")
                continue
            temporary = Path(tmp) / model['id']
            if source:
                shutil.copyfile(source, temporary)
            elif args.download:
                request = urllib.request.Request(model['url'], headers={'User-Agent': 'music-transcription-prepare/1'})
                with urllib.request.urlopen(request, timeout=120) as response, temporary.open('wb') as out:
                    shutil.copyfileobj(response, out)
                if model.get('wheel_member'):
                    with zipfile.ZipFile(temporary) as wheel:
                        content = wheel.read(model['wheel_member'])
                    temporary.write_bytes(content)
            else:
                parser.error(f"{model['id']} missing: provide offline file or explicitly use --download")
            if digest(temporary) != model['sha256']:
                raise SystemExit(f"Checksum mismatch: {model['id']}; no model published")
            target.parent.mkdir(parents=True, exist_ok=True)
            os.replace(temporary, target)
            print(f"Prepared {model['id']}")
    (args.output / 'demucs' / 'htdemucs_6s.yaml').write_text("models: ['5c90dfd2']\n")
    shutil.copyfile(ROOT / 'models.lock.json', args.output / 'manifest.json')
    print('Preparation complete. Verify inference offline before accepting production audio.')


if __name__ == '__main__':
    main()
