# 音谱 · 本地音乐转谱服务

React / TypeScript + Go + PostgreSQL + Redis Streams。Python 仅保留本地模型与乐谱处理流水线。用户上传完整音频或 MusicXML，后台处理，浏览器查看总谱与分谱、对照试听和下载。音频由本地模型处理，不调用云端推理 API。管理员能查看、播放、下载全部用户文件；普通用户只能访问自己的任务。

当前实现设计阶段 A 的 baseline Demo，**尚未完成任意乐器 / 全声部自动识别**。Demucs `htdemucs_6s` 提供六类混合分离，Basic Pitch 识别有音高事件，鼓使用频带 onset 近似检测。类别来自分离器而非乐器识别模型；`other` 标为未知/混合，置信度为空。`multi_instrument` 显示未就绪，不会接受任务。Tesla P4 尚未实机验证。

## 已实现

- 注册待审核，管理员批准、拒绝、禁用，Argon2id 密码与服务端 Cookie 会话；禁用后后续 API/文件请求立即失效。
- 我的任务、全部用户任务、账号审核、系统配置；可取消、重试和删除任务。默认每用户三个未完成任务、音频最长 600 秒、上传 200 MiB、永久保存。每次任务记录配置快照。
- MusicXML / XML / MXL 安全导入，保留原谱记谱，生成分谱、MIDI、合成 WAV/MP3、总谱/分谱 PDF、播放时间线。反复/跳房子只做直线播放并提示警告。
- 本地 Demucs + Basic Pitch ONNX + 鼓检测真实音频链路，无人工注入鼓点或固定歌曲参数；输出原始 note events、量化谱面和关联时间线。
- OSMD 总谱/分谱、缩放、原曲/合成/真实分轨共用一个播放器，播放、暂停、停止、seek、音量及近似谱面游标。源时间轴与合成时间轴分别映射。
- 私有文件 API 每次鉴权，包括 Range；逐项产物失败、partial 状态；outbox、任务 lease/心跳、重复领取防护、过期回收、期限清理和审计。
- CPU Compose、GPU override、非 root 容器、内部数据库/队列网络、单次 Alembic 迁移。

## CPU 容器启动

需要 Docker Engine / Compose v2，建议至少 8 GiB 可用内存和足够磁盘。模型和镜像准备需要联网；准备完成后处理服务使用内部网络，无推理外网。

```bash
cp .env.example .env
# 编辑 .env：设置随机 POSTGRES_PASSWORD，避免 URL 特殊字符，或正确 URL 编码。
mkdir -p data models
sudo chown 10001:10001 data
python3 scripts/prepare-models.py --download
# 模型准备动作只下载公开权重，不读取用户音频；校验 SHA256。
docker compose up -d --build
# 通过交互式隐藏输入设置管理员密码，至少 12 个字符，无默认管理员账号。
docker compose exec api musicd init-admin --username admin
```

打开 `http://localhost:8080`。管理员先登录，其他用户自行注册，管理员在「账号审核」批准后可登录。没有模型时账号和 MusicXML 功能仍可运行，音频上传会返回 `MODEL_NOT_READY`。

离线预置模型：

```bash
python3 scripts/prepare-models.py \
  --demucs-checkpoint /offline/5c90dfd2-34c22ccb.th \
  --basic-pitch-onnx /offline/nmp.onnx
```

模型来源、版本和 SHA256 见 [models.lock.json](models.lock.json)。容器不内置权重。准备脚本也支持 `--output`；自定义目录同步修改 `.env` 的 `MODELS_PATH`。`data` 由 UID/GID 10001 写入，模型挂载只读。模型不能用自行更换的 pickle checkpoint 替代：运维须信任并核对模型来源。

```bash
docker compose ps
docker compose logs -f worker dispatcher
docker compose exec api musicd health --role api
docker compose exec worker musicd health --role worker
# 保留持久卷与 data，重启不删除结果。
docker compose down
docker compose up -d
```

## 公网 HTTPS

默认 `.env.example` 是仅监听回环地址的本机 HTTP 配置。公网由现有 HTTPS 代理转发到 `127.0.0.1:8080`；设置 `PUBLIC_ORIGIN=https://music.example.com`、`COOKIE_SECURE=true`，保持整个站点同源。示例 Caddy：

```caddyfile
music.example.com {
    reverse_proxy 127.0.0.1:8080
}
```

仅 web 公开入口，PostgreSQL、Redis、API、worker 不映射宿主端口。上传硬上限在 Nginx 为 2049 MiB（含 multipart 开销）；实际用户限额以 API 管理配置为准，最大允许 2 GiB。代理也须允许对应大小和上传时间。写请求做 Origin / Fetch Metadata 校验；会话不存 localStorage。

运行 worker 是可信共享进程，没有宣称每用户独立沙箱。建议为 worker 设置宿主内存/CPU/磁盘配额，按并发 1 开始。时间限制由 `STAGE_TIMEOUT_SECONDS` 控制；自动 OOM 重试仅将同模型 Demucs 分块从 7 秒降至 4、2 秒，仍失败返回错误。

## GPU 候选配置

```bash
docker compose -f compose.yml -f compose.gpu.yml up -d --build
```

override 只替换同一个 worker。候选组合为 Torch / Torchaudio 2.5.1 + CUDA 11.8，float32、并发 1；**未在 Tesla P4 运行验收**。需 NVIDIA 驱动与 NVIDIA Container Toolkit，先在 worker 内检查 `nvidia-smi`、Torch CUDA / sm_61，再跑短样本完整任务。显式 `cuda` 不可用会失败，`auto` 可选 CPU 并记录原因。CPU / CUDA 使用同一 baseline 模型，不静默更换模型。多乐器路线、P4 显存/吞吐量/准确度评估仍属阶段 B。

## 本地开发与测试

Go 1.24+，Python 3.11（仅流水线），Node 22.14+，FFmpeg / ffprobe、FluidSynth、MuseScore 3、GM SoundFont。模型依赖使用独立 Python venv，避免 Basic Pitch 强制安装 TensorFlow。

```bash
uv venv --python 3.11 .venv
uv pip install --python .venv/bin/python -r backend/requirements.txt
uv venv --python 3.11 .model-venv
uv pip install --python .model-venv/bin/python torch==2.5.1 torchaudio==2.5.1 \
  --index-url https://download.pytorch.org/whl/cpu
uv pip install --python .model-venv/bin/python -r backend/requirements-models.txt
uv pip install --python .model-venv/bin/python --no-deps basic-pitch==0.4.0
(cd frontend && npm ci)
make test          # Go backend, Python pipeline and frontend
make build         # binary at .builds/bin/musicd
```

本地开发需要 Go 1.24+、Python 3.11 与前端 Node。`cd backend && go build -o /tmp/musicd ./cmd/musicd`，分别启动 `/tmp/musicd api`、`/tmp/musicd worker`、`/tmp/musicd dispatcher`；先设置 `DATABASE_URL`、`REDIS_URL`、`DATA_DIR`、`PUBLIC_ORIGIN`、`COOKIE_SECURE=false` 与 `PIPELINE_PYTHON`，并运行 `/tmp/musicd migrate` / `init-admin`。前端 `npm run dev`，代理默认 `http://127.0.0.1:8000`，可由 `API_PROXY_TARGET` 覆盖。worker 的 `PIPELINE_PYTHON` 必须能 import `pipeline.cli`。

模型运行环境变量：`MODEL_PYTHON` 为模型 venv 绝对路径，`DEMUCS_REPO`、`BASIC_PITCH_MODEL_PATH` 指向已准备的模型，`SOUNDFONT_PATH` 指向 `.sf2`，`MUSESCORE_BIN=musescore3`。API 不需要安装 Torch。依赖主版本已锁定；完整可重现性还受系统包、传递依赖和镜像补丁影响，部署应保存构建镜像 digest。

`scripts/make-demo.py` 可生成明确标注的合成测试 MusicXML/MIDI，不是歌曲或模型结果。

`scripts/smoke.py` 对实际服务执行审核、并发上传、资源归属/Range、产物下载及账号禁用验收，会创建 `smoke_` 测试账号和任务。管理员密码从标准输入读取：

```bash
.venv/bin/python scripts/smoke.py --url http://localhost:8080 --admin admin \
  --xml /path/to/test.musicxml --audio /path/to/short.wav < /private/admin-password-file
```

## 验证范围与限制

运行记录见 [docs/validation.md](docs/validation.md)。已跑真实 CPU 短音频与 191 秒完整参考歌曲，并浏览器检查谱面与播放。任务成功表示处理链与产物生成成功，不能证明识别准确。没有逐音符标注集或听写准确率评测。

当前谱面采用工具默认量化、默认 4/4 和估计恒定速度；复杂节拍和速度变化的音频准确性有限。导入 XML 的速度变化使用谱面 tempo map。多声部自动布局可能拥挤，不支持网页编辑。复杂反复不展开；压缩导入只支持安全 `score-partwise`，不支持 `score-timewise`。游标是近似位置同步。浏览器下载大文件会占用内存。任务/用户列表显示最近 200 项，尚无分页。运行失败的详细日志保留在服务日志，私有中间目录会清理。

长期存储会持续增长。备份应同时保存 PostgreSQL 和 `data` 卷（暂停写入后取得一致快照），模型可凭锁文件重新准备。修改 retention 只影响新建任务的配置快照，已存在任务期限不批量缩短。磁盘低于部署低水位会拒绝新任务，不自动删除永久保存结果。

第三方许可与音色说明见 [docs/third-party.md](docs/third-party.md)。
