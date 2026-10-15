import { useEffect, useState } from 'react';
import { QueryClient } from '@tanstack/react-query';
import type { Job } from './types';
import { ApiError } from './api';
export const queryClient = new QueryClient({ defaultOptions: { queries: {
  staleTime: 10_000, refetchOnWindowFocus: true,
  retry: (count, error) => !(error instanceof ApiError && error.status < 500) && count < 1,
} } });
export const isActive = (job: Pick<Job, 'status'>) => job.status === 'queued' || job.status === 'running';
export function pollInterval(active: boolean, hidden: boolean) { return active ? (hidden ? 30_000 : 3_000) : false; }
export function usePageHidden() {
  const [hidden, setHidden] = useState(document.hidden);
  useEffect(() => { const update = () => setHidden(document.hidden); document.addEventListener('visibilitychange', update); return () => document.removeEventListener('visibilitychange', update); }, []);
  return hidden;
}
