import type { Artifact, AudioSource, Job } from './types';

// Browsers reliably decode these containers; anything else is offered as a download only.
const playableMimes = new Set(['audio/mpeg', 'audio/mp3', 'audio/wav', 'audio/x-wav', 'audio/wave', 'audio/ogg', 'audio/webm', 'audio/mp4', 'audio/aac', 'audio/m4a']);
export function browserPlayable(artifact: Artifact | undefined): artifact is Artifact {
  return !!artifact && artifact.status === 'ready' && playableMimes.has((artifact.mime ?? '').split(';')[0].trim().toLowerCase());
}
/** Prefer the MP3 preview; fall back to the archive file only when the browser can decode it (FLAC stems cannot). */
function choosePreview(ready: Artifact[], previewKind: Artifact['kind'], archiveKind: Artifact['kind']): { artifact: Artifact; detail: string } | null {
  const preview = ready.find(a => a.kind === previewKind);
  if (preview) return { artifact: preview, detail: '试听转码（MP3）' };
  const archive = ready.find(a => a.kind === archiveKind);
  return browserPlayable(archive) ? { artifact: archive, detail: '原始文件（浏览器可直接播放）' } : null;
}
export function stemGroups(artifacts: Artifact[]) {
  const groups = new Map<string, Artifact[]>();
  for (const artifact of artifacts.filter(a => a.kind === 'stem' || a.kind === 'stem_preview')) {
    const key = artifact.track_id ?? artifact.id;
    groups.set(key, [...(groups.get(key) ?? []), artifact]);
  }
  return [...groups.entries()].map(([id, files]) => ({ id, files, label: files.find(a => a.kind === 'stem')?.label ?? files[0].label }));
}
export function audioSources(job: Job): AudioSource[] {
  const ready = job.artifacts.filter(a => a.status === 'ready');
  const result: AudioSource[] = [];
  if (job.kind === 'audio') {
    // original_preview is a browser-safe transcode and must win over the stored original, which may be FLAC.
    const original = choosePreview(ready, 'original_preview', 'original');
    if (original) result.push({ id: original.artifact.id, label: '原曲', role: 'original', detail: original.detail, mode: 'source', artifact: original.artifact });
  }
  const synth = choosePreview(ready, 'synth_preview', 'synth');
  if (synth) result.push({ id: synth.artifact.id, label: '合成演奏', role: 'synth', detail: synth.detail, mode: 'synth', artifact: synth.artifact });
  if (job.kind === 'audio') for (const group of stemGroups(job.artifacts)) {
    const picked = choosePreview(group.files.filter(a => a.status === 'ready'), 'stem_preview', 'stem');
    if (picked) result.push({ id: picked.artifact.id, label: `分离音轨 · ${group.label}`, role: 'stem', trackId: group.id, detail: picked.detail, mode: 'source', artifact: picked.artifact });
  }
  return result;
}
/** Ready audio artifacts the browser cannot decode and that have no preview transcode, e.g. a raw FLAC original. */
export function unplayableAudio(job: Job): Artifact[] {
  if (job.kind !== 'audio') return []; // a MusicXML job's "original" is the uploaded score, not audio
  const playable = new Set(audioSources(job).map(s => s.artifact.id));
  return job.artifacts.filter(a => a.status === 'ready'
    && (a.kind === 'original' || a.kind === 'stem' || a.kind === 'synth' || a.kind === 'original_preview' || a.kind === 'stem_preview' || a.kind === 'synth_preview')
    && !playable.has(a.id));
}
