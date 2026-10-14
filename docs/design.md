# 扒谱服务设计与 Demo 交接

> 状态：第一版设计，尚未实现或验证模型运行效果。
> 本文区分「已确认需求」「建议实现」和「待验证能力」。接手 Agent 可以直接按本文制作 Demo；无需继续询问谱号、量化网格、排版等细粒度记谱问题。

## 1. 目标与产品边界

提供一个公网部署、多用户使用的完整歌曲扒谱服务：上传音频，后台生成自动扒谱初稿，在网页查看 MusicXML，听原曲、合成演奏和分离音轨，并下载结果。也允许上传已有 MusicXML，直接查看及合成播放。

**产品目标是自动识别所有声部及乐器，不是固定编制的三轨/四轨谱。** 但当前参考流程不能保证实现这个目标。Demo 必须真实展示所使用模型的能力和限制，不得把「生成了有效 XML」等同于「完整准确还原歌曲」。

### 1.1 已确认需求

| 项目 | 决定 |
| --- | --- |
| 前端 | React + TypeScript |
| 后端 | Python + FastAPI，耗时处理由独立 worker 执行 |
| 输入 | 文件上传，不做歌曲链接导入；完整歌曲音频及已有 MusicXML |
| 扒谱目标 | 自动识别乐器，输出包含所有可识别声部的乐队总谱 |
| 无法识别的乐器 | 保留音符，标记为「未知乐器」 |
| 结果定位 | 自动扒谱初稿，允许漏音、错音、节奏及乐器识别误差 |
| 记谱 | 实音谱；其他记谱细节沿用模型及转换工具默认输出 |
| 编辑 | 第一版不支持任何手动编辑，包括音符、节奏及乐器类型 |
| 展示 | MusicXML 总谱及声部分谱 |
| 播放 | 原曲、音符合成演奏、分离后的原始音频；提供谱面位置同步 |
| 下载 | MusicXML 总谱、各声部分谱、MIDI、分离音频、合成音频、PDF 乐谱 |
| 部署 | 公网；提供 Containerfile 和 compose.yml |
| 算力 | 支持 NVIDIA Tesla P4，也支持纯 CPU 部署 |
| 隐私 | 全部使用本地模型/工具，不调用云端推理 API，不向外部发送用户音频 |
| 账号 | 用户名 + 密码，自行注册；管理员审核通过后才能登录 |
| 普通用户权限 | 仅访问自己的任务和文件 |
| 管理员权限 | 审核账号，管理系统；可查看、播放、下载全部用户文件 |
| 排队限制 | 每用户默认最多 3 个未完成任务，管理员可配置 |
| 音频时长 | 默认最长 10 分钟，管理员可配置 |
| 保存期限 | 原始音频及生成文件默认永久保存，可配置期限 |
| 优先级 | 准确度优先，接受长耗时异步处理 |

### 1.2 不做什么

- 不承诺所有乐器、同类乐器的多个演奏者、复杂和弦都能准确拆分。
- 不做网页乐谱编辑、手动乐器修正、歌词识别或实时边录边扒谱。
- 不做外部平台下载、邮件服务、支付计费、第三方登录。
- 不自行扩展简谱、六线谱、移调谱等格式；暂不精调排版及演奏记号。
- 不把管理员查看文件描述为端到端私密：注册及上传页面明确告知此权限。

## 2. 参考工程：可复用与不可照搬部分

参考目录：`../AgentMemoryBank/music2xml/`。当前本仓库仅有 LICENSE，未有应用代码。

| 参考文件 | 可借鉴内容 | 必须去掉的假设 |
| --- | --- | --- |
| `README.md` | Demucs 分轨、Basic Pitch 逐音、music21 导出及依赖踩坑 | 单曲 CPU 耗时不能当作本服务 SLA；多乐器模型描述不是本机验证结果 |
| `scripts/bp-to-musicxml.py` | CSV 音符、碎片合并、和弦块、时值拆分、跨小节连音、鼓 MIDI 映射 | 《浮尘》路径、BPM 92.29、相位 0.542、D 大调、74 小节、固定声部；硬编码的 crash 鼓点 |
| `scripts/midi-to-musicxml.py` | music21 总谱/分谱导出 | 固定四声部、固定调号、曲名和作者 |
| `scripts/analyze-structure.py` | 节拍/onset 分析思路 | 固定曲式段落、固定输出文件、BPM 搜索范围及恒定 4/4 假设 |
| `scripts/analyze-melody-voicing.py` | 辅助分析思路 | 固定 BPM、时间区间、和弦块，不应直接成为通用处理链 |

参考流程是案例脚本，不是可直接上线的服务，也没有浏览器播放器。

**重要：** Basic Pitch 是有音高的音符识别器，不负责自动判断所有乐器，也不负责鼓分类。Demucs 的 `htdemucs_6s` 输出 vocals/drums/bass/guitar/piano/other，是六类混合音源，不代表歌曲只有六个声部，也不能区分两把吉他。不能只给 Basic Pitch 的结果贴上 Demucs 类别名，就宣称完成「所有乐器自动识别」。

## 3. 建议技术架构

以下是为了尽快做出可部署 Demo 的建议，不是新增的用户要求。

```text
浏览器：React / TypeScript
          │ 同源 HTTPS，Cookie 会话
          ▼
web：静态前端 + Nginx 反向代理（公网唯一入口）
          │ /api
          ▼
api：FastAPI ─────────── PostgreSQL（用户、任务、产物、配置、审计）
          │                         ▲
          │ 入队                     │ 状态、阶段、心跳
          ▼                         │
Redis（Celery broker） ─────── worker：本地音频/模型/导出流水线
                                      │
api（授权后提供文件） ─────────────── 私有数据卷
                                      │
                                 本地模型/音色缓存
```

### 3.1 组件建议

| 组件 | 建议 | 说明 |
| --- | --- | --- |
| 前端 | Vite + React + TypeScript，React Router、TanStack Query | Query 管理任务状态；Demo 使用轮询即可，不必先做 WebSocket |
| 乐谱渲染 | OpenSheetMusicDisplay（OSMD） | 只负责谱面渲染与游标，不假设它自带完整音频播放器 |
| 播放 | HTMLAudioElement + 统一 transport 控制器 | 后端预生成合成音频；避免 Demo 同时实现一套浏览器 MIDI 合成器 |
| API | FastAPI + Pydantic + SQLAlchemy + Alembic | 建议 Python 3.11 起步，最终版本以依赖安装验证为准 |
| 数据库 | PostgreSQL | 作为权限、任务状态与配置的唯一权威来源 |
| 队列 | Celery + Redis | Demo 默认一个推理并发，后续可拆 GPU/CPU 队列 |
| 音频处理 | FFmpeg/ffprobe | 解码、真实时长检测、重采样、试听格式转换 |
| 记谱转换 | music21 | MusicXML/MIDI 读写及总谱、分谱导出 |
| 合成 | FluidSynth + 本地、许可允许再分发的 GM SoundFont | 生成 WAV 归档、MP3 或其他经浏览器验证的试听格式；注意鼓通道 |
| PDF | 本地 MuseScore CLI，必要时 headless/Xvfb | 在容器中实际验证导出命令、字体及运行时；不可假设 music21 自带 PDF 渲染 |

不引入 S3、Kubernetes 或多个微服务推理集群。文件存储先用宿主机挂载卷，抽象存储接口以便以后迁移。

## 4. 本地扒谱引擎与能力分级

### 4.1 统一引擎接口

将模型包装为适配器，而不是将业务代码绑死在一个 CLI：

```python
class TranscriptionEngine:
    def capabilities(self) -> dict: ...
    def transcribe(self, input_path, work_dir, config, progress) -> dict: ...
```

返回统一的声部、原始 note events、节拍/速度信息、模型标识及警告。业务层只处理统一结构，不能依赖《浮尘》CSV 文件名或固定声部顺序。

建议 note event 最小字段：`id / track_id / start_sec / end_sec / midi_pitch / velocity / is_drum`；秒数基于原曲零点，另保留规范化后的谱面位置和源事件关联。乐器 program 统一内部使用 MIDI 0–127，导出 MusicXML 时显式转换其编号，不能混用不同格式的编号。

建议提供两档引擎：

1. **`baseline`：先打通真实端到端流程。** Demucs 6 类分离 + Basic Pitch 处理有音高分轨 + 本地鼓 onset 检测 + music21。这是有限能力的 Demo 引擎，必须显示「分离器粗分类，未实现任意乐器识别」；`other` 显示为未知/混合声部，低能量音轨可自动过滤并记录规则。不得插入参考脚本里的人工鼓点或曲式。
2. **`multi_instrument`：满足核心产品目标的候选引擎。** 参考资料提及 YourMT3/YourMT3+、MT3 等本地多乐器转谱路线。先验证仓库可用性、权重下载、许可证、依赖、输出乐器类别、CPU 路径以及 P4 运行能力，再选择和集成。当前不把任何候选写成已经可运行或保证更准确的方案。

准确度优先意味着正式第一版应优先评估多乐器模型；baseline 只是可见、可测的降级方案，不是「全声部识别」的最终实现。**如果 Demo 仅完成 baseline，必须在 README、任务结果及交接说明中列为未完成能力。**

模型没有可靠类别/置信度时，允许输出未知乐器；没有置信度就返回 `null`，不编造百分比。如果某个乐器完全漏检，不能声称已经保留该声部。

### 4.2 分离音轨与乐谱声部不能等同

多乐器模型可能输出弦乐、管乐等多个乐谱声部，但 Demucs 只提供六类分离音频。两者不保证一一对应。

- UI 分开显示「识别声部」和「分离音轨」。
- 一个混合 `other` 音轨可能关联多个乐谱声部；映射不明时不绑定。
- 不生成假的「独立小提琴原始音频」。无法独立分离时，显示「暂无对应独立分轨」。
- 无法分离出的音轨不列出下载按钮；现有分离音轨全部可下载。
- 总谱保留模型实际输出的声部，不因分离器只有六类而截断为六个声部。

### 4.3 GPU / CPU 策略

- `INFERENCE_DEVICE=auto|cpu|cuda`。`auto` 可在 CUDA 不可用时选 CPU，但必须记录实际设备和原因；显式 `cuda` 不可用应报可读错误。
- Tesla P4 属于较老的 Pascal GPU。显存容量、驱动及 CUDA 可用性通过 `nvidia-smi` 实测；不能用新款 GPU 的经验推断吞吐量，也不能假设最新 PyTorch wheel 支持其计算能力。
- 锁定验证支持 P4 的 PyTorch/CUDA 组合；不默认依赖 BF16、FlashAttention 等新硬件特性。Demo 优先保守精度与 batch size 1。
- 分块/重叠推理、按任务串行加载模型以控制显存；拼接时修正绝对时间、去重，不能让每块的时间都从零开始。
- OOM 可按确定策略缩小分块重试，仍失败则明确失败；不得静默换低质量模型。
- CPU 部署必须能运行真实流水线，不仅是打开网页；允许明显更慢。CPU/GPU 是否使用同一模型及降级差异必须公开。
- 默认 `WORKER_CONCURRENCY=1`、Celery prefetch 1。限制科学计算库线程数，避免多进程争抢 P4 或 CPU。

Basic Pitch、Demucs、多乐器模型可能存在互不兼容的依赖。接手时先做安装和短音频推理验证；必要时在同一 worker 镜像内用独立 venv/子进程隔离，不强行拼装一个无法安装的 requirements 文件。

## 5. 处理流水线

### 5.1 上传音频

```text
上传并校验 → 持久化任务/入队 → 解码与标准化 → 节拍分析
→ 音源分离 + 音符/乐器识别 → 统一 note events
→ 默认记谱转换 → MusicXML 总谱/分谱、MIDI
→ 本地合成音频、PDF、试听分轨、播放时间线
→ 产物校验 → 发布结果
```

- 保留原始文件；各模型按其需要从标准化音频重采样。保留完整起始静音和时间原点，不擅自截头导致原曲对不上谱。
- 输入解码和模型推理全部在受限 worker 中执行；API 不同步等待扒谱完成。
- 调性、拍号、谱号、量化、连音及排版尽量采用工具默认/自动结果。不写死单曲参数，不另开一轮需求确认。自动估计不足时记录默认值及警告。
- 原始 note events 用于保留识别信息；谱面及合成产物共同使用一份规范化/量化后的事件，避免二者内容不一致。
- 鼓走专用识别/检测路径，并验证 MusicXML、MIDI 和音色映射；不能直接当作有音高的 Basic Pitch 输入。
- 总谱、分谱、MIDI、合成音频、PDF 均由同一规范结果生成，不能用无关示例文件代替。
- 检验 XML 可解析、声部可读、时值一致及浏览器可渲染；复杂输入涉及多声部、backup/forward、弱起和拍号变化，不能照搬参考中仅适用固定 4/4 的求和断言。

### 5.2 上传已有 MusicXML

支持 `.musicxml` / `.xml`；建议同时支持常见压缩格式 `.mxl`，但必须验证解压安全。

```text
上传 → 安全解析/校验 → 保留原谱 → 建立声部与规范化播放事件
→ 派生分谱/MIDI/合成音频/PDF/播放时间线 → 展示
```

- 不执行音源分离及扒谱，不创建不存在的原曲或分轨。
- 保留输入的乐器、速度、拍号等信息；不强行改造成固定乐队编制，也不擅自将用户原谱移调。
- 原谱可直接展示和下载；对无法支持的复杂记号、反复、跳房子等显示明确警告。Demo 可只支持已验证的直线播放子集，不能无提示地忽略反复结构。
- PDF/合成失败不应阻止有效原谱展示；任务按产物状态显示部分成功。

### 5.3 失败、部分成功和进度

任务状态：`queued → running → succeeded | partial | failed | cancelled`。

阶段使用稳定标识：`validating / preparing / analyzing / separating / transcribing / scoring / exporting / finalizing`。MusicXML 导入可跳过无关阶段。

- `stage_progress` 可空：只有真实可测时显示百分比，否则显示当前阶段、排队时间和耗时，不伪造持续增长的进度条。
- 谱面与关键转谱成功、某些导出失败：`partial`，逐个产物标注错误；失败按钮不能变成死链接。
- 核心识别/谱面生成失败：`failed`，展示脱敏错误及重试入口。
- 用户离开页面不影响任务。刷新后从数据库恢复状态。
- 不给精确完成时间承诺。队列序号最多作为估计，并说明运行中的任务不被抢占。

## 6. 谱面展示与三类播放

建议页面：`/register` 注册及待审核提示、`/login` 登录、`/jobs` 上传和我的任务列表、`/jobs/:id` 谱面/播放器/下载详情、`/admin/users` 审核账号、`/admin/jobs` 全部任务、`/admin/settings` 系统配置。任务详情在排队、处理中、失败、部分成功时也应可用，而不是只为成功结果制作页面。Demo 先采用中文界面，桌面使用优先，窄屏仍能访问上传和播放控制。

### 6.1 页面功能

- MusicXML 总谱/分谱切换、缩放、分页或纵向阅读。
- 显示识别的声部/乐器、未知乐器、引擎名称、能力限制及初稿提示。
- transport：播放、暂停、停止、拖动进度、音量、切换原曲/合成/具体分离音轨。
- Demo 先支持单个分轨试听；多分轨实时混音、solo/mute 不是阻塞交付的要求。
- 切换音源先停止旧音源，按统一时间线尽量保持位置，禁止多个播放器意外同时播放。
- 浏览器自动播放策略要求用户点击后启动；加载失败、资源未生成及不支持格式均有明确状态。

### 6.2 时间线：不能只凭 BPM 同步

建议后端输出 `timeline.json`：

- `score_position`：声部/小节/拍位置，或可定位谱面事件的稳定 ID。
- `source_start_sec/source_end_sec`：原曲时间，用于原曲及未裁切的分离音轨。
- `synth_start_sec/synth_end_sec`：谱面实际合成演奏时间。
- `tempo_map`、初始偏移、分块偏移、映射版本与警告。

原始音频事件和谱面量化事件应保留关联，不能全部丢弃后只用固定 BPM 回算。谱面转换可能修改节奏，原曲和合成时轴需要显式映射。

前端通过 OSMD 游标/可定位元素，将音频 `currentTime` 映射到谱面。暂停和 seek 后重新定位，不能靠独立计时器不断累加产生漂移。

**Demo 同步验收是可定位的谱面位置/游标跟随，不承诺逐音符精确高亮。** OSMD 对复杂 MusicXML 的定位能力需实测；若无法可靠定位逐音符，可先按拍/小节定位并标注近似。只有 MusicXML 的任务仅使用合成时间轴，原曲/分轨模式不可用。

## 7. 账号、授权和管理

### 7.1 账号流程

- 账号状态：`pending / active / rejected / disabled`；角色：`user / admin`。
- 注册成功即为待审核，无有效登录会话；页面提示「等待管理员审核」。拒绝和禁用账号同样不可登录。
- 管理员通过 CLI/一次性初始化命令创建，不允许公网表单自行注册 admin，不提供默认公开密码。
- 已登录账号被禁用时，撤销会话，并拒绝后续 API/文件请求。
- 建议 Argon2id 密码哈希，服务端会话 + HttpOnly/Secure/SameSite Cookie；写操作做 CSRF/同源校验，不将长期凭据存 localStorage。
- 登录/注册限速；管理员设置、审核、敏感文件访问及用户禁用操作写审计日志。

### 7.2 文件与任务授权

- 普通用户：列表、详情、重试、删除、播放及下载均检查资源归属。
- 管理员：可以访问所有任务和产物；在界面区分「我的任务」和「全部用户任务」。
- UUID 不是权限控制。每次文件请求，包括 Range 试听请求，均校验会话和文件归属。
- 原始音频、分轨、MusicXML、PDF、合成音频及 timeline 都是私有产物；不得挂载到公开静态目录。
- 可采用 API 授权 + Nginx `internal` / X-Accel-Redirect 发送大文件，并支持 Range；禁止外部直接命中内部文件 URL。
- 工作目录按用户/任务隔离。第一版是应用级隔离，worker 仍为可信共享进程，不宣称做到每用户独立安全沙箱。

### 7.3 管理页面

最低包含：待审核用户列表、批准/拒绝、账号禁用、全部任务及文件浏览、任务错误、全局配置。

建议系统配置：

| 配置 | 默认 | 变更规则 |
| --- | --- | --- |
| `max_audio_duration_seconds` | 600 | 上传及后端 ffprobe 校验 |
| `max_unfinished_jobs_per_user` | 3 | 原子入队时校验；建议导入任务也计入 |
| `retention_days` | `null`（永久） | 可设正整数；建议仅作用于修改后创建的新任务，批量缩短既有期限需明确确认 |
| `max_upload_bytes` | 200 MiB（建议值） | 前后端和 Nginx 一致；这是实现默认，不是用户已确认值 |
| `default_engine` | 已验证可运行的引擎 | 页面公开能力；不能无提示换引擎 |

运行设备、worker 并发、模型路径及数据库密钥用部署配置控制，不通过网页任意执行命令。任务创建时记录配置快照，处理中不受全局设置修改影响。

## 8. 数据、队列与文件生命周期

### 8.1 最小数据模型

| 表 | 核心字段 |
| --- | --- |
| `users` | id、username（规范化且唯一）、password_hash、role、status、审核信息、时间戳 |
| `sessions` | id/token_hash、user_id、expires_at、revoked_at |
| `jobs` | id、user_id、kind（audio/musicxml）、status、stage、stage_progress、engine、actual_device、config_snapshot、warnings、error_code、attempt、时间戳、expires_at |
| `tracks` | id、job_id、label、instrument/program、confidence 可空、label_source、is_drum、source_stem_ids |
| `artifacts` | id、job_id、track_id 可空、kind、storage_key、mime、bytes、hash、status、error、created_at |
| `settings` | key、value、updated_by、updated_at |
| `audit_logs` | actor_id、action、resource_id、时间戳、脱敏详情 |
| `queue_outbox` | job_id、attempt、投递状态、重试时间 |

`label_source` 区分 `model / separator_category / unknown / imported`。API 不把粗分类冒充模型乐器识别。

### 8.2 队列可靠性

- 上传先保存为私有临时文件并校验。在同一数据库事务中锁定用户，检查 queued/running 数量、创建任务和 outbox 事件，避免并行请求绕过「3 个任务」。失败临时文件要清理。
- outbox 调度器将任务 ID/attempt 发给 Celery，解决数据库提交成功但 Redis 入队失败的问题；部署中包含单个调度维护进程。
- FIFO 非抢占队列即可做 Demo，默认一个 worker 并发；每用户上限防止大量预占，不额外实现复杂调度算法。
- 消息只携带任务 ID，不含可信度不足的路径或命令。worker 从数据库读取配置和真实文件。
- worker 原子领取任务并记录 lease/心跳。重复投递、服务重启不得同时处理同一个 attempt。
- 每次尝试使用独立目录。最终产物先写临时文件，再原子发布；失败或旧 attempt 不覆盖已发布结果。
- 心跳超时且确认失去执行权后恢复/失败；worker 发布结果前再次校验 lease，避免旧进程回写。
- 自动重试仅针对暂时错误且有次数上限；缺模型、坏文件、持续 OOM 等不无限重跑。
- 重试仍受用户上限约束。取消排队任务立即取消；运行任务通过标志协作停止并终止其受控子进程。

### 8.3 存储建议

```text
/data/users/<user_id>/jobs/<job_id>/
  input/original.<ext>
  work/<attempt>/          # 临时解码和模型中间文件
  results/
    score.musicxml
    parts/<track_id>.musicxml
    score.mid
    stems/<stem_id>.wav
    preview/<stem_id>.mp3
    synth/score.wav
    synth/score.mp3
    pdf/score.pdf
    pdf/parts/<track_id>.pdf
    timeline.json
    manifest.json
/models/                   # 预置权重与校验清单
/soundfonts/               # 本地合成音色及许可文件
```

- API 返回 artifact ID，不直接返回宿主机路径。输入展示名与真实存储名分离，真实文件名由服务生成。
- 保留输入及用户可下载结果；中间文件在成功/失败后按短期策略清理，不因永久保存结果而永久保存所有缓存。
- `expires_at=null` 表示永久。设置期限时建议从任务完成时计算，不清理 queued/running 任务。
- 建议支持用户删除自己的任务和文件；管理员也可删除。运行任务先取消并确认停止，再清理文件，不让 worker 在删除后重建文件。
- 期限清理、失败重试残留、孤儿上传清理由维护进程幂等执行。结果删除后保留最小审计/墓碑，不留可访问下载链接。
- 磁盘低水位时拒绝新任务并告警，不静默清理永久保存文件。部署文档说明磁盘增长及数据库/数据卷备份。

## 9. API 草案

统一前缀 `/api/v1`。以下为建议接口，允许 Demo 内合理简化命名，不降低授权要求。

| 方法与路径 | 功能 |
| --- | --- |
| `POST /auth/register` | 用户名密码注册，返回待审核状态 |
| `POST /auth/login`、`POST /auth/logout`、`GET /auth/me` | 会话管理 |
| `POST /jobs` | multipart 上传，kind=audio 或 musicxml；返回 202 和 job ID |
| `GET /jobs`、`GET /jobs/{id}` | 我的任务、阶段、警告、产物和能力信息 |
| `POST /jobs/{id}/cancel`、`POST /jobs/{id}/retry` | 取消/重试 |
| `DELETE /jobs/{id}` | 删除任务及文件 |
| `GET /jobs/{id}/tracks` | 识别声部及分离音轨关系 |
| `GET /jobs/{id}/artifacts` | 下载清单及每项生成状态 |
| `GET /artifacts/{id}/content` | 经授权读取/播放，支持 Range |
| `GET /artifacts/{id}/download` | 经授权下载，安全 Content-Disposition |
| `GET /admin/users` | 审核与账号管理列表 |
| `POST /admin/users/{id}/approve`、`.../reject`、`.../disable` | 账号操作 |
| `GET /admin/jobs` | 全部用户任务及文件入口 |
| `GET /admin/settings`、`PATCH /admin/settings` | 管理员配置 |
| `GET /health/live`、`GET /health/ready` | 存活及依赖健康；不公开密钥或内部路径 |

建议前端每 2–5 秒轮询未完成任务，页面隐藏时降低频率，终态停止轮询。错误有稳定 code，例如 `ACCOUNT_PENDING`、`JOB_LIMIT_REACHED`、`AUDIO_TOO_LONG`、`MODEL_NOT_READY`、`ARTIFACT_NOT_AVAILABLE`；跨用户访问不泄露资源存在性。

## 10. 上传与公网安全基线

- 建议首批音频支持 WAV、MP3、FLAC、M4A、OGG；以容器 FFmpeg 实际可解码结果为准。校验文件内容，不只看扩展名或浏览器 MIME。
- 限制上传字节数、解码时长、采样率/声道范围、解码超时、进程资源及 MXL 解压后大小/条目数。
- XML 禁用外部实体、DTD 加载与网络访问；合法 MusicXML 常带 DOCTYPE，安全处理而非联网获取 DTD。MXL 防路径穿越、绝对路径、符号链接、压缩炸弹。
- FFmpeg、模型及 MuseScore 使用参数列表启动子进程，禁止 shell 拼接用户文件名；为每个阶段设置可配置超时，向用户只暴露脱敏错误。
- CPU/GPU worker 不对公网暴露端口；PostgreSQL/Redis 仅 Compose 内网；容器非 root，数据卷最小权限，不挂 Docker socket。
- 仅 web 暴露服务，公网入口启用 TLS；可由现有反向代理终止 TLS，提供清晰接入示例。开发 HTTP 不能被标为公网安全配置。
- 普通任务运行阶段不需要外网。首次模型下载是运维准备动作，仅下载公开权重，不发送用户音频；可完全离线预置。
- 前端 JS、字体、SoundFont 等部署本地，不依赖第三方 CDN 传送用户资源或保证页面可用性。

## 11. Containerfile 与 Compose 交付要求

建议仓库组织：

```text
frontend/                 # React + TypeScript
backend/
  app/                    # FastAPI、数据模型、授权、队列维护
  pipeline/               # engine 适配器、note events、导出及时间线
  tests/
Containerfile             # 多阶段构建 web/runtime；CPU 默认，GPU 可构建独立 target/参数
compose.yml               # 默认纯 CPU：web、api、worker、dispatcher、postgres、redis
compose.gpu.yml           # 建议 GPU override，仅替换 worker 镜像及设备设置
.env.example
scripts/                  # 初始化管理员、模型准备、演示样本/验证工具
models.lock.json          # 权重来源、版本、哈希、许可证；待模型选定后填写
README.md
docs/design.md
```

部署约定：

- `compose.yml` 默认无 GPU 依赖。显式 `build.dockerfile: Containerfile`，不能只交付名为 Dockerfile 的配置。
- 建议另提供 `compose.gpu.yml`，通过 `docker compose -f compose.yml -f compose.gpu.yml up -d --build` 在同一套服务上替换 worker，不意外启动两个推理 worker。
- GPU 宿主机安装兼容驱动与 NVIDIA Container Toolkit；以容器内 GPU 检测和短样本推理作为验收，不只检查设备挂载。
- CPU/GPU 镜像分别安装对应 Torch 依赖，避免 CPU 镜像无意拉入巨型 CUDA 包。API 不必安装完整模型栈；API 和 worker 共用数据模型版本。
- 配置示例包括数据库、Redis、会话密钥、数据卷、模型卷、设备、并发、超时、初始管理员创建方式及 HTTPS 代理设置。
- 所有服务提供适合自身角色的健康检查。模型缺失时可以运行账号和 MusicXML 功能，但禁止接受无法执行的音频任务并说明缺失内容。
- 数据库迁移使用单次初始化步骤，不由多个副本竞争执行；Redis 重启不丢失数据库任务，依赖 outbox/lease 恢复。
- 不将用户数据、模型权重和生产密钥提交 Git；数据库和数据文件有持久卷，`down/up` 不丢失用户结果。
- pin 经安装与运行验证的依赖、镜像及权重版本。参考提及的 Python 3.11 / `setuptools<81` 是兼容性线索，不是所有候选模型统一适用的锁文件。
- 下载及再分发前检查模型权重、代码、SoundFont、MuseScore、字体及编解码相关许可证，保留 attribution；不要照抄参考表作为法律结论。

## 12. Demo 实施顺序

### 阶段 A：先打通可真实演示的垂直链路

1. 建立 React/FastAPI/数据库/队列骨架，完成管理员初始化、注册审核、登录及归属授权。
2. **先实现已有 MusicXML 导入 → 渲染 → 合成播放 → 分谱/MIDI/PDF 下载。** 用于快速验证播放器、音色、PDF 容器依赖和文件访问。
3. 接入音频上传、时长和任务上限校验、持久队列、真实 baseline 扒谱，生成总谱/分谱/MIDI。
4. 接入原曲、真实分离音轨、合成音频及时间线；实现所有可用产物下载及部分失败展示。
5. 增加管理员全局浏览与配置、删除/期限清理、CPU Compose 部署与测试。

### 阶段 B：验证核心模型能力与 P4

1. 用短样本和完整歌曲评估本地多乐器模型：乐器类别、声部数量、漏音、鼓、时间线、资源占用。
2. 在 Tesla P4 及 CPU 上实际运行，记录模型版本、歌曲时长、耗时、峰值内存/显存和失败原因。
3. 集成验证通过的模型到 `multi_instrument` 适配器，保留粗分类 baseline 作为明确标识的替代方案。
4. 提供 GPU override 并做容器内端到端验收。没有可用 P4 环境时标记「待实机验证」，不要伪造通过记录。

**阶段 A 可以交付用于先看效果的 Demo，但必须列出阶段 B 未完成项。** 若默认只做模拟页面，必须叫 UI 原型，不能叫真实扒谱 Demo。样例/fixture 必须显式标记，不得冒充用户上传后的模型结果。

## 13. 验收清单

### 13.1 功能与权限

- [ ] 用户注册后不能登录；管理员批准后可以登录，拒绝/禁用时不能登录。
- [ ] 两个普通账号互相无法访问任务、timeline、原曲、分轨、谱面和下载，包括直接请求 ID/Range；管理员可访问全部。
- [ ] 上传真实音频后离开/刷新页面，后台仍处理；阶段、错误及终态可恢复。
- [ ] 并发提交不能绕过每用户默认 3 个未完成任务限制；600 秒默认上限可配置且由后端验证。
- [ ] MusicXML 导入不运行扒谱，可展示、合成播放及下载派生产物；没有原曲/分轨时明确不可用。
- [ ] 音频任务总谱/分谱可渲染；三类音源可播放、暂停、seek，并有近似或实测谱面定位。
- [ ] MusicXML、MIDI、分离音频、合成音频及总谱/分谱 PDF 可真实下载；生成失败逐项展示原因。
- [ ] 无网页编辑入口；未知/混合声部、初稿及模型能力限制可见。
- [ ] 默认永久保存；设置期限、用户删除、管理员查看及审计行为与设计一致。

### 13.2 运维与鲁棒性

- [ ] `Containerfile` + `compose.yml` 能在无 GPU 环境启动并跑完一个短音频真实任务。
- [ ] GPU 配置能在 P4 上跑真实任务，或明确列为未验证；记录设备及依赖版本。
- [ ] Redis/worker 中断、重复投递、失败重试不会重复发布或丢失任务；取消后不重建删除文件。
- [ ] 缺权重、OOM、坏音频、恶意 XML/MXL、导出超时、磁盘不足均有可理解的失败路径。
- [ ] 浏览器实际检查乐谱及三类播放器，不仅跑后端单元测试；鼓合成试听不是普通旋律音色。
- [ ] 离线权重准备完成后，断开推理外网仍可使用；没有用户音频云上传或 CDN 依赖。

### 13.3 效果验证与交接

建议准备有权使用的短样本：鼓/贝斯/吉他歌曲、包含键盘/其他乐器的歌曲、未知/重叠声部，以及带速度变化的 MusicXML。至少给出一首完整歌曲的真实运行记录；无需在 Demo 前制定复杂乐谱美学标准。

交接必须包含：启动命令、模型准备命令、管理员初始化方式、样例运行记录、CPU/GPU 验证范围、已完成项和已知限制。对不能可靠识别或分离的乐器写清楚，不用漂亮谱面替代效果说明。

## 14. 给接手 Agent 的执行摘要

按本文做一个可运行、可演示的版本，不再追问细粒度记谱规则。优先完成 MusicXML 导入与播放的垂直链路，再接入真实音频扒谱。复用参考工程的分析和转换思路，但去掉全部单曲常量与人工注入内容。

React + TypeScript、Python + FastAPI、本地推理、多账号审核、队列和私有文件访问是确定范围。基础分离+Basic Pitch 只构成有限能力 Demo；任意乐器/全声部自动识别仍需验证本地多乐器模型。将真实进度、失败和降级展示给用户。交付 Containerfile、CPU compose.yml、建议的 GPU override、运行说明和测试记录；不要承诺尚未实测的准确度或 Tesla P4 性能。
