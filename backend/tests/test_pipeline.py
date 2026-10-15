import json
import zipfile
from pathlib import Path
import pytest
from music21 import instrument, meter, note, stream, tempo
from pipeline.service import process, PipelineError
from pipeline.xmlio import load_xml
from pipeline.scoring import build_score, timeline


def fixture(path):
    score = stream.Score()
    part = stream.Part()
    part.partName = '测试钢琴'
    part.insert(0, instrument.Piano())
    part.insert(0, meter.TimeSignature('3/4'))
    part.insert(0, tempo.MetronomeMark(number=120))
    part.insert(0, note.Note('C4', quarterLength=3))
    part.insert(3, tempo.MetronomeMark(number=60))
    part.insert(3, note.Note('D4', quarterLength=3))
    score.insert(0, part)
    score.write('musicxml', fp=str(path))


def test_import_real_exports(tmp_path, monkeypatch):
    source = tmp_path/'input.musicxml'
    fixture(source)
    monkeypatch.setenv('SOUNDFONT_PATH', '/usr/share/sounds/sf2/TimGM6mb.sf2')
    result = process(source, tmp_path/'work', 'musicxml', {}, lambda *a: None)
    assert len(result['tracks']) == 1
    artifacts = {a['kind']: a for a in result['artifacts']}
    assert artifacts['score']['status'] == 'ready'
    assert artifacts['part']['status'] == 'ready'
    assert artifacts['midi']['status'] == 'ready'
    data = json.loads((tmp_path/'work'/artifacts['timeline']['path']).read_text())
    assert data['tempo_map'] == [{'quarter': 0.0, 'bpm': 120.0}, {'quarter': 3.0, 'bpm': 60.0}]
    assert data['events'][-1]['synth_end_sec'] == 4.5
    assert all(e['source_start_sec'] is None for e in data['events'])


def test_reject_entity(tmp_path):
    path = tmp_path/'evil.xml'
    path.write_text('<!DOCTYPE x [<!ENTITY a SYSTEM "file:///etc/passwd">]><score-partwise>&a;</score-partwise>')
    with pytest.raises(PipelineError):
        load_xml(path)


def test_reject_external_image_reference(tmp_path):
    path = tmp_path/'linked.xml'
    fixture(path)
    data = path.read_text().replace('</measure>', '<direction><direction-type><image source="file:///etc/passwd" type="png"/></direction-type></direction></measure>', 1)
    path.write_text(data)
    with pytest.raises(PipelineError):
        load_xml(path)


def test_mxl_paths_and_valid(tmp_path):
    xml = tmp_path/'source.xml'
    fixture(xml)
    bad = tmp_path/'evil.mxl'
    with zipfile.ZipFile(bad, 'w') as z:
        z.writestr('../escape', 'bad')
    with pytest.raises(PipelineError):
        load_xml(bad)
    good = tmp_path/'safe.mxl'
    with zipfile.ZipFile(good, 'w') as z:
        z.writestr('META-INF/container.xml', '<container><rootfiles><rootfile full-path="score.xml"/></rootfiles></container>')
        z.writestr('score.xml', xml.read_bytes())
    assert load_xml(good)[1].tag == 'score-partwise'


def test_drum_midi_mapping(tmp_path):
    tid = 'drums'
    result = {'bpm': 120, 'tracks': [{'id': tid, 'label': '鼓', 'program': None, 'is_drum': True}],
        'events': [{'id': 'a', 'track_id': tid, 'start_sec': 0, 'end_sec': .1, 'midi_pitch': 36, 'velocity': 90}]}
    score, sources = build_score(result)
    xml = tmp_path/'drums.xml'
    score.write('musicxml', fp=str(xml))
    _, root = load_xml(xml)
    assert root.find('.//unpitched') is not None
    assert root.find('.//midi-channel').text == '10'
    # MusicXML unpitched number is MIDI pitch + 1.
    assert root.find('.//midi-unpitched').text == '37'
    midi = tmp_path/'drums.mid'
    score.write('midi', fp=str(midi))
    import mido
    notes = [m for t in mido.MidiFile(midi).tracks for m in t if m.type == 'note_on']
    assert notes and all(m.channel == 9 and m.note == 36 for m in notes)
    assert timeline(score, result['tracks'], sources)['events'][0]['source_event_id'] == 'a'


def test_all_drum_pieces_use_channel_ten(tmp_path):
    from pipeline.exports import fix_drum_channels
    result = {'bpm': 120, 'tracks': [{'id': 'd', 'label': '鼓', 'program': None, 'is_drum': True}],
              'events': [{'id': str(p), 'track_id': 'd', 'start_sec': i*.5, 'end_sec': i*.5+.1,
                          'midi_pitch': p, 'velocity': 90} for i, p in enumerate([36, 38, 42])]}
    score, _ = build_score(result)
    path = tmp_path/'drums.xml'
    score.write('musicxml', fp=str(path))
    fix_drum_channels(path)
    _, root = load_xml(path)
    assert {i.find('midi-channel').text for i in root.findall('.//midi-instrument')} == {'10'}
    assert {i.text for i in root.findall('.//midi-unpitched')} == {'37', '39', '43'}
