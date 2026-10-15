"""Runs only in MODEL_PYTHON. Offline weights; no service/database imports."""
import argparse
import json
import os
from pathlib import Path
import uuid


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('mode', choices=['device', 'analyze', 'pitch', 'drums'])
    parser.add_argument('input')
    parser.add_argument('output')
    parser.add_argument('--track-id', default='')
    args = parser.parse_args()
    result = {}
    if args.mode == 'device':
        import torch
        requested = os.getenv('INFERENCE_DEVICE', 'auto')
        if requested not in ['auto', 'cuda', 'cpu']:
            raise RuntimeError('Invalid inference device')
        available = torch.cuda.is_available()
        if requested == 'cuda' and not available:
            raise RuntimeError('Requested CUDA is unavailable')
        device = 'cuda' if requested != 'cpu' and available else 'cpu'
        reason = 'CUDA 不可用，auto 使用 CPU' if requested == 'auto' and not available else None
        result = {'device': device, 'reason': reason, 'torch': torch.__version__}
    elif args.mode == 'analyze':
        import librosa
        import numpy as np
        y, sr = librosa.load(args.input, sr=22050)
        bpm, beats = librosa.beat.beat_track(y=y, sr=sr)
        result = {'bpm': float(np.asarray(bpm).reshape(-1)[0]) if np.asarray(bpm).size else 120.}
        if not 20 <= result['bpm'] <= 300:
            result['bpm'] = 120.
            result['warning'] = '速度估计不足，使用 120 BPM 默认值'
    elif args.mode == 'pitch':
        from basic_pitch.inference import predict
        _, _, events = predict(args.input, model_or_model_path=os.environ['BASIC_PITCH_MODEL_PATH'])
        result = {'events': [{'id': str(uuid.uuid4()), 'track_id': args.track_id,
                    'start_sec': float(start), 'end_sec': float(end), 'midi_pitch': int(pitch),
                    'velocity': max(1, min(127, round(float(amplitude)*127))), 'is_drum': False}
                   for start, end, pitch, amplitude, _ in events]}
    elif args.mode == 'drums':
        import numpy as np
        import soundfile as sf
        from scipy.signal import stft, find_peaks
        y, sr = sf.read(args.input, always_2d=True, dtype='float32')
        y = y.mean(axis=1)
        f, t, z = stft(y, sr, nperseg=2048, noverlap=1536)
        magnitude = np.abs(z)
        events = []
        for pitch, lo, hi in [(36, 35, 110), (38, 120, 350), (42, 5000, 13000)]:
            energy = magnitude[(f >= lo) & (f <= hi)].sum(axis=0)
            flux = np.maximum(0, np.diff(energy, prepend=energy[0]))
            if flux.max(initial=0) < 1e-6:
                continue
            indices, _ = find_peaks(flux, height=flux.mean()+1.5*flux.std(), distance=max(1, int(.1*sr/512)))
            for index in indices:
                start = float(t[index])
                events.append({'id': str(uuid.uuid4()), 'track_id': args.track_id,
                    'start_sec': start, 'end_sec': start+.1, 'midi_pitch': pitch,
                    'velocity': 90, 'is_drum': True})
        result = {'events': events}
    Path(args.output).write_text(json.dumps(result))


if __name__ == '__main__':
    main()
