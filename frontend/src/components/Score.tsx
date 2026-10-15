import { useEffect, useRef, useState } from 'react';
import type { OpenSheetMusicDisplay } from 'opensheetmusicdisplay';
import type { Artifact, TimelineEvent } from '../lib/types';
import { artifactResponse, errorMessage } from '../lib/api';
import { Loading } from './Common';
interface CursorStep { quarter: number; index: number }
export function Score({ artifact, position }: { artifact: Artifact | undefined; position: TimelineEvent | null }) {
  const container = useRef<HTMLDivElement>(null); const instance = useRef<OpenSheetMusicDisplay | null>(null);
  const steps = useRef<CursorStep[]>([]); const cursorIndex = useRef(0); const current = useRef(position); current.current = position;
  const [zoom, setZoom] = useState(1); const zoomRef = useRef(zoom); zoomRef.current = zoom;
  const [loading, setLoading] = useState(false); const [error, setError] = useState<string | null>(null); const [syncError, setSyncError] = useState<string | null>(null); const [retry, setRetry] = useState(0); const [version, setVersion] = useState(0);
  function locate(event: TimelineEvent | null) {
    const osmd = instance.current; if (!osmd) return;
    if (!event || !steps.current.length) { osmd.cursor.hide(); return; }
    const q = event.score_position.quarter; let low = 0, high = steps.current.length - 1, target = 0;
    while (low <= high) { const mid = (low + high) >>> 1; if (steps.current[mid].quarter <= q + 0.00001) { target = mid; low = mid + 1; } else high = mid - 1; }
    const index = steps.current[target].index; const cursor = osmd.cursor;
    if (cursorIndex.current > index || cursor.Iterator.EndReached) { cursor.reset(); cursorIndex.current = 0; }
    while (cursorIndex.current < index && !cursor.Iterator.EndReached) { cursor.Iterator.moveToNextVisibleVoiceEntry(false); cursorIndex.current++; }
    cursor.show(); cursor.update();
  }
  useEffect(() => {
    const element = container.current; if (!element || !artifact) return;
    let disposed = false; let osmd: OpenSheetMusicDisplay | undefined; let observer: ResizeObserver | undefined; let timer: ReturnType<typeof setTimeout> | undefined;
    const abort = new AbortController(); setLoading(true); setError(null); setSyncError(null); steps.current = [];
    async function load() {
      try {
        const [{ OpenSheetMusicDisplay: OSMD }, response] = await Promise.all([import('opensheetmusicdisplay'), artifactResponse(artifact!.id, false, abort.signal)]);
        const content = await response.blob(); if (disposed) return;
        osmd = new OSMD(element!, { backend: 'svg', autoResize: false, followCursor: false, drawTitle: true, drawComposer: true, pageFormat: 'Endless', disableCursor: false });
        await osmd.load(content); if (disposed) { osmd.clear(); return; }
        osmd.Zoom = zoomRef.current; osmd.render(); instance.current = osmd;
        try {
          // OSMD timestamps use whole notes; contract quarters are quarter notes. Source timestamps keep written-score positions even for repeats.
          const cursor = osmd.cursor; cursor.reset(); const seen = new Set<number>(); const result: CursorStep[] = [];
          let index = 0;
          while (!cursor.Iterator.EndReached && index < 200_000) {
            const quarter = cursor.Iterator.CurrentSourceTimestamp.RealValue * 4;
            if (Number.isFinite(quarter) && !seen.has(quarter)) { seen.add(quarter); result.push({ quarter, index }); }
            cursor.Iterator.moveToNextVisibleVoiceEntry(false); index++;
          }
          if (index >= 200_000) throw new Error('谱面结构过于复杂，无法建立可靠游标索引。');
          steps.current = result.sort((a, b) => a.quarter - b.quarter); cursor.reset(); cursor.hide(); cursorIndex.current = 0;
        } catch (e) { setSyncError(`谱面可以查看，但游标定位不可用：${errorMessage(e)}`); }
        setVersion(v => v + 1); setLoading(false);
        if (typeof ResizeObserver !== 'undefined') { let width = element!.clientWidth; observer = new ResizeObserver(() => { if (disposed || Math.abs(width - element!.clientWidth) < 2) return; width = element!.clientWidth; clearTimeout(timer); timer = setTimeout(() => { try { osmd!.render(); cursorIndex.current = 0; osmd!.cursor.reset(); locate(current.current); } catch (e) { setSyncError(errorMessage(e)); } }, 180); }); observer.observe(element!); }
      } catch (e) { if (!disposed) { setError(`谱面加载失败：${errorMessage(e)}`); setLoading(false); } }
    }
    void load();
    return () => { disposed = true; abort.abort(); observer?.disconnect(); clearTimeout(timer); if (instance.current === osmd) instance.current = null; osmd?.clear(); element.replaceChildren(); };
  }, [artifact?.id, retry]);
  useEffect(() => { const osmd = instance.current; if (!osmd) return; try { osmd.Zoom = zoom; osmd.render(); osmd.cursor.reset(); cursorIndex.current = 0; locate(current.current); } catch (e) { setSyncError(errorMessage(e)); } }, [zoom]);
  useEffect(() => { try { locate(position); } catch (e) { setSyncError(`游标暂不可定位：${errorMessage(e)}`); instance.current?.cursor.hide(); } }, [position, version]);
  return <><div className="score-tools"><span className="muted small">纵向阅读 · 只读谱面 · 拍 / 小节近似同步</span><div className="actions"><button aria-label="缩小乐谱" disabled={zoom <= 0.5} onClick={() => setZoom(z => Math.max(0.5, z - 0.1))}>−</button><span className="zoom-value">{Math.round(zoom * 100)}%</span><button aria-label="放大乐谱" disabled={zoom >= 2} onClick={() => setZoom(z => Math.min(2, z + 0.1))}>＋</button><button onClick={() => setZoom(1)}>重置</button></div></div>{loading && <Loading>正在加载本地乐谱渲染器…</Loading>}{error && <div className="notice danger" role="alert">{error}<button className="text-button" onClick={() => setRetry(v => v + 1)}>重新加载谱面</button></div>}{syncError && <div className="notice warning">{syncError}</div>}<div className="score-position" role="status">{position ? `定位：第 ${position.score_position.measure} 小节 · 第 ${position.score_position.beat.toFixed(2).replace(/\.00$/, '')} 拍（近似）` : '当前没有可映射的谱面事件'}</div><div className="score-scroll"><div ref={container} className="score-canvas" /></div></>;
}
