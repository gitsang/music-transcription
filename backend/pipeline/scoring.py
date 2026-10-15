import copy
import uuid
from music21 import chord, clef, instrument, meter, note, stream, tempo


def build_score(result):
    score = stream.Score()
    bpm = float(result.get('bpm') or 120)
    score.insert(0, tempo.MetronomeMark(number=bpm))
    tracks = result['tracks']
    source_map = []
    for track in tracks:
        part = stream.Part(id=track['id'])
        part.partName = track['label']
        if not track['is_drum']:
            inst = instrument.Instrument()
            inst.instrumentName = track['label']
            inst.midiProgram = track.get('program') or 0
            part.insert(0, inst)
        part.insert(0, meter.TimeSignature('4/4'))
        part.insert(0, clef.PercussionClef() if track['is_drum'] else clef.bestClef(part))
        events = [e for e in result['events'] if e['track_id'] == track['id']]
        # Separate pitch voices retain simultaneous notes with independent durations.
        groups = {}
        for event in events:
            q = max(0, round(event['start_sec'] * bpm / 60 * 4) / 4)
            end = max(q + .25, round(event['end_sec'] * bpm / 60 * 4) / 4)
            groups.setdefault(event['midi_pitch'], []).append((q, end, event))
        for pitch, values in groups.items():
            voice = stream.Voice()
            if track['is_drum']:
                mapping = {36: instrument.BassDrum, 38: instrument.SnareDrum, 42: instrument.HiHatCymbal}
                drum = mapping.get(pitch, instrument.BassDrum)()
                drum.percMapPitch = pitch
                drum.midiChannel = 9
                voice.insert(0, drum)
            last_end = 0
            for q, end, event in sorted(values, key=lambda x: x[0]):
                q = max(q, last_end)
                if end <= q:
                    continue
                if q > last_end:
                    voice.insert(last_end, note.Rest(quarterLength=q-last_end))
                if track['is_drum']:
                    n = note.Unpitched()
                    n.storedInstrument = drum
                else:
                    n = note.Note(pitch)
                source_map.append({**event, 'quarter': q, 'end_quarter': end})
                n.duration.quarterLength = end-q
                n.volume.velocity = event['velocity']
                voice.insert(q, n)
                last_end = end
            part.insert(0, voice)
        score.insert(0, part)
    score = score.makeNotation(inPlace=False)
    return score, source_map


def timeline(score, tracks, source_map=None):
    """Use music21 tempo boundaries; absolute quarter offsets, not a fixed BPM clock."""
    bounds = score.metronomeMarkBoundaries()
    tempo_map = []
    for start, end, mark in bounds:
        bpm = mark.getQuarterBPM() or 120
        if not tempo_map or tempo_map[-1]['bpm'] != bpm:
            tempo_map.append({'quarter': float(start), 'bpm': float(bpm)})
    if not tempo_map:
        tempo_map = [{'quarter': 0., 'bpm': 120.}]

    def seconds(q):
        t = 0.
        for i, change in enumerate(tempo_map):
            a = change['quarter']
            b = tempo_map[i+1]['quarter'] if i+1 < len(tempo_map) else q
            if q > a:
                t += max(0., min(q, b)-a)*60/change['bpm']
        return t

    events = []
    for index, part in enumerate(score.parts):
        tid = tracks[index]['id']
        for n in part.recurse().notes:
            q = float(n.getOffsetInHierarchy(score))
            end = q + float(n.duration.quarterLength)
            pitches = list(n.pitches) if hasattr(n, 'pitches') else []
            source = None
            if source_map:
                candidates = [e for e in source_map if e['track_id'] == tid and
                              e['quarter'] <= q < e['end_quarter'] and
                              (tracks[index]['is_drum'] or any(p.midi == e['midi_pitch'] for p in pitches))]
                if candidates:
                    source = min(candidates, key=lambda e: abs(e['quarter']-q))
            source_start = source_end = None
            if source:
                length = source['end_quarter']-source['quarter']
                span = source['end_sec']-source['start_sec']
                source_start = source['start_sec'] + max(0., q-source['quarter']) / length * span
                source_end = source['start_sec'] + min(length, end-source['quarter']) / length * span
            events.append({'id': str(uuid.uuid4()), 'track_id': tid,
                'source_event_id': source['id'] if source else None,
                'score_position': {'measure': n.measureNumber or 1, 'beat': float(n.beat or 1), 'quarter': q},
                'source_start_sec': source_start,
                'source_end_sec': source_end,
                'synth_start_sec': seconds(q), 'synth_end_sec': seconds(end)})
    return {'version': 1, 'initial_offset_sec': 0, 'tempo_map': tempo_map,
            'warnings': ['游标按谱面事件近似定位，非逐音符准确高亮'], 'events': events}
