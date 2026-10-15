# 实施接口约定

此文件用于阶段 A 实现协作；能力边界仍以 design.md 为准。

## 后端 API

`/api/v1`；错误 `{error:{code,message}}`；Cookie 会话；写请求 `Origin` 必须匹配配置 `PUBLIC_ORIGIN`（CLI 无 Origin 允许）。JSON 使用 snake_case。

- auth/register/login: `{username,password}`；register 返回 `{status:"pending"}`；login/me 返回 User `{id,username,role,status}`。
- auth/logout: POST。
- GET /capabilities: `{engines:[{id,label,ready,limitations: string[]}],max_upload_bytes,max_audio_duration_seconds,max_unfinished_jobs_per_user}`。
- POST /jobs: multipart `file`, `kind` audio/musicxml, `engine` baseline 可选。202 返回 Job。
- Job: `{id,user_id,username?,title,kind,status,stage,stage_progress,engine,actual_device,config_snapshot,warnings:string[],error_code,error_message,attempt,created_at,started_at,completed_at,expires_at,tracks:Track[],artifacts:Artifact[]}`。
- GET /jobs 和 /admin/jobs 返回 Job[]（最多近期 200 个）；GET /jobs/:id 返回完整 Job。
- POST /jobs/:id/cancel 和 /retry；DELETE /jobs/:id。
- GET /jobs/:id/tracks、/artifacts 返回数组。
- Track: `{id,label,program:number|null,confidence:number|null,label_source,is_drum,source_stem_ids:string[]}`。
- Artifact: `{id,track_id:null|string,kind,label,mime,bytes,status:"ready"|"failed",error:null|string}`。kind 为 original / score / part / midi / stem / stem_preview / synth / synth_preview / pdf / part_pdf / timeline / notes / manifest。GET /artifacts/:id/content、/download；失败项无下载。
- GET /admin/users 返回 User[]；POST /admin/users/:id/approve、/reject、/disable。
- GET /admin/settings 和 PATCH /admin/settings：`{max_audio_duration_seconds:600,max_unfinished_jobs_per_user:3,retention_days:null,max_upload_bytes:209715200,default_engine:"baseline"}`。
- GET /health/live、/ready。

## Python 流水线接口

模块 `pipeline.service` 提供：

```python
def capabilities() -> list[dict]: ...
def process(input_path: Path, work_dir: Path, kind: str, config: dict,
            progress: Callable[[str, float | None], None]) -> dict: ...
```

progress 可抛出取消异常，所有子进程需定时 progress/检查取消、超时。异常类 `PipelineError(code,message)` 提供脱敏错误。

process 输出 `{tracks: Track[], artifacts: PipelineArtifact[], warnings: list[str], actual_device: str, engine: str}`。PipelineArtifact `{track_id,kind,label,path:相对于work_dir的文件名,mime,status,error}`；仅 ready 项需要 path。业务 worker 验证路径在 attempt work_dir 内，计算 bytes/sha256 并原子移到 `results/<attempt>/`；校验 lease 后在事务中发布。tracks 的 id 是流水线生成 UUID（前端使用此 ID 选分谱）。原始文件由 API 登记 artifact，流水线不重复返回。

MusicXML 有效但 synth/PDF 失败时返回 failed 产物、任务 partial；核心 score 失败抛 PipelineError。

后端依赖根 `backend/requirements.txt` 包含 API + pipeline 的轻依赖。模型依赖位于 `backend/requirements-models.txt` 并使用 `/opt/models-venv/bin/python`（环境 `MODEL_PYTHON`）子进程隔离。Demucs 本地仓库 `DEMUCS_REPO=/models/demucs`；Basic Pitch 使用包内 ONNX 权重。`INFERENCE_DEVICE=auto|cpu|cuda`；`SOUNDFONT_PATH=/soundfonts/default.sf2`；`MUSESCORE_BIN=musescore`；`STAGE_TIMEOUT_SECONDS=3600`。baseline readiness 禁止自动下载用户任务所需权重。

## 时间线

`{version:1,initial_offset_sec:0,tempo_map:[{quarter:0,bpm:120}],warnings:string[],events:[{id,track_id,score_position:{measure:number,beat:number,quarter:number},source_start_sec:number|null,source_end_sec:number|null,synth_start_sec:number,synth_end_sec:number}]}`。MusicXML 原谱保留，仅合成时间轴；重复结构提示直线播放。游标用 quarter 定位；分谱按 track_id 筛选。JSON 不含宿主路径。
