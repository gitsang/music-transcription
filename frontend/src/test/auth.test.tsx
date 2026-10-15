import { afterEach, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { QueryClientProvider } from '@tanstack/react-query';
import { MemoryRouter, Outlet, Route, Routes } from 'react-router-dom';
import { AuthPage } from '../pages/AuthPages';
import { AuthProvider, Protected } from '../components/Auth';
import { Layout } from '../components/Layout';
import { queryClient } from '../lib/query';
import { api, ApiError } from '../lib/api';

afterEach(() => { cleanup(); queryClient.clear(); vi.restoreAllMocks(); });
it('updates mounted authentication after login and logout', async () => {
  vi.spyOn(api, 'me').mockRejectedValue(new ApiError(401, 'AUTH_REQUIRED', '请登录'));
  vi.spyOn(api, 'login').mockResolvedValue({ id: 'u', username: 'alice', role: 'user', status: 'active' });
  vi.spyOn(api, 'logout').mockResolvedValue(undefined);
  render(<QueryClientProvider client={queryClient}><MemoryRouter initialEntries={['/login']}><AuthProvider><Routes>
    <Route path="/login" element={<AuthPage />} />
    <Route element={<Protected />}><Route element={<Layout />}><Route path="/jobs" element={<div>任务测试页面<Outlet /></div>} /></Route></Route>
  </Routes></AuthProvider></MemoryRouter></QueryClientProvider>);
  await screen.findByRole('button', { name: '登录' });
  fireEvent.change(screen.getByLabelText('用户名'), { target: { value: 'alice' } });
  fireEvent.change(screen.getByLabelText('密码'), { target: { value: 'password-123456' } });
  fireEvent.click(screen.getByRole('button', { name: '登录' }));
  await screen.findByText('任务测试页面');
  expect(screen.getByText('alice')).toBeTruthy();
  fireEvent.click(screen.getByRole('button', { name: '退出' }));
  await waitFor(() => expect(screen.getByRole('button', { name: '登录' })).toBeTruthy());
});
