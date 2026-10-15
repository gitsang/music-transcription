import copy
import hashlib
import json
import os
from pathlib import Path
import shutil
import uuid
from functools import lru_cache


@lru_cache(maxsize=16)
def verified_file(path, size, mtime_ns, expected):
    h = hashlib.sha256()
    with Path(path).open('rb') as f:
        for block in iter(lambda: f.read(1024 * 1024), b''):
            h.update(block)
    return h.hexdigest() == expected
from .common import PipelineError, run
from .xmlio import load_xml
from .exports import fix_drum_channels

LIMITATIONS = ['自动扒谱初稿，可能漏音、错音和节奏误差',
              '分离器粗分类，未实现任意乐器识别；同类多个演奏者不可区分',
              '鼓为频带 onset 近似检测，不是可靠鼓分类；other 为未知/混合声部']


def capabilities():
    repo = Path(os.getenv('DEMUCS_REPO', '/models/demucs'))
    bp = Path(os.getenv('BASIC_PITCH_MODEL_PATH', '/models/basic-pitch/nmp.onnx'))
    try:
        required = [(repo/'5c90dfd2.th', '34c22ccb381c6f9fdbf324f04e1e2fe21aaaf293f5ded163a162697ff9a02ddd'),
                    (bp, '2c3c1d144bfa61ad236e92e169c13535c880469a12a047d4e73451f2c059a0ec')]
        ready = (repo/'htdemucs_6s.yaml').is_file() and all(
            verified_file(str(p), p.stat().st_size, p.stat().st_mtime_ns, expected) for p, expected in required)
    except OSError:
        ready = False
    return [{'id': 'baseline', 'label': 'Demucs 6类 + Basic Pitch（有限能力）',
             'ready': ready, 'limitations': LIMITATIONS},
            {'id': 'multi_instrument', 'label': '多乐器识别（未集成）', 'ready': False,
             'limitations': ['候选模型及 CPU / Tesla P4 尚未验证，不能用于任务']}]


def process(input_path, work_dir, kind, config, progress):
    from music21 import converter, instrument
    from .scoring import build_score, timeline
    import soundfile as sf
    import numpy as np
    progress('validating', None)
    work = Path(work_dir)
    work.mkdir(parents=True, exist_ok=True)
    artifacts, warnings, tracks = [], [], []
    actual_device = 'cpu'
    source_map = None

    def add(path, artifact_kind, label, mime, track_id=None):
        path = Path(path)
        if not path.is_file() or path.stat().st_size == 0:
            raise PipelineError('EMPTY_ARTIFACT', '工具未生成有效文件')
        artifacts.append({'kind': artifact_kind, 'label': label, 'mime': mime,
                          'track_id': track_id, 'path': str(path.relative_to(work)), 'status': 'ready', 'error': None})

    def optional(artifact_kind, label, mime, callback, track_id=None):
        try:
            path = callback()
            add(path, artifact_kind, label, mime, track_id)
        except (PipelineError, OSError, ValueError) as exc:
            # Cancellation raised by progress is not swallowed.
            artifacts.append({'kind': artifact_kind, 'label': label, 'mime': mime,
                              'track_id': track_id, 'status': 'failed', 'error': getattr(exc, 'message', '本地导出失败')})

    progress('validating', None)
    if kind == 'musicxml':
        data, root = load_xml(Path(input_path))
        score_path = work/'score.musicxml'
        score_path.write_bytes(data)
        try:
            score = converter.parse(str(score_path), forceSource=True)
        except Exception as exc:
            raise PipelineError('INVALID_MUSICXML', '无法解析乐谱，请检查 MusicXML 内容') from exc
        if root.findall('.//repeat') or root.findall('.//ending') or root.findall('.//segno') or root.findall('.//coda'):
            warnings.append('此 Demo 只做直线播放，反复、跳房子及 D.C./D.S. 不展开；原谱保持不变')
        warnings.append('导入原谱保持原有记谱；复杂演奏记号可能无法准确合成')
        for part in score.parts:
            instruments = list(part.recurse().getElementsByClass(instrument.Instrument))
            inst = instruments[0] if instruments else instrument.Instrument()
            tracks.append({'id': str(uuid.uuid4()), 'label': part.partName or inst.instrumentName or '未知乐器',
                'program': inst.midiProgram, 'confidence': None, 'label_source': 'imported',
                'is_drum': inst.midiChannel == 9 or isinstance(inst, instrument.UnpitchedPercussion), 'source_stem_ids': []})
    else:
        if not capabilities()[0]['ready']:
            raise PipelineError('MODEL_NOT_READY', '本地模型缺失，请管理员准备并验证模型')
        python = os.getenv('MODEL_PYTHON', '/opt/models-venv/bin/python')
        infer = Path(__file__).with_name('infer.py')
        if not Path(python).is_file():
            raise PipelineError('MODEL_NOT_READY', '模型运行环境不可用')
        device_path = work/'device.json'
        try:
            run([python, infer, 'device', input_path, device_path], work, progress, 'preparing', 60)
        except PipelineError as exc:
            raise PipelineError('DEVICE_NOT_AVAILABLE', '指定推理设备或模型环境不可用，请检查 CUDA 配置') from exc
        device = json.loads(device_path.read_text())
        actual_device = device['device']
        if device.get('reason'):
            warnings.append(device['reason'])
        # ffprobe validates actual content in the restricted worker, not the upload API.
        probe_path = work/'probe.json'
        run(['ffprobe', '-v', 'error', '-protocol_whitelist', 'file,pipe', '-format_whitelist', 'wav,mp3,flac,mov,ogg', '-show_format', '-show_streams', '-of', 'json', '-o', probe_path, input_path], work, progress, 'validating', 30)
        try:
            metadata = json.loads(probe_path.read_text())
            duration = float(metadata['format']['duration'])
            audio_streams = [s for s in metadata['streams'] if s['codec_type'] == 'audio']
            if not audio_streams or any(int(s.get('channels', 0)) not in range(1, 9) or not 8000 <= int(s.get('sample_rate', 0)) <= 192000 for s in audio_streams):
                raise ValueError('audio stream limit')
        except Exception as exc:
            raise PipelineError('INVALID_AUDIO', '音频无法解码或采样率、声道不受支持') from exc
        if not 0 < duration <= config.get('max_audio_duration_seconds', 600):
            raise PipelineError('AUDIO_TOO_LONG', '音频超过管理员设置的时长上限')
        normalized = work/'normalized.wav'
        run(['ffmpeg', '-nostdin', '-v', 'error', '-y', '-protocol_whitelist', 'file,pipe', '-format_whitelist', 'wav,mp3,flac,mov,ogg', '-i', input_path, '-map', '0:a:0', '-t', str(duration), '-ac', '2', '-ar', '44100', normalized], work, progress, 'preparing')
        preview = work/'original.mp3'
        optional('original_preview', '原曲试听', 'audio/mpeg', lambda: convert_audio(normalized, preview, work, progress))
        analysis = work/'analysis.json'
        run([python, infer, 'analyze', normalized, analysis], work, progress, 'analyzing')
        info = json.loads(analysis.read_text())
        warnings.extend(LIMITATIONS)
        warnings.append('自动估计恒定速度；拍号采用默认 4/4，不保证复杂拍号及速度变化准确')
        if info.get('warning'):
            warnings.append(info['warning'])
        separation = work/'separated'
        command = [python, '-m', 'demucs', '-n', 'htdemucs_6s', '--repo', os.getenv('DEMUCS_REPO', '/models/demucs'), '-d', actual_device,
                   '--shifts', '0', '--segment', '7', '-j', '1', '-o', separation, normalized]
        for segment in ('7', '4', '2'):
            command[command.index('--segment') + 1] = segment
            try:
                run(command, work, progress, 'separating')
                break
            except PipelineError as exc:
                if exc.code != 'OUT_OF_MEMORY' or segment == '2':
                    raise
                warnings.append(f'显存/内存不足，保持同一模型缩小分块重试（原分块 {segment} 秒）')
        stems = separation/'htdemucs_6s'/'normalized'
        events = []
        labels = {'vocals': ('人声（粗分类）', 53), 'bass': ('贝斯（粗分类）', 33),
                  'guitar': ('吉他（粗分类）', 24), 'piano': ('钢琴（粗分类）', 0),
                  'other': ('未知/混合声部', None), 'drums': ('鼓（onset 近似）', None)}
        for category, (label, program) in labels.items():
            stem = stems/f'{category}.wav'
            stem_id = str(uuid.uuid4())
            add(stem, 'stem', f'{category} 分离原始音频', 'audio/wav', stem_id)
            optional('stem_preview', f'{category} 分离试听', 'audio/mpeg',
                     lambda s=stem, c=category: convert_audio(s, work/f'{c}.mp3', work, progress), stem_id)
            audio, _ = sf.read(stem, dtype='float32')
            rms = float(np.sqrt(np.mean(audio**2)))
            if rms < 10**(-60/20):
                warnings.append(f'{category} 低能量音轨（RMS < -60 dBFS）未转谱，分离音频仍可下载')
                continue
            tid = str(uuid.uuid4())
            track = {'id': tid, 'label': label, 'program': program, 'confidence': None,
                     'is_drum': category == 'drums', 'label_source': 'unknown' if category == 'other' else 'separator_category',
                     'source_stem_ids': [stem_id]}
            output = work/f'events-{category}.json'
            run([python, infer, 'drums' if category == 'drums' else 'pitch', stem, output, '--track-id', tid], work, progress, 'transcribing')
            detected = json.loads(output.read_text())['events']
            if detected:
                tracks.append(track)
                events.extend(detected)
            else:
                warnings.append(f'{category} 未识别出音符；未声称保留该声部')
        if not tracks:
            raise PipelineError('NO_NOTES_DETECTED', '未识别出可生成乐谱的音符')
        notes_path = work/'notes.json'
        notes_path.write_text(json.dumps({'tracks': tracks, 'events': events, 'bpm': info['bpm']}, ensure_ascii=False))
        add(notes_path, 'notes', '原始音符事件', 'application/json')
        progress('scoring', None)
        score, source_map = build_score({'tracks': tracks, 'events': events, 'bpm': info['bpm']})
        score_path = work/'score.musicxml'
        score.write('musicxml', fp=str(score_path))
        fix_drum_channels(score_path)
    if not tracks:
        raise PipelineError('INVALID_MUSICXML', '乐谱不包含可读声部')
    load_xml(score_path)
    add(score_path, 'score', 'MusicXML 总谱', 'application/vnd.recordare.musicxml+xml')
    progress('exporting', None)
    midi = work/'score.mid'
    try:
        score.write('midi', fp=str(midi))
        add(midi, 'midi', 'MIDI 总谱', 'audio/midi')
    except Exception as exc:
        artifacts.append({'kind': 'midi', 'label': 'MIDI 总谱', 'mime': 'audio/midi', 'track_id': None, 'status': 'failed', 'error': 'MIDI 导出失败'})
    for index, track in enumerate(tracks):
        part_path = work/f"parts/{track['id']}.musicxml"
        part_path.parent.mkdir(exist_ok=True)
        part_score = copy.deepcopy(score.parts[index])
        # Include global tempo marks in parts where they were stored on the score.
        from music21 import tempo
        for mark in score.getElementsByClass(tempo.MetronomeMark):
            part_score.insert(mark.offset, copy.deepcopy(mark))
        try:
            part_score.write('musicxml', fp=str(part_path))
            if kind != 'musicxml':
                fix_drum_channels(part_path)
            add(part_path, 'part', track['label']+' MusicXML', 'application/vnd.recordare.musicxml+xml', track['id'])
        except Exception:
            progress('exporting', None)
            artifacts.append({'kind': 'part', 'label': track['label']+' MusicXML', 'mime': 'application/vnd.recordare.musicxml+xml',
                              'track_id': track['id'], 'status': 'failed', 'error': '该声部 MusicXML 导出失败'})
        optional('part_pdf', track['label']+' PDF', 'application/pdf', lambda p=part_path: export_pdf(p, work, progress), track['id'])
    optional('pdf', 'PDF 总谱', 'application/pdf', lambda: export_pdf(score_path, work, progress))
    if midi.exists():
        wav = work/'synth.wav'
        def synthesize():
            soundfont = Path(os.getenv('SOUNDFONT_PATH', '/soundfonts/default.sf2'))
            if not soundfont.is_file():
                raise PipelineError('SOUNDFONT_NOT_READY', '本地 SoundFont 缺失，无法合成演奏')
            run(['fluidsynth', '-ni', '-F', wav, '-r', '44100', soundfont, midi], work, progress, 'exporting')
            return wav
        optional('synth', '合成演奏 WAV', 'audio/wav', synthesize)
        if wav.exists():
            optional('synth_preview', '合成演奏试听', 'audio/mpeg', lambda: convert_audio(wav, work/'synth.mp3', work, progress))
    timeline_path = work/'timeline.json'
    timeline_path.write_text(json.dumps(timeline(score, tracks, source_map), ensure_ascii=False))
    add(timeline_path, 'timeline', '播放时间线', 'application/json')
    progress('finalizing', None)
    manifest = work/'manifest.json'
    manifest.write_text(json.dumps({'engine': 'imported' if kind == 'musicxml' else 'baseline',
        'actual_device': actual_device, 'warnings': warnings, 'tracks': tracks,
        'artifacts': [{k: v for k, v in a.items() if k != 'path'} for a in artifacts]}, ensure_ascii=False))
    add(manifest, 'manifest', '产物清单', 'application/json')
    return {'tracks': tracks, 'artifacts': artifacts, 'warnings': warnings,
            'engine': 'imported' if kind == 'musicxml' else 'baseline', 'actual_device': actual_device}


def convert_audio(source, target, work, progress):
    run(['ffmpeg', '-nostdin', '-v', 'error', '-y', '-i', source, '-codec:a', 'libmp3lame', '-q:a', '3', target], work, progress, 'exporting')
    return target


def export_pdf(source, work, progress):
    target = source.with_suffix('.pdf')
    binary = os.getenv('MUSESCORE_BIN', 'musescore3')
    run([binary, '-o', target, source], work, progress, 'exporting', 120)
    return target
