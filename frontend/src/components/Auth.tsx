import { createContext, useContext, useEffect, type ReactNode } from 'react';
import { useQuery } from '@tanstack/react-query';
import { Navigate, Outlet, useLocation } from 'react-router-dom';
import { api, ApiError } from '../lib/api';
import { queryClient } from '../lib/query';
import type { User } from '../lib/types';
import { ErrorNotice, Loading } from './Common';
const Context = createContext<{ user: User | null }>({ user: null });
export const useAuth = () => useContext(Context);
export function AuthProvider({ children }: { children: ReactNode }) {
  const query = useQuery({ queryKey: ['me'], queryFn: async () => {
    try { return await api.me(); } catch (error) { if (error instanceof ApiError && error.status === 401) return null; throw error; }
  }, staleTime: 30_000, retry: false });
  useEffect(() => { const expire = () => { void queryClient.cancelQueries(); queryClient.removeQueries({ predicate: query => query.queryKey[0] !== 'me' }); queryClient.setQueryData(['me'], null); }; window.addEventListener('session-expired', expire); return () => window.removeEventListener('session-expired', expire); }, []);
  return <Context.Provider value={{ user: query.data?.status === 'active' ? query.data : null }}>
    {query.isPending ? <Loading>正在确认登录状态…</Loading> : query.isError ? <main className="auth-recovery"><ErrorNotice error={query.error} retry={() => void query.refetch()} /></main> : children}
  </Context.Provider>;
}
export function Protected({ admin = false }: { admin?: boolean }) {
  const { user } = useAuth(); const location = useLocation();
  if (!user) return <Navigate to="/login" state={{ from: location.pathname }} replace />;
  if (admin && user.role !== 'admin') return <div className="page"><h1>没有管理员权限</h1><p>普通用户只能访问自己的任务和文件。</p></div>;
  return <Outlet />;
}
