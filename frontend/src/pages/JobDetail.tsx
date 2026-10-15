import { useMemo, useState } from 'react';
import { Link, useParams } from 'react-router-dom';
import { useQuery } from '@tanstack/react-query';
import { api, artifactResponse } from '../lib/api';
import { artifactLabels, date, stageLabels } from '../lib/format';
import { isActive, pollInterval, usePageHidden } from '../lib/query';
import { positionAt, timedEvents, validTimeline } from '../lib/timeline';
import { audioSources, stemGroups } from '../lib/audio';
import { useTransport } from '../lib/useTransport';
import type { Job, Track } from '../lib/types';
import { Empty, ErrorNotice, Loading, PageHeading, StatusBadge } from '../components/Common';
import { JobActions } from '../components/JobActions';
import { Transport } from '../components/Transport';
import { Score } from '../components/Score';
import { Artifacts } from '../components/Artifacts';
import { useAuth } from '../components/Auth';

export function JobDetail() {
  const { id = '' } = useParams(); const hidden = usePageHidden();
  const query = useQuery({ queryKey: ['job', id], queryFn: () => api.job(id), refetchInterval: q => pollInterval(q.state.data ? isActive(q.state.data) : false, hidden), refetchIntervalInBackground: true });
  if (query.isPending) return <Loading>正在恢复任务状态…</Loading>;
  if (!query.data) return <><Link to="/jobs" className="text-link">← 返回任务</Link><PageHeading eyebrow="任务详情" title="无法读取此任务">资源不存在、已被删除，或你没有访问权限。</PageHeading><ErrorNotice error={query.error} retry={() => void query.refetch()} /></>;
  return <><ErrorNotice error={query.error} retry={() => void query.refetch()} /><JobWorkspace key={`${id}:${query.data.attempt}`} job={query.data} /></>;
}

const labelSources: Record<Track['label_source'], string> = { model: '模型分类', separator_category: '分离器粗分类 · 非乐器识别', unknown: '未知类别', imported: '用户导入原谱' };
const isUnknownPart = (t: Track) => t.label_source === 'unknown' || (!t.is_drum && t.program == null && t.label_source !== 'imported');

function JobWorkspace({ job }: { job: Job }) {
  const { user } = useAuth(); const cap = useQuery({ queryKey: ['capabilities'], queryFn: api.capabilities });
  const timelineArtifact = job.artifacts.find(a => a.kind === 'timeline' && a.status === 'ready');
  const timeline = useQuery({ queryKey: ['timeline', timelineArtifact?.id], enabled: !!timelineArtifact, queryFn: async ({ signal }) => { const response = await artifactResponse(timelineArtifact!.id, false, signal); const data: unknown = await response.json(); if (!validTimeline(data)) throw new Error('时间线格式不受支持；仍可独立播放和查看谱面。'); return data; }, staleTime: Infinity });
  const sources = useMemo(() => audioSources(job), [job]);
  const [sourceId, setSourceId] = useState(''); const [trackId, setTrackId] = useState('');
  const source = sources.find(s => s.id === sourceId) ?? sources[0];
  const transport = useTransport(source, timeline.data);
  const events = useMemo(() => timedEvents(timeline.data, source?.mode ?? 'synth', trackId || undefined), [timeline.data, source?.mode, trackId]);
  const position = positionAt(events, transport.currentTime);
  const stems = stemGroups(job.artifacts);
  const engine = cap.data?.engines.find(e => e.id === job.engine);
  const score = job.artifacts.find(a => a.kind === (trackId ? 'part' : 'score') && (!trackId || a.track_id === trackId) && a.status === 'ready');
  const scoreFailure = job.artifacts.find(a => a.kind === (trackId ? 'part' : 'score') && (!trackId || a.track_id === trackId) && a.status === 'failed');
  const back = user?.role === 'admin' && job.user_id !== user.id ? '/admin/jobs' : '/jobs';
  const summary = job.status === 'queued' ? '等待后台处理' : job.status === 'running' ? (job.stage ? stageLabels[job.stage] : '正在处理') : job.status === 'partial' ? '谱面与部分产物已生成' : job.status === 'failed' ? '任务处理失败' : job.status === 'cancelled' ? '任务已取消' : '处理完成';
  return <>
    <Link className="text-link breadcrumb" to={back}>← 返回任务列表</Link>
    <PageHeading eyebrow={`${job.kind === 'audio' ? '音频转谱' : 'MusicXML 导入'} / 任务详情`} title={job.title} action={<StatusBadge status={job.status} />}>{job.username && `所属用户：${job.username} · `}第 {job.attempt} 次尝试 · 创建于 {date(job.created_at)}</PageHeading>
    <section className="panel job-progress"><div><h2>{summary}</h2><p className="muted">{isActive(job) ? '没有准确完成时间承诺。离开或刷新页面不会中断后台处理；队列非抢占运行。' : '所有已发布产物在下方逐项展示；谱面与音频可独立使用。'}</p>{job.status === 'running' && job.stage_progress != null && <div className="measured-progress"><progress max="1" value={Math.min(1, Math.max(0, job.stage_progress))} /><span>当前阶段 {Math.round(Math.min(1, Math.max(0, job.stage_progress)) * 100)}%</span></div>}{job.error_message && <div className="notice danger" role="alert"><strong>{job.error_code ?? '处理错误'}</strong> {job.error_message}</div>}<div className="job-meta"><span>开始：{date(job.started_at)}</span><span>完成：{date(job.completed_at)}</span><span>保存：{job.expires_at ? `至 ${date(job.expires_at)}` : job.config_snapshot.retention_days ? `完成后 ${job.config_snapshot.retention_days} 天` : '永久'}</span><span>设备：{job.actual_device ?? '尚未确定'}</span><span>类型：{job.kind === 'audio' ? '音频转谱' : '已有 MusicXML'}</span></div></div><JobActions job={job} /></section>
    <div className="notice warning"><div><strong>{job.kind === 'audio' ? '自动扒谱初稿，不是完整准确还原' : '导入乐谱 · 只读展示'}</strong><p>{job.kind === 'audio' ? '可能存在漏音、错音、节奏及乐器识别误差。未知 / 混合类别不会伪造置信度；本版不提供手动编辑。' : '不执行转谱或分离。复杂反复、跳房子等以实际时间线警告为准；近似游标不承诺逐音符高亮。'}</p><span className="small">引擎：{engine?.label ?? job.engine ?? 'MusicXML 导入'}</span>{job.engine === 'baseline' && <p><strong>Baseline：分离器粗分类，未实现任意乐器 / 全声部识别。</strong></p>}{engine?.limitations.length ? <ul>{engine.limitations.map((l, i) => <li key={i}>{l}</li>)}</ul> : cap.isError && <p>当前无法获取引擎能力；不据此声称支持任意乐器。</p>}</div></div>
    {(job.warnings.length > 0 || (timeline.data?.warnings.length ?? 0) > 0) && <section className="notice warning"><div><strong>处理与播放警告</strong><ul>{[...new Set([...job.warnings, ...(timeline.data?.warnings ?? [])])].map((w, i) => <li key={i}>{w}</li>)}</ul></div></section>}
    <Transport sources={sources} source={source} onSource={setSourceId} transport={transport} job={job} timeline={timeline.data} />
    <ErrorNotice error={timeline.error} retry={() => void timeline.refetch()} />
    <section className="panel score-panel"><div className="panel-heading"><h2>乐谱阅览</h2><label className="inline-label">查看<select aria-label="总谱或分谱" value={trackId} onChange={e => setTrackId(e.target.value)}><option value="">乐队总谱</option>{job.tracks.map(t => <option key={t.id} value={t.id}>{isUnknownPart(t) ? `未知 / 混合乐器 · ${t.label}` : t.label}</option>)}</select></label></div>{score ? <Score key={score.id} artifact={score} position={position} /> : <Empty title={trackId ? '该分谱暂不可用' : '总谱暂不可用'}>{scoreFailure?.error ?? '谱面尚未生成，或此项导出失败。任务详情与其他文件仍可访问。'}</Empty>}</section>
    <div className="result-columns">
      <section className="panel"><div className="panel-heading"><h2>识别声部 <span className="count">{job.tracks.length}</span></h2></div><p className="panel-intro">谱面声部不等同于分离音轨；不会虚构独立乐器的原始音频。选择分谱可在上方查看。</p>{job.tracks.length ? <div className="track-list">{job.tracks.map(t => { const related = stems.filter(s => t.source_stem_ids.includes(s.id) || s.files.some(a => t.source_stem_ids.includes(a.id))); const part = job.artifacts.find(a => a.kind === 'part' && a.track_id === t.id); return <article key={t.id}><div><strong>{isUnknownPart(t) ? `未知 / 混合乐器 · ${t.label}` : t.label}</strong><span className="badge neutral">{t.is_drum ? '打击乐' : t.program == null ? '乐器未确定' : `MIDI program ${t.program}（0–127）`}</span></div><p>{labelSources[t.label_source] ?? t.label_source} · {t.confidence == null ? '未提供置信度' : `模型置信度 ${Math.round(t.confidence * 100)}%`}</p><small className="muted">{related.length ? `关联混合音轨：${related.map(s => s.label).join('、')}（不保证独立分离）` : '暂无对应独立分轨'}</small>{part && (part.status === 'ready' ? <button className="small-button" onClick={() => { setTrackId(t.id); }}>查看此分谱</button> : <small className="error-text">分谱生成失败：{part.error ?? '未知原因'}</small>)}</article>; })}</div> : <Empty title="声部尚未发布">{job.status === 'failed' ? '处理失败，未发布任何声部。' : '排队或处理阶段不会展示不存在的声部。'}</Empty>}</section>
      <section className="panel"><div className="panel-heading"><h2>分离音轨 <span className="count">{stems.length}</span></h2></div><p className="panel-intro">真实分离出的混合音源；只支持单轨试听，不提供实时混音或编辑。</p>{stems.length && job.kind === 'audio' ? <div className="track-list">{stems.map(s => { const playable = sources.find(a => a.role === 'stem' && s.files.some(f => f.id === a.id)); const ready = s.files.some(f => f.status === 'ready'); const errorText = s.files.map(f => f.error).filter(Boolean).join('；'); return <article key={s.id}><strong>{s.label}</strong><p>{ready ? '真实分离音轨 · 粗类别不等于独立乐器' : errorText || '分离失败'}</p><button className="small-button" disabled={!playable} onClick={() => { if (playable) setSourceId(playable.id); }}>{playable ? '选择此音轨（停止当前播放）' : ready ? '浏览器不可直接播放，请下载' : '分离失败'}</button></article>; })}</div> : <Empty title="独立分轨不可用">{job.kind === 'musicxml' ? 'MusicXML 输入不存在原曲或分离音轨。' : '尚未发布分离产物；识别声部不会自动获得假的原始音频。'}</Empty>}</section>
    </div>
    <Artifacts artifacts={job.artifacts} />
    <p className="muted small">产物逐项标注生成状态；失败项不提供下载链接。{artifactLabels.part_pdf} 与分谱一一对应。</p>
  </>;
}
