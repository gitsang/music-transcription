import { useQuery } from '@tanstack/react-query';
import { api } from '../lib/api';
import { isActive, pollInterval, usePageHidden } from '../lib/query';
import { ErrorNotice, Loading, PageHeading } from '../components/Common';
import { Upload } from '../components/Upload';
import { JobList } from '../components/JobList';
export function JobsPage({ admin = false }: { admin?: boolean }) {
  const hidden = usePageHidden();
  const query = useQuery({ queryKey: [admin ? 'admin-jobs' : 'jobs'], queryFn: () => api.jobs(admin), refetchInterval: q => pollInterval(q.state.data?.some(isActive) ?? false, hidden), refetchIntervalInBackground: true });
  const jobs = query.data ?? [];
  return <><PageHeading eyebrow={admin ? '管理 / 全局浏览' : '工作空间 / 我的任务'} title={admin ? '全部用户任务' : '我的任务'} action={<button onClick={() => void query.refetch()} disabled={query.isFetching}>刷新列表</button>}>{admin ? '管理员可访问所有用户的任务、谱面及私有文件。' : '上传、等待处理，再对照谱面与音频。任务状态会自动恢复。'}</PageHeading>{!admin && <Upload />}{query.isPending ? <Loading /> : <><ErrorNotice error={query.error} retry={() => void query.refetch()} /><div className="summary-strip"><span><strong>{jobs.length}</strong> 近期任务</span><span><strong>{jobs.filter(isActive).length}</strong> 未完成</span><span><strong>{jobs.filter(j => j.status === 'partial').length}</strong> 部分成功</span><span>隐藏页面时降低刷新频率，终态停止轮询</span></div><JobList jobs={jobs} admin={admin} /></>}</>;
}
