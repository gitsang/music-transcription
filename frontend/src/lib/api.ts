import type { Capabilities, Job, Settings, User } from './types';

const prefix = '/api/v1';
const explanations: Record<string, string> = {
  ACCOUNT_PENDING: '账号正在等待管理员审核，批准后才能登录。',
  ACCOUNT_REJECTED: '账号申请已被拒绝，请联系管理员。',
  ACCOUNT_DISABLED: '账号已停用，会话不再有效，请联系管理员。',
  JOB_LIMIT_REACHED: '未完成任务已达上限，请等待现有任务完成或取消后再试。',
  AUDIO_TOO_LONG: '音频时长超过系统限制，请上传较短的音频。',
  MODEL_NOT_READY: '本地模型尚未准备好，暂时不能创建此音频任务。',
  ARTIFACT_NOT_AVAILABLE: '该文件尚未生成或已不可用。',
  INVALID_CREDENTIALS: '用户名或密码不正确。',
  ORIGIN_NOT_ALLOWED: '请求来源不被允许，请检查同源部署和 PUBLIC_ORIGIN 配置。',
  ORIGIN_FORBIDDEN: '请求来源不被允许，请检查同源部署和 PUBLIC_ORIGIN 配置。',
  USERNAME_TAKEN: '该用户名已被注册，请换一个。',
  INVALID_USERNAME: '用户名格式不符合要求。',
  INVALID_PASSWORD: '密码格式不符合要求。',
  RATE_LIMITED: '操作过于频繁，请稍后再试。',
  AUTH_REQUIRED: '请先登录。',
  FORBIDDEN: '没有此操作或文件的访问权限。',
  NOT_FOUND: '资源不存在、已被删除，或你没有访问权限。',
  ADMIN_PROTECTED: '管理员账号只能通过运维流程管理。',
  JOB_NOT_RETRYABLE: '任务当前状态不可重试（可能仍在运行）。',
  INPUT_NOT_AVAILABLE: '原始输入文件已不存在，无法重试。',
  ENGINE_NOT_AVAILABLE: '所选引擎尚未验证可用，请改用已就绪的引擎。',
  UPLOAD_TOO_LARGE: '文件超过上传大小限制。',
  INVALID_INPUT: '上传内容无效，请检查文件。',
  INVALID_AUDIO: '音频无法解码，或采样率、声道不受支持。',
  INVALID_MUSICXML: '无法解析该 MusicXML，请检查乐谱内容。',
  NO_NOTES_DETECTED: '未识别出可生成乐谱的音符，无法生成谱面。',
  SOUNDFONT_NOT_READY: '本地音色库缺失，暂时无法生成合成演奏。',
  TOOL_NOT_READY: '本地导出工具未就绪，部分产物不可用。',
  DEVICE_NOT_AVAILABLE: '指定推理设备不可用，请检查部署配置。',
  DISK_LOW: '服务器磁盘空间不足，暂时不能接受新任务。',
  STAGE_TIMEOUT: '处理阶段超时，任务已停止。',
  PROCESSING_ERROR: '处理过程中出现错误。',
  EMPTY_ARTIFACT: '本地工具未生成有效文件。',
  INVALID_RANGE: '无效的字节范围请求。',
  JOB_DELETING: '任务正在删除。',
};
export class ApiError extends Error {
  constructor(public status: number, public code: string, message: string) { super(explanations[code] ?? message); this.name = 'ApiError'; }
}
export function errorMessage(error: unknown): string { return error instanceof Error ? error.message : '请求失败，请稍后重试。'; }
// Same-origin URLs only. Browsers set Origin on writes; do not override it or persist session tokens.
export async function request<T>(path: string, init: RequestInit = {}): Promise<T> {
  const headers = new Headers(init.headers);
  if (typeof init.body === 'string') headers.set('Content-Type', 'application/json');
  let response: Response;
  try { response = await fetch(`${prefix}${path}`, { ...init, credentials: 'include', headers }); }
  catch (error) { if (error instanceof DOMException && error.name === 'AbortError') throw error; throw new Error('无法连接服务，请检查网络或服务是否启动。'); }
  if (!response.ok) {
    const body = await response.json().catch(() => null);
    const code = body?.error?.code ?? `HTTP_${response.status}`;
    const fallback = response.status === 401 ? '登录已失效，请重新登录。' : response.status === 403 ? '没有此操作或文件的访问权限。' : response.status === 404 ? '资源不存在或没有访问权限。' : `请求失败（${response.status}），请稍后重试。`;
    if (response.status === 401 && !path.startsWith('/auth/')) window.dispatchEvent(new Event('session-expired'));
    throw new ApiError(response.status, code, body?.error?.message ?? fallback);
  }
  if (response.status === 204) return undefined as T;
  return response.json().catch(() => undefined) as Promise<T>;
}
export async function artifactResponse(id: string, download = false, signal?: AbortSignal): Promise<Response> {
  const response = await fetch(artifactUrl(id, download), { credentials: 'include', signal });
  if (!response.ok) {
    const body = await response.json().catch(() => null);
    if (response.status === 401) window.dispatchEvent(new Event('session-expired'));
    throw new ApiError(response.status, body?.error?.code ?? `HTTP_${response.status}`, body?.error?.message ?? '文件无法读取；可能未生成、已过期或没有访问权限。');
  }
  return response;
}
export const artifactUrl = (id: string, download = false) => `${prefix}/artifacts/${encodeURIComponent(id)}/${download ? 'download' : 'content'}`;
export const api = {
  me: () => request<User>('/auth/me'),
  login: (username: string, password: string) => request<User>('/auth/login', { method: 'POST', body: JSON.stringify({ username, password }) }),
  register: (username: string, password: string) => request<{ status: 'pending' }>('/auth/register', { method: 'POST', body: JSON.stringify({ username, password }) }),
  logout: () => request<void>('/auth/logout', { method: 'POST' }),
  capabilities: () => request<Capabilities>('/capabilities'),
  jobs: (admin = false) => request<Job[]>(admin ? '/admin/jobs' : '/jobs'),
  job: (id: string) => request<Job>(`/jobs/${encodeURIComponent(id)}`),
  create: (body: FormData) => request<Job>('/jobs', { method: 'POST', body }),
  jobAction: (id: string, action: 'retry' | 'cancel' | 'delete') => request<Job | void>(`/jobs/${encodeURIComponent(id)}${action === 'delete' ? '' : `/${action}`}`, { method: action === 'delete' ? 'DELETE' : 'POST' }),
  users: () => request<User[]>('/admin/users'),
  userAction: (id: string, action: 'approve' | 'reject' | 'disable') => request<void>(`/admin/users/${encodeURIComponent(id)}/${action}`, { method: 'POST' }),
  settings: () => request<Settings>('/admin/settings'),
  saveSettings: (settings: Settings) => request<Settings>('/admin/settings', { method: 'PATCH', body: JSON.stringify(settings) }),
};
