import { BrowserRouter, Navigate, Route, Routes } from 'react-router-dom';
import { AuthProvider, Protected } from './components/Auth';
import { Layout } from './components/Layout';
import { AuthPage } from './pages/AuthPages';
import { JobsPage } from './pages/Jobs';
import { JobDetail } from './pages/JobDetail';
import { AdminSettings, AdminUsers } from './pages/Admin';
function NotFound() {
  return <div className="empty"><h1>页面不存在</h1><p>链接可能已失效，或你没有访问权限。</p><a className="button" href="/jobs">返回工作空间</a></div>;
}
export function App() {
  return <BrowserRouter><AuthProvider><Routes>
    <Route path="/login" element={<AuthPage />} />
    <Route path="/register" element={<AuthPage register />} />
    <Route element={<Protected />}>
      <Route element={<Layout />}>
        <Route path="/jobs" element={<JobsPage />} />
        <Route path="/jobs/:id" element={<JobDetail />} />
        <Route element={<Protected admin />}>
          <Route path="/admin/users" element={<AdminUsers />} />
          <Route path="/admin/jobs" element={<JobsPage admin />} />
          <Route path="/admin/settings" element={<AdminSettings />} />
        </Route>
      </Route>
    </Route>
    <Route path="/" element={<Navigate to="/jobs" replace />} />
    <Route path="*" element={<NotFound />} />
  </Routes></AuthProvider></BrowserRouter>;
}
