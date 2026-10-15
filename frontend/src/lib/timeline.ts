import type { AudioMode, Timeline, TimelineEvent } from './types';
export interface TimedEvent { event: TimelineEvent; start: number; end: number }
export function timedEvents(timeline: Timeline | undefined, mode: AudioMode, trackId?: string): TimedEvent[] {
  const selected = (timeline?.events ?? []).filter(event => (!trackId || event.track_id === trackId) && Number.isFinite(event?.score_position?.quarter));
  const startOf = (event: TimelineEvent) => mode === 'source' ? event.source_start_sec : event.synth_start_sec;
  const endOf = (event: TimelineEvent) => mode === 'source' ? event.source_end_sec : event.synth_end_sec;
  const starts = selected.map(startOf);
  return selected.flatMap((event, index) => {
    const start = starts[index];
    if (start == null || !Number.isFinite(start)) return [];
    const declared = endOf(event);
    let end: number | null = declared != null && Number.isFinite(declared) && declared >= start ? declared : null;
    // The contract allows an open final bound; close it with the next known start, falling back to the start itself.
    if (end == null) for (let i = index + 1; i < starts.length; i++) { const next = starts[i]; if (next != null && Number.isFinite(next) && next > start) { end = next; break; } }
    return [{ event, start, end: Math.max(end ?? start, start) }];
  }).sort((a, b) => a.start - b.start || a.event.score_position.quarter - b.event.score_position.quarter);
}
export function positionAt(events: TimedEvent[], seconds: number): TimelineEvent | null {
  let low = 0, high = events.length - 1, index = -1;
  while (low <= high) { const mid = (low + high) >>> 1; if (events[mid].start <= seconds) { index = mid; low = mid + 1; } else high = mid - 1; }
  // During rests retain the preceding known score position. Before the first event there is no fabricated position.
  return index < 0 ? null : events[index].event;
}
export function remapTime(timeline: Timeline | undefined, seconds: number, from: AudioMode, to: AudioMode): number {
  if (from === to || !timeline) return seconds;
  const anchors = timeline.events.flatMap(event => event.source_start_sec == null || event.source_end_sec == null ? [] : [
    { source: event.source_start_sec, synth: event.synth_start_sec },
    { source: event.source_end_sec, synth: event.synth_end_sec },
  ]).filter(a => Number.isFinite(a.source) && Number.isFinite(a.synth)).sort((a, b) => a[from] - b[from]);
  if (!anchors.length) return seconds;
  const exact = anchors.find(a => Math.abs(a[from] - seconds) < 0.0001);
  if (exact) return Math.max(0, exact[to]);
  let low = anchors[0], high = anchors[anchors.length - 1];
  for (let i = 1; i < anchors.length; i++) { if (anchors[i][from] > seconds) { high = anchors[i]; low = anchors[i - 1]; break; } }
  if (seconds < anchors[0][from]) return Math.max(0, seconds + anchors[0][to] - anchors[0][from]);
  if (seconds > anchors[anchors.length - 1][from]) { const last = anchors[anchors.length - 1]; return Math.max(0, seconds + last[to] - last[from]); }
  const span = high[from] - low[from];
  return Math.max(0, span > 0 ? low[to] + (seconds - low[from]) / span * (high[to] - low[to]) : low[to]);
}
export function validTimeline(value: unknown): value is Timeline {
  if (!value || typeof value !== 'object') return false;
  const data = value as Timeline;
  return data.version === 1 && Array.isArray(data.events) && Array.isArray(data.warnings) && data.events.every(e =>
    e && typeof e.track_id === 'string' && e.score_position && Number.isFinite(e.score_position.quarter) &&
    Number.isFinite(e.synth_start_sec) && Number.isFinite(e.synth_end_sec) &&
    (e.source_start_sec == null || Number.isFinite(e.source_start_sec)) && (e.source_end_sec == null || Number.isFinite(e.source_end_sec)));
}
