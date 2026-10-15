import { useCallback, useEffect, useRef, useState } from 'react';
import { artifactUrl, errorMessage } from './api';
import { remapTime } from './timeline';
import type { AudioSource, Timeline } from './types';

/** One HTMLAudioElement for the whole detail workspace: original, synth and every stem share it, so two sources can never play at once. */
export function useTransport(source: AudioSource | undefined, timeline: Timeline | undefined) {
  const audioRef = useRef<HTMLAudioElement | null>(null);
  const previous = useRef<AudioSource | undefined>(undefined);
  const pendingTime = useRef(0);
  const generation = useRef(0);
  const [currentTime, setTime] = useState(0);
  const [duration, setDuration] = useState(0);
  const [playing, setPlaying] = useState(false);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [volume, setVolume] = useState(0.8);
  const [reload, setReload] = useState(0);
  const timelineRef = useRef(timeline); timelineRef.current = timeline;

  useEffect(() => {
    const audio = new Audio();
    audio.preload = 'metadata';
    audio.crossOrigin = 'use-credentials'; // same-origin cookie session must be sent for private artifacts
    audio.volume = 0.8;
    audioRef.current = audio;
    const update = () => setTime(audio.currentTime);
    const metadata = () => {
      const known = Number.isFinite(audio.duration);
      setDuration(known ? audio.duration : 0);
      // Apply the position remembered across a source switch as soon as the new media is seekable.
      try { audio.currentTime = known ? Math.min(pendingTime.current, audio.duration) : pendingTime.current; } catch { setError('此音源暂不支持定位播放。'); }
      update(); setLoading(false);
    };
    const failure = () => {
      setLoading(false); setPlaying(false);
      const code = audio.error?.code;
      setError(code === 4 ? '浏览器无法播放此格式或文件不可用；请下载原文件，或选择试听音频。' : code === 3 ? '音频解码失败，请尝试试听格式或下载文件。' : '音频加载失败，请检查网络、登录权限与文件是否仍然可用。');
    };
    const onPlay = () => setPlaying(true);
    const onPause = () => { setPlaying(false); update(); };
    const ended = () => { setPlaying(false); update(); };
    const waiting = () => setLoading(true);
    const ready = () => setLoading(false);
    audio.addEventListener('timeupdate', update);
    audio.addEventListener('loadedmetadata', metadata);
    audio.addEventListener('error', failure);
    audio.addEventListener('play', onPlay);
    audio.addEventListener('pause', onPause);
    audio.addEventListener('ended', ended);
    audio.addEventListener('waiting', waiting);
    audio.addEventListener('canplay', ready);
    audio.addEventListener('seeked', update);
    return () => {
      generation.current++; audio.pause(); audio.removeAttribute('src'); audio.load(); audioRef.current = null;
      audio.removeEventListener('timeupdate', update);
      audio.removeEventListener('loadedmetadata', metadata);
      audio.removeEventListener('error', failure);
      audio.removeEventListener('play', onPlay);
      audio.removeEventListener('pause', onPause);
      audio.removeEventListener('ended', ended);
      audio.removeEventListener('waiting', waiting);
      audio.removeEventListener('canplay', ready);
      audio.removeEventListener('seeked', update);
    };
  }, []);

  useEffect(() => {
    const audio = audioRef.current; if (!audio) return;
    generation.current++;
    const old = previous.current;
    // Preserve the audible position across modes via the timeline anchors; never restart from zero by accident.
    const time = old && source ? remapTime(timelineRef.current, audio.currentTime, old.mode, source.mode) : pendingTime.current;
    audio.pause();
    pendingTime.current = Math.max(0, time);
    setPlaying(false); setTime(pendingTime.current); setDuration(0); setError(null);
    previous.current = source;
    if (!source) { audio.removeAttribute('src'); audio.load(); setLoading(false); return; }
    setLoading(true); audio.src = artifactUrl(source.artifact.id); audio.load();
    // Deliberately never auto-resume: the old source stopped before the new URL was assigned.
  }, [source?.id, reload]);

  useEffect(() => { if (audioRef.current) audioRef.current.volume = volume; }, [volume]);

  useEffect(() => {
    if (!playing) return;
    let frame = 0; let last = 0;
    const tick = (now: number) => { if (now - last >= 80) { const audio = audioRef.current; if (audio) setTime(audio.currentTime); last = now; } frame = requestAnimationFrame(tick); };
    frame = requestAnimationFrame(tick);
    return () => cancelAnimationFrame(frame);
  }, [playing]);

  const toggle = useCallback(async () => {
    const audio = audioRef.current; if (!audio || !source) return;
    if (!audio.paused) { audio.pause(); return; }
    const token = generation.current; setError(null);
    try { await audio.play(); }
    catch (e) {
      if (token !== generation.current) return;
      setPlaying(false); setLoading(false);
      setError(e instanceof DOMException && e.name === 'NotAllowedError' ? '浏览器阻止了自动播放，请再次点击播放。' : `播放失败：${errorMessage(e)}`);
    }
  }, [source]);

  const seek = useCallback((seconds: number) => {
    const audio = audioRef.current; if (!audio) return;
    const maximum = Number.isFinite(audio.duration) && audio.duration > 0 ? audio.duration : seconds;
    const target = Math.max(0, Math.min(seconds, maximum));
    pendingTime.current = target; setTime(target);
    if (audio.readyState >= 1) { try { audio.currentTime = target; } catch { setError('此音源无法定位到所选时间。'); } }
  }, []);

  const stop = useCallback(() => { const audio = audioRef.current; if (audio) audio.pause(); seek(0); setPlaying(false); }, [seek]);
  return { currentTime, duration, playing, loading, error, volume, setVolume, toggle, seek, stop, retry: () => setReload(v => v + 1) };
}
