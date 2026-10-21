# 阶段 A 验证记录

此记录描述实际执行的检查，不将产物生成成功等同于歌曲还原准确。阶段 B 的任意乐器识别、候选多乐器模型、Tesla P4 实测尚未完成。

## Go 后端迁移验证（最新）

后端改为 Go：账号、授权、上传、管理、outbox、dispatcher 和 worker 外壳在 `musicd`；模型、music21 和导出仍在 Python `pipeline.cli` 子进程。队列使用仓库内 Redis Streams 实现。

- `cd backend && go test ./...` 在专用 PostgreSQL `mt-go-test-postgres` 与 Redis 上通过（未设置测试库时集成测试自动 skip）。覆盖 Go API 鉴权/同源/quota/Range、worker 租约与 fencing、MQ ACK/重投/DLQ/长任务续租/Close、producer 连接回归。
- 既有生产 schema 与用户/会话表由 GORM 只增列迁移；`api/migrate/dispatcher/worker` 全部健康，`audio_ready=true`。
- 真实音频（4 秒合成 WAV）经 Go dispatcher → Redis Stream → Go worker → Python 子进程完成：`succeeded`，2 个粗分类声部、6 条 Demucs 音轨、26 个产物，PDF 下载为有效 PDF，Range 返回 206。
- MusicXML 导入经 Go API → Go worker → Python 流水线：`succeeded`，12 个产物；浏览器 本机部署地址 登录、OSMD 总谱渲染、播放及游标跟随正常。
- 修复集成缺陷：dispatcher 的 MQ producer 未 Connect 导致任务永远 queued；nginx 缓存 API 容器 IP 导致重建后 502（改为 Docker DNS 动态解析）。

## 阶段 A 自动检查（Python 时代）

- 后端 `PYTHONPATH=backend .venv/bin/pytest backend/tests -q`：28 项通过。涵盖审核/登录/同源请求、资源归属、Range、quota、outbox、租约领取与旧 token 写回、取消、删除、独立 stem ID、部分成功、期限计算、文件锁、实体与压缩 XML 防护、tempo map、全部鼓件 MusicXML 通道 10 和 MIDI 鼓映射。
- 前端 `npm run build`：TypeScript 与 Vite 构建通过。OSMD bundle 较大，已拆为延迟加载块，构建给出体积提示。
- 前端 `npm test`：21 项通过，涵盖事件时间映射、原曲/合成/分轨、失败产物下载、认证登录/退出状态更新。
- Alembic 首次迁移在本机 PostgreSQL 与 Compose PostgreSQL 实际执行。测试使用独立 SQLite，禁止继承生产数据库以免 drop_all 影响用户数据。
- `docker compose config` 和 GPU override 配置解析通过。CPU runtime/web/worker 镜像实际构建，生产 Compose 六个常驻服务均 healthy。

## 真正 CPU 推理

环境：Python 3.11.14；Torch / Torchaudio 2.5.1 CPU；Demucs 4.1.0；Basic Pitch 0.4.0 ONNX；ONNX Runtime 1.22.0；music21 9.7.1；并发 1、数值库线程 2。未使用 TensorFlow 或云端 API。权重 SHA256 见模型锁文件。

| 输入 | 路径性质 | 结果 | 耗时与产物 |
| --- | --- | --- | --- |
| 4 秒合成钢琴/贝斯 WAV | `scripts/make-demo.py` 生成的测试 fixture，明确不是歌曲/模型结果 | succeeded | 本机实际 Celery 队列约 47 秒；2 个粗分类声部、6 条真实 Demucs 音轨，全部导出可用 |
| 191 秒《浮尘》参考 WAV | 已有本机参考工程的完整音频，仅本地读取，不提交/再分发 | succeeded | 本机实际 PostgreSQL + Redis + Celery + dispatcher；约 424.84 秒；6 个粗分类/未知/鼓声部、34 个可用产物（含原文件） |
| MusicXML 合成 fixture | 非模型结果 | succeeded | Compose 容器内 4.13 秒；原谱、分谱、MIDI、总谱/分谱 PDF、WAV/MP3、timeline |
| 4 秒合成 WAV | 同上 | succeeded | Compose 非 root CPU worker，内部网络（无推理外网），84.80 秒；26 个可用产物（含原文件） |

完整歌曲生成 MusicXML 总谱约 3.9 MB、MIDI 51 KB、6 份分谱/PDF、总谱 PDF 323 KB、合成 WAV 33.9 MB、timeline 2.3 MB，以及全部分离 WAV/MP3。分离器仍只能区分粗类别；这首输入生成六个声部并不证明所有实际乐器都被识别。人声泄漏、错音、鼓类错判、量化及布局均未做人工准确率评测。不存在可声明的 F1、置信度、P4 吞吐量或 SLA。

## 浏览器实际检查

使用本机 Chromium 与 agent-browser 访问真实 API / 真实异步任务，未 mock 网络结果。

- 管理员 Cookie 登录、任务上传/轮询到成功；MusicXML 导入只出现合成音源，不虚构原曲或分轨。
- MusicXML 总谱 SVG 渲染、分谱选择、播放后位置从第 1 拍推进到后续谱面事件。
- 短音频原曲、合成、真实 bass 分轨可解码播放，切换暂停旧源；进度由 HTMLAudioElement currentTime 映射。
- 完整歌曲总谱在浏览器生成 SVG，显示全部六个实际输出声部，包括未知/混合与鼓；原曲播放完成游标可定位至后续小节。没有声称逐音符高亮准确。
- 发现并修复了登录后 Query cache 清空导致已挂载 auth observer 不更新的问题，新增登录/退出回归测试。
- 发现并修复分离 stem ID 被错误当作谱面 track 外键、导出下载名无扩展名、多个鼓件 MusicXML 通道被工具分配到旋律通道的问题。

截图和输入/输出留在本机被 gitignore 的 `data` 目录，避免提交用户文件和音频。GPU 环境不可用；GPU override 仅是候选部署，未运行 GPU 推理。

真实 HTTP `scripts/smoke.py` 在更新后的 CPU Compose 上通过：两账号注册/审核，5 个并发上传请求、跨账号任务/文件/Range 404、管理员 Range 206，MusicXML 与音频异步处理、每项真实下载、禁用账号后 API 与文件 401。该检查创建单独 smoke_ 账号，无模拟推理。

## 仍需验证或限制

- 多乐器引擎未集成；YourMT3 / MT3 等候选许可证、权重、CPU/P4 可用性和准确度需独立评估。
- 尚未执行真实 GPU OOM、进程断电、磁盘写满和长时间并发压力实验。相关状态与 fencing 路径有测试，不能替代故障注入实机测试。
- 文件锁保证同任务旧 attempt 不与删除/新 attempt 同时写文件；锁文件在私有根目录保留为轻量协调对象，未存原始音频。
- 导入反复/跳房子不展开，音频自动恒定速度/默认 4/4 有限制；游标近似，同步准确率未标注评测。
- PDF/音色依赖在 Debian 容器实际可用；字体和复杂输入的排版覆盖有限。
- 界面桌面优先；未对每个移动设备、浏览器及编解码格式做完整兼容性矩阵。
- 主依赖/权重版本固定；系统包和传递依赖并非完全 hermetic 锁定，部署时保存镜像 digest。模型准备下载源需按上游可用性维护。
