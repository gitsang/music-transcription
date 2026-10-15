export type UserStatus = 'pending' | 'active' | 'rejected' | 'disabled';
export interface User { id: string; username: string; role: 'user' | 'admin'; status: UserStatus }
export type JobStatus = 'queued' | 'running' | 'succeeded' | 'partial' | 'failed' | 'cancelled';
export type Stage = 'validating' | 'preparing' | 'analyzing' | 'separating' | 'transcribing' | 'scoring' | 'exporting' | 'finalizing';
export type ArtifactKind = 'original' | 'original_preview' | 'score' | 'part' | 'midi' | 'stem' | 'stem_preview' | 'synth' | 'synth_preview' | 'pdf' | 'part_pdf' | 'timeline' | 'notes' | 'manifest';
export interface Artifact { id: string; track_id: string | null; kind: ArtifactKind; label: string; mime: string; bytes: number | null; status: 'ready' | 'failed'; error: string | null }
export interface Track { id: string; label: string; program: number | null; confidence: number | null; label_source: 'model' | 'separator_category' | 'unknown' | 'imported'; is_drum: boolean; source_stem_ids: string[] }
export interface Settings { max_audio_duration_seconds: number; max_unfinished_jobs_per_user: number; retention_days: number | null; max_upload_bytes: number; default_engine: string }
export interface Job {
  id: string; user_id: string; username?: string; title: string; kind: 'audio' | 'musicxml'; status: JobStatus;
  stage: Stage | null; stage_progress: number | null; engine: string | null; actual_device: string | null;
  config_snapshot: Partial<Settings>; warnings: string[]; error_code: string | null; error_message: string | null;
  attempt: number; created_at: string; started_at: string | null; completed_at: string | null; expires_at: string | null;
  tracks: Track[]; artifacts: Artifact[];
}
export interface Engine { id: string; label: string; ready: boolean; limitations: string[] }
export interface Capabilities { engines: Engine[]; max_upload_bytes: number; max_audio_duration_seconds: number; max_unfinished_jobs_per_user: number }
export interface TimelineEvent {
  id: string; track_id: string;
  score_position: { measure: number; beat: number; quarter: number };
  source_start_sec: number | null; source_end_sec: number | null;
  synth_start_sec: number; synth_end_sec: number;
}
export interface Timeline { version: 1; initial_offset_sec: number; tempo_map: { quarter: number; bpm: number }[]; warnings: string[]; events: TimelineEvent[] }
export type AudioMode = 'source' | 'synth';
export type AudioRole = 'original' | 'synth' | 'stem';
export interface AudioSource { id: string; label: string; mode: AudioMode; role: AudioRole; trackId?: string; detail?: string; artifact: Artifact }
