import { useMutation } from '@tanstack/react-query';
import { useNavigate } from 'react-router-dom';
import { api } from '../lib/api';
import { isActive, queryClient } from '../lib/query';
import type { Job } from '../lib/types';
import { ErrorNotice } from './Common';
export function JobActions({ job }: { job: Job }) {
  const navigate = useNavigate();
  const mutation = useMutation({
    mutationFn: (action: 'retry' | 'cancel' | 'delete') => api.jobAction(job.id, action),
    onSuccess: (_data, action) => {
      void queryClient.invalidateQueries({ queryKey: ['jobs'] }); void queryClient.invalidateQueries({ queryKey: ['admin-jobs'] });
      if (action === 'delete') { queryClient.removeQueries({ queryKey: ['job', job.id] }); navigate('/jobs', { replace: true }); }
      else { void queryClient.invalidateQueries({ queryKey: ['job', job.id] }); }
    },
  });
  const messages = {
    delete: '永久删除该任务及所有文件？此操作无法撤销。运行中的任务会先取消并等待停止。',
    cancel: '取消该任务？运行中的处理会协作停止，状态以服务端为准。',
    retry: '重新处理此任务？会从原始输入重新开始，重试仍受未完成任务上限约束。',
  };
  function act(action: 'retry' | 'cancel' | 'delete') { if (window.confirm(messages[action])) mutation.mutate(action); }
  const retryable = job.status === 'failed' || job.status === 'partial' || job.status === 'cancelled';
  return <div><div className="actions">{isActive(job) && <button onClick={() => act('cancel')} disabled={mutation.isPending}>取消任务</button>}{!isActive(job) && <><button className="danger-button" onClick={() => act('delete')} disabled={mutation.isPending}>删除任务</button>{retryable && <button className="primary" onClick={() => act('retry')} disabled={mutation.isPending}>重试</button>}</>}{mutation.isPending && <span className="muted">正在提交…</span>}</div>{isActive(job) && <small className="muted">运行中的任务请先取消，确认停止后再删除；取消不会发布半成品。</small>}{job.status === 'partial' && <small className="muted">重试会重新生成全部产物；当前可用的谱面与文件在重试前仍可下载。</small>}<ErrorNotice error={mutation.error} /></div>;
}
