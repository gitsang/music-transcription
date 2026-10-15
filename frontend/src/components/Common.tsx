import type { ReactNode } from 'react';
import { errorMessage } from '../lib/api';
import { statusLabels } from '../lib/format';
import type { JobStatus } from '../lib/types';
export function ErrorNotice({ error, retry }: { error: unknown; retry?: () => void }) { return error ? <div className="notice danger" role="alert"><span>{errorMessage(error)}</span>{retry && <button className="text-button" onClick={retry}>重新加载</button>}</div> : null; }
export function Loading({ children = '正在加载…' }: { children?: ReactNode }) { return <div className="loading" role="status"><span className="spinner" />{children}</div>; }
export function Empty({ title, children }: { title: string; children?: ReactNode }) { return <div className="empty"><h3>{title}</h3><p>{children}</p></div>; }
export function StatusBadge({ status }: { status: JobStatus }) { return <span className={`badge ${status}`}>{statusLabels[status] ?? status}</span>; }
export function PageHeading({ eyebrow, title, children, action }: { eyebrow: string; title: string; children?: ReactNode; action?: ReactNode }) { return <header className="page-heading"><div><p className="eyebrow">{eyebrow}</p><h1>{title}</h1>{children && <p className="muted">{children}</p>}</div>{action}</header>; }
