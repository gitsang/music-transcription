import { describe, expect, it } from 'vitest';
import { audioSources, browserPlayable, stemGroups, unplayableAudio } from '../lib/audio';
import type { Artifact, ArtifactKind, Job } from '../lib/types';

let seq = 0;
const artifact = (kind: ArtifactKind, mime: string, extra: Partial<Artifact> = {}): Artifact =>
  ({ id: `a${seq++}`, track_id: null, kind, label: kind, mime, bytes: 1, status: 'ready', error: null, ...extra });
const job = (kind: Job['kind'], artifacts: Artifact[]): Job => ({
  id: 'j', user_id: 'u', title: 'song', kind, status: 'succeeded', stage: null, stage_progress: null,
  engine: 'baseline', actual_device: 'cpu', config_snapshot: {}, warnings: [], error_code: null,
  error_message: null, attempt: 1, created_at: '', started_at: null, completed_at: null, expires_at: null,
  tracks: [], artifacts,
});

describe('browserPlayable', () => {
  it('accepts common audio containers and rejects FLAC and failed artifacts', () => {
    expect(browserPlayable(artifact('original', 'audio/mpeg'))).toBe(true);
    expect(browserPlayable(artifact('stem', 'audio/wav'))).toBe(true);
    expect(browserPlayable(artifact('stem', 'audio/flac'))).toBe(false);
    expect(browserPlayable(artifact('original', 'audio/flac'))).toBe(false);
    expect(browserPlayable(artifact('original', 'audio/mpeg', { status: 'failed' }))).toBe(false);
  });
});

describe('audioSources', () => {
  it('prefers the MP3 original_preview over a FLAC original', () => {
    const sources = audioSources(job('audio', [artifact('original', 'audio/flac'), artifact('original_preview', 'audio/mpeg')]));
    expect(sources.map(s => s.role)).toEqual(['original']);
    expect(sources[0].artifact.kind).toBe('original_preview');
    expect(sources[0].detail).toContain('MP3');
  });
  it('falls back to a browser-playable original when no preview exists', () => {
    const sources = audioSources(job('audio', [artifact('original', 'audio/mpeg')]));
    expect(sources).toHaveLength(1);
    expect(sources[0].artifact.kind).toBe('original');
  });
  it('offers neither original nor stems for MusicXML imports', () => {
    expect(audioSources(job('musicxml', [artifact('original', 'audio/mpeg'), artifact('stem', 'audio/wav')]))).toEqual([]);
  });
  it('adds synth and one entry per stem, ignoring failed files', () => {
    const sources = audioSources(job('audio', [
      artifact('synth_preview', 'audio/mpeg'),
      artifact('stem_preview', 'audio/mpeg', { track_id: 't1', label: 'vocals 分离试听' }),
      artifact('stem', 'audio/flac', { track_id: 't1', label: 'vocals 分离原始音频' }),
      artifact('stem', 'audio/wav', { track_id: 't2', label: 'drums 分离原始音频' }),
      artifact('stem_preview', 'audio/mpeg', { track_id: 't2', status: 'failed', error: '导出失败' }),
    ]));
    expect(sources.map(s => [s.role, s.trackId ?? null])).toEqual([['synth', null], ['stem', 't1'], ['stem', 't2']]);
  });
});

describe('stemGroups', () => {
  it('groups stem and preview artifacts by track so no fake instrument audio is invented', () => {
    const groups = stemGroups([artifact('stem', 'audio/wav', { track_id: 't1', label: 'bass' }), artifact('stem_preview', 'audio/mpeg', { track_id: 't1', label: 'bass preview' })]);
    expect(groups).toHaveLength(1);
    expect(groups[0].label).toBe('bass');
  });
});

describe('unplayableAudio', () => {
  it('lists undecodable ready audio that has no preview', () => {
    const value = unplayableAudio(job('audio', [artifact('original', 'audio/flac'), artifact('original_preview', 'audio/mpeg')]));
    expect(value.map(a => a.kind)).toEqual(['original']);
  });
  it('never treats a MusicXML import as unplayable audio', () => {
    expect(unplayableAudio(job('musicxml', [artifact('original', 'application/vnd.recordare.musicxml+xml')]))).toEqual([]);
  });
});
