import { useState, type FormEvent } from 'react';
import { Link, Navigate, useLocation, useNavigate } from 'react-router-dom';
import { api } from '../lib/api';
import { queryClient } from '../lib/query';
import { useAuth } from '../components/Auth';
import { ErrorNotice } from '../components/Common';
export function AuthPage({ register = false }: { register?: boolean }) {
  const { user } = useAuth(); const navigate = useNavigate(); const location = useLocation();
  const [username, setUsername] = useState(''); const [password, setPassword] = useState('');
  const [busy, setBusy] = useState(false); const [error, setError] = useState<unknown>(null); const [pending, setPending] = useState(false);
  if (user) return <Navigate to="/jobs" replace />;
  async function submit(event: FormEvent) {
    event.preventDefault(); setBusy(true); setError(null);
    try {
      if (register) { await api.register(username.trim(), password); setPassword(''); setPending(true); }
      else { const result = await api.login(username.trim(), password); await queryClient.cancelQueries(); queryClient.removeQueries({ predicate: query => query.queryKey[0] !== 'me' }); queryClient.setQueryData(['me'], result); setPassword(''); const from: unknown = location.state?.from; navigate(typeof from === 'string' && from.startsWith('/') && !from.startsWith('//') ? from : '/jobs', { replace: true }); }
    } catch (e) { setError(e); } finally { setBusy(false); }
  }
  return <div className="auth-page"><section className="auth-story"><Link className="brand light" to="/login"><span className="brand-mark">♮</span><span>音谱<small>本地音乐转谱</small></span></Link><div><p className="eyebrow">从声音，到可阅读的乐谱</p><h1>让音乐的每个声部<br />都有迹可循。</h1><p>上传歌曲或已有 MusicXML，查看总谱与分谱，<br />对照原曲、合成演奏和真实分离音轨。</p><div className="staff-art" aria-hidden="true">♪ &nbsp; ♫ &nbsp; ♩</div></div><small>自动识别仅生成初稿，可能存在漏音、错音与乐器识别误差。</small></section><section className="auth-form"><div className="auth-card">{pending ? <><span className="badge queued">待审核</span><h2>注册申请已提交</h2><p>等待管理员审核。批准前不能登录，也不会创建有效会话。</p><Link className="button primary" to="/login">返回登录</Link></> : <><p className="eyebrow">{register ? '创建个人工作空间' : '欢迎回来'}</p><h2>{register ? '申请账号' : '登录音谱'}</h2><p className="muted">{register ? '自行注册，管理员审核后启用。' : '使用已审核通过的用户名和密码。'}</p><form onSubmit={submit}><label>用户名<input autoComplete="username" value={username} onChange={e => setUsername(e.target.value)} required maxLength={128} disabled={busy} /></label><label>密码<input type="password" autoComplete={register ? 'new-password' : 'current-password'} value={password} onChange={e => setPassword(e.target.value)} required disabled={busy} /></label><ErrorNotice error={error} /><button className="primary" disabled={busy}>{busy ? '正在提交…' : register ? '提交注册申请' : '登录'}</button></form><p className="auth-switch">{register ? '已有账号？' : '还没有账号？'}<Link to={register ? '/login' : '/register'}>{register ? '去登录' : '申请注册'}</Link></p></>}<div className="privacy-note"><strong>数据与权限说明</strong><p>音频在本地模型中处理，不发送到云端推理。管理员可以查看、播放和下载全部用户文件；这不是端到端私密存储。</p></div></div></section></div>;
}
