import { useState } from 'react';
import { NavLink, Outlet, useNavigate, Link } from 'react-router-dom';
import { useAuth } from './Auth';
import { api } from '../lib/api';
import { queryClient } from '../lib/query';
import { ErrorNotice } from './Common';
export function Layout() {
  const { user } = useAuth(); const navigate = useNavigate();
  const [busy, setBusy] = useState(false); const [error, setError] = useState<unknown>(null);
  async function logout() { setBusy(true); setError(null); try { await api.logout(); await queryClient.cancelQueries(); queryClient.removeQueries({ predicate: query => query.queryKey[0] !== 'me' }); queryClient.setQueryData(['me'], null); navigate('/login'); } catch (e) { setError(e); } finally { setBusy(false); } }
  return <div className="app-shell"><aside className="sidebar"><Link to="/jobs" className="brand"><span className="brand-mark">♮</span><span>音谱<small>本地音乐转谱</small></span></Link><div className="nav-caption">工作空间</div><nav><NavLink to="/jobs" end>我的任务</NavLink>{user?.role === 'admin' && <><div className="nav-caption">管理员</div><NavLink to="/admin/users">账号审核</NavLink><NavLink to="/admin/jobs">全部用户任务</NavLink><NavLink to="/admin/settings">系统配置</NavLink></>}</nav><div className="sidebar-bottom"><span className="user-avatar">{user?.username.slice(0, 1)}</span><div className="user-info"><strong>{user?.username}</strong><small>{user?.role === 'admin' ? '管理员 · 可访问全部文件' : '仅可访问自己的文件'}</small></div><button className="text-button" onClick={() => void logout()} disabled={busy}>退出</button></div></aside><div className="workspace"><div className="topbar"><span>私有工作空间</span><span className="local-indicator">本地处理 · 不使用云端推理</span></div><ErrorNotice error={error} /><main className="page"><Outlet /></main><footer>自动扒谱初稿，不保证完整还原 · 第一版不提供谱面编辑</footer></div></div>;
}
