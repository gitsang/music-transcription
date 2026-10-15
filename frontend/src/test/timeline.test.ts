import { describe, expect, it } from 'vitest';
import { positionAt, remapTime, timedEvents, validTimeline } from '../lib/timeline';
import type { Timeline } from '../lib/types';

const event = (id: string, track: string, quarter: number, source: [number | null, number | null], synth: [number, number]) => ({
  id, track_id: track, score_position: { measure: 1, beat: 1, quarter },
  source_start_sec: source[0], source_end_sec: source[1], synth_start_sec: synth[0], synth_end_sec: synth[1],
});
const timeline: Timeline = {
  version: 1, initial_offset_sec: 0, warnings: [],
  tempo_map: [{ quarter: 0, bpm: 120 }],
  events: [event('a', 't1', 0, [0, 2], [0, 1]), event('b', 't1', 1, [2, 4], [1, 2]), event('c', 't2', 2, [4, null], [2, 3])],
};

describe('timedEvents', () => {
  it('uses source bounds in source mode and synth bounds in synth mode', () => {
    expect(timedEvents(timeline, 'source', 't1').map(e => [e.start, e.end])).toEqual([[0, 2], [2, 4]]);
    expect(timedEvents(timeline, 'synth', 't1').map(e => [e.start, e.end])).toEqual([[0, 1], [1, 2]]);
  });
  it('drops the single event of a track without source timestamps', () => {
    // "c" has no source_end_sec and there is no later source start to close it, so its synthetic end is itself.
    expect(timedEvents(timeline, 'source', 't2').map(e => [e.start, e.end])).toEqual([[4, 4]]);
  });
  it('closes an open final source event with the next known start', () => {
    expect(timedEvents(timeline, 'source').at(-1)!.end).toBe(4);
  });
  it('tolerates a timeline without any events', () => {
    expect(timedEvents(undefined, 'source')).toEqual([]);
  });
});

describe('positionAt', () => {
  it('returns the preceding event during a rest and null before the first event', () => {
    const events = timedEvents(timeline, 'synth');
    expect(positionAt(events, -1)).toBeNull();
    expect(positionAt(events, 0.5)!.id).toBe('a');
    expect(positionAt(events, 1.9)!.id).toBe('b');
    expect(positionAt(events, 99)!.id).toBe('c');
  });
});

describe('remapTime', () => {
  it('interpolates between source and synth anchors', () => {
    const value = remapTime(timeline, 1, 'source', 'synth');
    expect(value).toBeGreaterThan(0); expect(value).toBeLessThan(1.2);
  });
  it('is an identity without a timeline or same mode', () => {
    expect(remapTime(undefined, 3, 'source', 'synth')).toBe(3);
    expect(remapTime(timeline, 3, 'synth', 'synth')).toBe(3);
  });
});

describe('validTimeline', () => {
  it('accepts a well-formed timeline and rejects malformed payloads', () => {
    expect(validTimeline(timeline)).toBe(true);
    expect(validTimeline({ version: 2, events: [], warnings: [] })).toBe(false);
    expect(validTimeline({ version: 1, warnings: [], events: [{ track_id: 't', score_position: { quarter: NaN }, synth_start_sec: 0, synth_end_sec: 1 }] })).toBe(false);
    expect(validTimeline(null)).toBe(false);
  });
});
