import { useState } from 'react';
import type { Artifact } from '../lib/types';
import { artifactLabels, bytes } from '../lib/format';
import { artifactResponse } from '../lib/api';
import { Empty, ErrorNotice } from './Common';
export function Artifacts({ artifacts }: { artifacts: Artifact[] }) {
  const [busy, setBusy] = useState<string | null>(null); const [error, setError] = useState<unknown>(null);
  async function download(artifact: Artifact) {
    setBusy(artifact.id); setError(null);
    try {
      const response = await artifactResponse(artifact.id, true);
      // Keep server authorization and Content-Disposition, including UTF-8 filenames; use an ephemeral blob URL only.
      const disposition = response.headers.get('Content-Disposition') ?? '';
      const utf8 = /filename\*=UTF-8''([^;]+)/i.exec(disposition)?.[1];
      const plain = /filename="([^"]+)"/i.exec(disposition)?.[1];
      let filename = plain ?? artifact.label; if (utf8) try { filename = decodeURIComponent(utf8); } catch { /* fall back to the supplied display name */ }
      const blob = await response.blob(); const url = URL.createObjectURL(blob); const link = document.createElement('a'); link.href = url; link.download = filename.replace(/[\\/]/g, '_'); link.click(); setTimeout(() => URL.revokeObjectURL(url), 1000);
    } catch (e) { setError(e); } finally { setBusy(null); }
  }
  return <section className="panel"><div className="panel-heading"><h2>文件与导出</h2><span className="muted small">{artifacts.filter(a => a.status === 'ready').length} 项可用 / {artifacts.length} 项产物</span></div><ErrorNotice error={error} />{artifacts.length ? <div className="table-scroll"><table><thead><tr><th>产物</th><th>格式 / 大小</th><th>生成状态</th><th>下载</th></tr></thead><tbody>{artifacts.map(a => <tr key={a.id}><td><strong>{artifactLabels[a.kind] ?? a.kind}</strong><small className="muted">{a.label}</small></td><td><span className="muted small">{a.mime}</span><small>{bytes(a.bytes)}</small></td><td>{a.status === 'ready' ? <span className="badge succeeded">可用</span> : <><span className="badge failed">生成失败</span><small className="error-text">{a.error ?? '此产物生成失败，请查看任务警告或重试。'}</small></>}</td><td>{a.status === 'ready' ? <button className="small-button" disabled={busy !== null} onClick={() => void download(a)}>{busy === a.id ? '读取中…' : '下载'}</button> : <span className="muted">不可下载</span>}</td></tr>)}</tbody></table></div> : <Empty title="产物尚未发布">排队或处理阶段不会显示不存在的下载链接。</Empty>}<div className="panel-foot">失败项不会提供死链接。PDF、合成失败不影响有效 MusicXML 展示。大文件下载会先读取到浏览器内存。</div></section>;
}
