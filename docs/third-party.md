# 第三方组件与许可记录

这是构建来源与核对入口，不是法律意见。公开部署和再分发前应按所用确切版本复核代码、权重、字体、编解码器和音色许可。

| 组件 | 来源 | 许可入口 / 处理 |
| --- | --- | --- |
| Demucs 4.1.0 / htdemucs_6s | https://github.com/facebookresearch/demucs | 上游 MIT；权重单独由管理员下载，SHA256 见 models.lock.json |
| Basic Pitch 0.4.0 / ICASSP 2022 ONNX | https://github.com/spotify/basic-pitch | 上游 Apache-2.0；权重提取自同版公开 wheel，不随仓库提交 |
| music21 9.7.1 | https://github.com/cuthbertLab/music21 | 上游 BSD；只使用转换能力，不随本服务再发布其示例音乐 |
| OpenSheetMusicDisplay | https://github.com/opensheetmusicdisplay/opensheetmusicdisplay | BSD-3-Clause；npm 安装，构建后本地提供 |
| React / Go / GORM / pgx | 各上游仓库与包元数据 | MIT / BSD / Apache-2.0，按 go.sum 核对 |
| pkg/mq Redis Streams | 仓库内实现 | 只依赖 go-redis 与 google/uuid，详见 backend/pkg/mq/README.md |
| MuseScore 3 | Debian musescore3 软件包 | GPL；容器保留系统 /usr/share/doc 版权文件，源码按 Debian 包入口获取 |
| FluidSynth | Debian fluidsynth 软件包 | LGPL；保留系统包版权说明 |
| TimGM6mb SoundFont | Debian timgm6mb-soundfont | Debian 包版权记录；容器复制原版权到 /soundfonts/LICENSE，音色无第三方 CDN 请求 |
| FFmpeg | Debian ffmpeg 软件包 | 实际构建包含 LGPL/GPL 编解码配置；按 Debian 包和 ffmpeg -L 核查 |
| DejaVu fonts | Debian fonts-dejavu-core | 保留系统包版权文件 |

依赖和模型准备阶段可能访问公开包/权重仓库。运行处理服务无外部推理请求；Compose worker/API/dispatcher 仅接内部网络。用户上传文件的使用权由部署者和上传者处理；仓库不提交参考歌曲、权重或用户结果。
