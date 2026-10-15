import { afterEach, describe, expect, it, vi } from 'vitest';
import { cleanup, render, screen } from '@testing-library/react';
import { Transport } from '../components/Transport';
import { Artifacts } from '../components/Artifacts';
import { audioSources } from '../lib/audio';
import type { Artifact, ArtifactKind, Job } from '../lib/types';

let seq = 0;
const artifact = (kind: ArtifactKind, mime: string, extra: Partial<Artifact> = {}): Artifact =>
  ({ id: `a${seq++}`, track_id: null, kind, label: kind, mime, bytes: 1024, status: 'ready', error: null, ...extra });
const job = (kind: Job['kind'], artifacts: Artifact[]): Job => ({
  id: 'j', user_id: 'u', title: 'song', kind, status: 'succeeded', stage: null, stage_progress: null,
  engine: 'baseline', actual_device: 'cpu', config_snapshot: {}, warnings: [], error_code: null,
  error_message: null, attempt: 1, created_at: '', started_at: null, completed_at: null, expires_at: null,
  tracks: [], artifacts,
});
const transport = {
  currentTime: 0, duration: 0, playing: false, loading: false, error: null, volume: 0.8,
  setVolume: vi.fn(), toggle: vi.fn(), seek: vi.fn(), stop: vi.fn(), retry: vi.fn(),
};
// Vitest does not run with globals here, so register explicit cleanup between renders.
afterEach(cleanup);

describe('Transport', () => {
  it('offers original, synth and stem options for an audio job', () => {
    const value = job('audio', [artifact('original_preview', 'audio/mpeg'), artifact('synth_preview', 'audio/mpeg'), artifact('stem_preview', 'audio/mpeg', { track_id: 't1', label: 'vocals' })]);
    const sources = audioSources(value);
    render(<Transport sources={sources} source={sources[0]} onSource={vi.fn()} transport={transport} job={value} timeline={undefined} />);
    expect((screen.getByLabelText('音源') as HTMLSelectElement).value).toBe(sources[0].id);
    expect(screen.getByText(/时间线不可用/)).toBeTruthy();
  });

  it('flags unreachable audio without preview and explains MusicXML imports', () => {
    const value = job('audio', [artifact('original', 'audio/flac')]);
    render(<Transport sources={[]} source={undefined} onSource={vi.fn()} transport={transport} job={value} timeline={undefined} />);
    expect(screen.getByText(/仅提供下载/)).toBeTruthy();
    expect(screen.getByText(/暂无可用音源/)).toBeTruthy();
  });

  it('surfaces a playback error with a retry control', () => {
    const sources = audioSources(job('audio', [artifact('original_preview', 'audio/mpeg')]));
    render(<Transport sources={sources} source={sources[0]} onSource={vi.fn()} transport={{ ...transport, error: '解码失败' }} job={job('audio', [])} timeline={undefined} />);
    expect(screen.getByRole('alert').textContent).toContain('解码失败');
    expect(screen.getByText('重新加载音源')).toBeTruthy();
  });
});

describe('Artifacts', () => {
  it('shows per-item status and hides downloads for failed artifacts', () => {
    render(<Artifacts artifacts={[artifact('score', 'application/vnd.recordare.musicxml+xml'), artifact('pdf', 'application/pdf', { status: 'failed', error: 'MuseScore 导出失败' })]} />);
    expect(screen.getByText('MusicXML 总谱')).toBeTruthy();
    expect(screen.getByText('MuseScore 导出失败')).toBeTruthy();
    expect(screen.getAllByText('不可下载')).toHaveLength(1);
    expect(screen.getByRole('button', { name: '下载' })).toBeTruthy();
  });
});
