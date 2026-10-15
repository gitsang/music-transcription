import type { ArtifactKind, JobStatus, Stage } from './types';
export const statusLabels: Record<JobStatus, string> = { queued: '排队中', running: '处理中', succeeded: '已完成', partial: '部分成功', failed: '失败', cancelled: '已取消' };
export const stageLabels: Record<Stage, string> = { validating: '校验输入', preparing: '准备音频', analyzing: '分析节拍', separating: '音源分离', transcribing: '识别音符', scoring: '生成谱面', exporting: '导出与合成', finalizing: '校验结果' };
export const artifactLabels: Record<ArtifactKind, string> = { original: '原始文件', original_preview: '原曲试听', score: 'MusicXML 总谱', part: 'MusicXML 分谱', midi: 'MIDI', stem: '分离音轨', stem_preview: '分轨试听', synth: '合成音频', synth_preview: '合成试听', pdf: 'PDF 总谱', part_pdf: 'PDF 分谱', timeline: '播放时间线', notes: '音符事件', manifest: '产物清单' };
export function date(value?: string | null) { return value ? new Intl.DateTimeFormat('zh-CN', { dateStyle: 'medium', timeStyle: 'short' }).format(new Date(value)) : '—'; }
export function bytes(value?: number | null) { return value == null ? '—' : value < 1024 * 1024 ? `${(value / 1024).toFixed(1)} KB` : `${(value / 1024 / 1024).toFixed(1)} MB`; }
export function time(value: number) { const n = Math.max(0, Math.floor(Number.isFinite(value) ? value : 0)); return `${Math.floor(n / 60)}:${String(n % 60).padStart(2, '0')}`; }
