# immich2motion

把 Immich 图库里的苹果 **Live Photo**（HEIC + MOV）转换成 **Google 相册可识别的 Motion Photo**（JPG 容器 + 尾部拼接视频 + XMP `MicroVideo` 元数据），输出目录交给 Syncthing 同步到 Pixel，由 Pixel 的 Google 相册 App 自动备份为「动态照片」。

单 Go 二进制 + 内嵌 Web 面板 + SQLite，一个容器装完 `heif-convert` 与 `exiftool`。

```mermaid
flowchart LR
    A["Immich 图库<br/>/source（只读）"] -->|递归扫描| B["配对<br/>ContentIdentifier UUID"]
    B --> C["HEIC → JPG<br/>heif-convert"]
    C --> D["复制 EXIF 时间/GPS<br/>写 XMP MicroVideo"]
    D --> E["JPG + MOV<br/>原流拼接"]
    E --> F["输出 /output<br/>保留目录结构"]
    F -->|Syncthing| G["Pixel"]
    G -->|Google 相册备份| H["动态照片"]
    B -.-> I[("SQLite /data")]
    D -.-> I
    E -.-> I
    I -.-> J["Web 面板<br/>:8080"]
```

---

## 核心特性

| 能力 | 说明 |
| --- | --- |
| **UUID 配对** | 只认 `ContentIdentifier`，文件名、目录名、修改时间都不参与配对 |
| **增量扫描** | mtime 未变的文件不重复解析元数据、不重复算 MD5；已成功的任务不重复转换 |
| **MD5 完整性校验** | 源文件与产物都记 MD5；支持周期性「深度校验」发现 `mtime 未变但内容变了` 的静默损坏（bit rot） |
| **并发控制** | 可配置并发数（默认 2），任务队列持久化在 SQLite，改并发数**立即生效**无需重启 |
| **容错** | 失败指数退避重试（默认 3 次），崩溃后 `running` 任务自动打回队列，全程事务化 |
| **源目录只读** | 代码层面只以只读方式打开源文件，输出路径做双重越界校验，拒绝写入源目录 |
| **Web 面板** | 仪表盘 / 任务列表 / 实时日志 / 设置，htmx 局部刷新 + SSE 实时推送，构建期打进二进制 |
| **界面** | 玻璃拟态 + 极光背景，按浏览器本地时间（06:00–18:00）自动切换日夜配色，`?theme=light/dark` 可覆盖 |
| **Webhook** | 任务开始/完成/失败/系统故障可分别开关，同类消息在时间窗口内合并成一条；自动适配钉钉/飞书/企微/Slack/Discord/ntfy/Bark |
| **CLI** | `--web`、`--scan-once`、`--dry-run`，适合 cron 定时跑 |

---

## 快速开始

> 镜像已发布：`docker pull andreasgyh/immich2motion:latest`
>
> 只想跑起来用上面的 pull 即可，不需要 clone 仓库、也不需要本地构建。

### 1. docker compose（推荐）

```bash
git clone https://github.com/immich2motion/immich2motion.git
cd immich2motion
```

编辑 `docker-compose.yml` 里的三条挂载路径：

```yaml
volumes:
  - /volume1/photo/immich/upload:/source:ro   # 源图库，必须 :ro
  - /volume1/photo/motion:/output             # 输出目录，Syncthing 同步这里
  - immich2motion_data:/data                  # SQLite 持久化（具名卷，见下方说明）
```

```bash
docker compose up -d      # 直接拉取镜像，不会本地构建
docker compose logs -f
```

打开 `http://<NAS-IP>:8080`。

> **`/data` 为什么用具名卷**
>
> SQLite 处于 WAL 模式，依赖 `mmap` 与文件锁。Windows/macOS 上 Docker Desktop 的
> bind mount（9p/drvfs）对这两者支持不完整，实测表现为「容器内已提交、宿主机读到旧快照」，
> 容器被强杀时还可能丢数据。
>
> Linux/NAS 上 bind mount 完全没问题，改成 `./data:/data` 即可。

### 2. 直接 docker run

```bash
docker run -d --name immich2motion \
  --restart unless-stopped \
  -p 8080:8080 \
  -v /volume1/photo/immich/upload:/source:ro \
  -v /volume1/photo/motion:/output \
  -v immich2motion_data:/data \
  -e TZ=Asia/Shanghai \
  -e I2M_WORKERS=2 \
  andreasgyh/immich2motion:latest
```

### 3. 自己构建镜像

改过代码、或想固定某个提交时：

```bash
docker build -t immich2motion:local .
```

国内网络下 Dockerfile 默认已指向 `docker.m.daocloud.io` 与 `goproxy.cn`；
能直连官方源时传 `--build-arg REGISTRY=docker.io/library/ --build-arg GOPROXY=https://proxy.golang.org,direct`。

备份数据库：

```bash
docker run --rm -v immich2motion_data:/data -v "$PWD:/backup" alpine \
  tar czf /backup/immich2motion-data.tar.gz -C /data .
```

### 4. 第一次使用建议

1. 打开 **仪表盘 → Dry-Run**，只做扫描与配对，确认「配对组数」符合预期；
2. 打开 **任务列表**，检查 UUID、HEIC 路径、视频路径是否成对；
3. 确认无误后关闭 Dry-Run，任务立即开始转换；
4. 定期看 **日志** 页的 `warn` / `error`。

---

## 配置

优先级：**Web 设置页（存 SQLite）> 环境变量 > 内置默认值**。
环境变量只在首次建库时作为初始值写入，之后以 Web 为准。

| 环境变量 | 默认值 | 说明 |
| --- | --- | --- |
| `I2M_SOURCE_DIR` | `/source` | 源图库目录（只读挂载） |
| `I2M_OUTPUT_DIR` | `/output` | 输出目录 |
| `I2M_DATA_DIR` | `/data` | 数据目录（SQLite） |
| `I2M_WEB_ADDR` | `:8080` | Web 监听地址 |
| `I2M_LOG_LEVEL` | `info` | `info` / `warn` / `error` |
| `I2M_WORKERS` | `2` | 并发转换数（1-16） |
| `I2M_JPG_QUALITY` | `100` | 传给 `heif-convert -q` 的质量 |
| `I2M_ENABLE_MD5` | `true` | MD5 完整性校验总开关 |
| `I2M_AUTO_SCAN` | `true` | 是否后台持续扫描 |
| `I2M_SCAN_INTERVAL_SEC` | `300` | 扫描间隔（秒，最小 10） |
| `I2M_MAX_RETRIES` | `3` | 失败重试次数 |
| `I2M_OUTPUT_SUFFIX` | `-motion` | 输出文件名后缀 |
| `I2M_EXIFTOOL` | `exiftool` | exiftool 可执行文件路径 |
| `I2M_HEIF_CONVERT` | `heif-convert` | heif-convert 可执行文件路径 |

Web 设置页还可以改：输出文件名后缀、HEIC/视频扩展名、深度校验周期、日志保留条数、是否转换前校验源 MD5、是否保留原始 Orientation。

---

## 目录行为与环境自检

三个挂载路径的处理策略不同，这是刻意的：

| 路径 | 不存在时 | 原因 |
| --- | --- | --- |
| `/source` 源目录 | **不创建，直接报错** | 自动建个空目录只会把「挂载漏了」表现成「扫描成功但 0 个文件」，比报错难排查得多 |
| `/output` 输出目录 | **自动创建** | 程序自己的产出位置，建目录是合理默认行为 |
| `/data` 数据目录 | **自动创建** | SQLite 必须能落盘 |

启动时会自动执行一次环境自检，结果直接显示在仪表盘顶部：

- **阻断项** —— `exiftool`、`heif-convert`、输出目录可写、数据目录可写。
  任一项不通过 → 横幅显示 **「转换已暂停」**，worker 停止领取任务，**队列原样保留**（不会把任务逐条重试成 `failed`）。
- **非阻断项** —— 源目录是否存在 / 可读。
  只有这一项失败时横幅显示 **「仅影响扫描」**，转换能力不受影响。

修好挂载或装好工具后，点横幅右上角的 **「重新检测」**（或全局操作区的「重新检测环境」）即可恢复，**不需要手工重跑失败任务**。

保存设置时的校验：

- 源目录**发生变化**时才强制要求它已存在（只改并发数、质量不会被路径问题卡住）；
- 输出目录自动创建；
- 输出目录落在源目录内部、或二者互相包含，会被拒绝并提示。

---

## 输出格式

输出是一个**单文件**：

```
[ JPG 数据 ][ MOV 数据 ]
|<-- N 字节 -->|
```

- XMP 写入：
  - `GCamera:MicroVideo = 1`
  - `GCamera:MicroVideoVersion = 1`
  - `GCamera:MicroVideoOffset = N`（N = JPG 字节长度）
- `mov` **原样拼接，不转码**，字节数与源文件完全一致。

### 为什么 `MicroVideoOffset` 要迭代写入

`exiftool` 改写 XMP 会改变 JPG 文件长度，而 `MicroVideoOffset` 必须**恰好等于**最终 JPG 长度——两者互相依赖。实现上做迭代收敛：

```
写 offset=0 → 量实际长度 L1
写 offset=L1 → 量实际长度 L2
若 L2 == L1 收敛；否则继续，最多 6 次
```

拼接前还会**回读校验** `MicroVideo=1` 且 `MicroVideoOffset == 文件长度`，不通过直接判失败，绝不产出坏图。

> 没有采用新版的 `GContainer:Directory`（`Item:Length` / `Semantic=Motion`）写法：
> 它要求同时写容器目录，字段更多、兼容性差异更大，而你要求的 `MicroVideo*` 是 Pixel 一代起就在用的格式，Google 相册识别稳定。

---

## CLI

```bash
# 默认：启动 Web 服务（容器入口）
immich2motion --web

# 扫描一次 + 处理完队列后退出（适合 cron）
immich2motion --scan-once

# 只扫描配对，不转换，打印统计后退出
immich2motion --dry-run

# 打印版本
immich2motion --version
```

容器内执行：

```bash
docker exec -it immich2motion immich2motion --scan-once
```

---

## Web 面板

| 页面 | 内容 |
| --- | --- |
| **仪表盘** `/` | 待处理/处理中/成功/失败统计、当前转换任务与进度条、速度与剩余时间、全局操作按钮、运行信息 |
| **任务列表** `/jobs` | UUID、HEIC/视频路径与大小、状态、进度、错误信息、单条重转；支持状态筛选、路径/UUID 搜索、分页 |
| **日志** `/logs` | SSE 实时滚动日志，按 info/warn/error 过滤，自动滚动可关闭，可清空 |
| **设置** `/settings` | 目录、并发、质量、后缀、扫描间隔、重试、MD5 相关开关、扩展名、日志保留 |

全局操作：**立即扫描 / 暂停 / 恢复 / Dry-Run / 清空失败标记 / 重跑失败项 / 深度 MD5 校验 / 重新检测环境**。

界面按浏览器本地时间自动切换：06:00–18:00 日间（浅色玻璃），其余夜间（深色玻璃）。
想在固定主题下调试，URL 加 `?theme=light` 或 `?theme=dark` 即可。

---

## 通知（Webhook）

在「设置 → 通知（Webhook）」里配置，支持四类事件独立开关：

| 事件 | 触发时机 | 默认 |
| --- | --- | --- |
| 任务开始 | 每次开始转换 | 关（量大） |
| 任务完成 | 单个任务转换成功 | 开 |
| 任务失败 | 重试耗尽或不可恢复 | 开 |
| 系统故障 | 环境自检失败、扫描失败 | 开 |

**消息合并**：同一类事件在窗口内合并成一条，默认窗口 **180 秒（3 分钟）**，可改；
填 `0` 表示不合并、立即发送。单桶最多 50 条，超过会提前发出。

```
[immich2motion] 3 个任务转换成功
· 2024/05/IMG_4939-motion.jpg
· 2024/05/IMG_4941-motion.jpg
· 2024/06/IMG_4942-motion.jpg
```

**格式自动适配**（按域名判断，不需要选平台）：

| 目标 | 报文 |
| --- | --- |
| 钉钉 `oapi.dingtalk.com` | `{"msgtype":"text","text":{"content":…}}` |
| 飞书 `open.feishu.cn` | `{"msg_type":"text","content":{"text":…}}` |
| 企业微信 `qyapi.weixin.qq.com` | 同钉钉 |
| Slack / Discord | `{"text":…}` / `{"content":…}` |
| ntfy | JSON 发布（中文标题不会被 header 限制破坏） |
| Bark `api.day.app` | `{"title":…,"body":…}` |
| 其他地址 | 通用 JSON（见下） |

通用 JSON 便于自建接收端做结构化处理：

```json
{
  "source": "immich2motion",
  "event": "task.success",
  "title": "[immich2motion] 3 个任务转换成功",
  "message": "· A1/IMG_4939-motion.jpg\n· A1/IMG_4941-motion.jpg",
  "count": 3,
  "items": ["A1/IMG_4939-motion.jpg", "A1/IMG_4941-motion.jpg"],
  "at": "2026-11-18T10:36:50+08:00",
  "platform": "generic"
}
```

设置页有「发送测试消息」按钮（用已保存的配置立即发一条，不走合并窗口）。
推送失败只记日志并重试一次，**不会阻塞转换**。

---

## 数据库

SQLite（`modernc.org/sqlite`，纯 Go 无 CGO），WAL 模式，位于 `/data/immich2motion.db`。

```sql
-- 源文件清单：HEIC 与视频各一行
CREATE TABLE live_assets (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    source_root     TEXT    NOT NULL,              -- 源根目录，换库不互相污染
    rel_path        TEXT    NOT NULL,              -- 相对路径
    kind            TEXT    NOT NULL CHECK (kind IN ('heic','video')),
    size_bytes      INTEGER NOT NULL DEFAULT 0,
    mtime_unix      INTEGER NOT NULL DEFAULT 0,
    content_id      TEXT,                          -- ContentIdentifier（配对唯一依据）
    cid_source      TEXT,                          -- 命中的 exiftool 标签组
    md5             TEXT,
    md5_mtime_unix  INTEGER,                       -- 计算 MD5 时的 mtime，用于增量复用
    md5_checked_at  INTEGER,
    state           TEXT    NOT NULL DEFAULT 'ok', -- ok|no_cid|unreadable|bitrot|missing
    warning         TEXT,
    first_seen_at   INTEGER NOT NULL,
    last_seen_at    INTEGER NOT NULL,
    last_scan_id    INTEGER NOT NULL DEFAULT 0,
    updated_at      INTEGER NOT NULL
);
CREATE UNIQUE INDEX ux_assets_root_path ON live_assets(source_root, rel_path);
CREATE INDEX ix_assets_cid ON live_assets(source_root, content_id);

-- 配对结果：一行 = 一组已确认的 Live Photo
CREATE TABLE live_pairs (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    source_root    TEXT    NOT NULL,
    content_id     TEXT    NOT NULL,
    heic_asset_id  INTEGER NOT NULL REFERENCES live_assets(id) ON DELETE CASCADE,
    video_asset_id INTEGER NOT NULL REFERENCES live_assets(id) ON DELETE CASCADE,
    heic_rel_path  TEXT    NOT NULL,
    video_rel_path TEXT    NOT NULL,
    heic_mtime     INTEGER NOT NULL DEFAULT 0,     -- 源指纹快照，用于发现变化后重转
    video_mtime    INTEGER NOT NULL DEFAULT 0,
    heic_size      INTEGER NOT NULL DEFAULT 0,
    video_size     INTEGER NOT NULL DEFAULT 0,
    created_at     INTEGER NOT NULL,
    updated_at     INTEGER NOT NULL,
    UNIQUE (source_root, heic_asset_id, video_asset_id)
);

-- 转换任务：与配对一对一
CREATE TABLE jobs (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    pair_id       INTEGER NOT NULL UNIQUE REFERENCES live_pairs(id) ON DELETE CASCADE,
    status        TEXT    NOT NULL DEFAULT 'pending',  -- pending|running|success|failed|skipped
    stage         TEXT,
    progress      REAL    NOT NULL DEFAULT 0,
    attempts      INTEGER NOT NULL DEFAULT 0,
    max_attempts  INTEGER NOT NULL DEFAULT 3,
    last_error    TEXT,
    next_retry_at INTEGER,                             -- 指数退避
    started_at    INTEGER,
    finished_at   INTEGER,
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL
);
CREATE INDEX ix_jobs_status ON jobs(status);

-- 输出产物
CREATE TABLE motion_outputs (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    pair_id      INTEGER NOT NULL REFERENCES live_pairs(id) ON DELETE CASCADE,
    job_id       INTEGER,
    source_root  TEXT    NOT NULL,
    rel_path     TEXT    NOT NULL,
    abs_path     TEXT    NOT NULL,
    size_bytes   INTEGER NOT NULL,
    jpg_bytes    INTEGER NOT NULL,     -- 等于 MicroVideoOffset
    video_bytes  INTEGER NOT NULL,
    md5          TEXT,                 -- 产物完整性校验
    xmp_verified INTEGER NOT NULL DEFAULT 0,
    created_at   INTEGER NOT NULL,
    verified_at  INTEGER,
    UNIQUE (source_root, rel_path)
);

-- 运行日志 / 扫描批次 / 键值设置 / bit rot 事件
CREATE TABLE logs             (id INTEGER PRIMARY KEY AUTOINCREMENT, ts INTEGER NOT NULL,
                               level TEXT NOT NULL, scope TEXT, message TEXT NOT NULL);
CREATE TABLE scans            (id INTEGER PRIMARY KEY AUTOINCREMENT, source_root TEXT NOT NULL,
                               trigger TEXT NOT NULL, started_at INTEGER NOT NULL, finished_at INTEGER,
                               files_seen INTEGER DEFAULT 0, heic_seen INTEGER DEFAULT 0,
                               video_seen INTEGER DEFAULT 0, pairs_total INTEGER DEFAULT 0,
                               pairs_new INTEGER DEFAULT 0, pairs_unpaired INTEGER DEFAULT 0,
                               warnings INTEGER DEFAULT 0, error TEXT);
CREATE TABLE settings         (key TEXT PRIMARY KEY, value TEXT NOT NULL, updated_at INTEGER NOT NULL);
CREATE TABLE integrity_events (id INTEGER PRIMARY KEY AUTOINCREMENT, asset_id INTEGER NOT NULL,
                               rel_path TEXT NOT NULL, kind TEXT NOT NULL, old_md5 TEXT, new_md5 TEXT,
                               detail TEXT, created_at INTEGER NOT NULL);
CREATE TABLE schema_meta      (key TEXT PRIMARY KEY, value TEXT NOT NULL);
```

---

## 项目结构

```
immich2motion/
├── cmd/immich2motion/main.go        # 入口：flag 解析、Web/CLI 模式分派、优雅退出
├── internal/
│   ├── config/                      # 启动期配置（env）+ 运行期设置（可持久化、可校验）
│   │   ├── config.go
│   │   └── settings.go
│   ├── store/                       # SQLite 持久化层
│   │   ├── db.go                    # 连接、PRAGMA、迁移、事务重试
│   │   ├── schema.go                # 全部建表 DDL
│   │   ├── assets.go / bulk.go      # 源资产 CRUD + 批量写（扫描性能关键）
│   │   ├── pairs.go                 # 配对登记
│   │   ├── jobs.go                  # 任务队列：原子领取、退避重试、崩溃恢复
│   │   ├── outputs.go               # 产物记录
│   │   ├── logs.go / scans.go       # 日志、扫描批次、完整性事件
│   │   └── settings.go              # 设置读写（JSON blob）
│   ├── library/                     # 图库感知层
│   │   ├── scanner.go               # 递归扫描 + 增量 + MD5 复用 + 配对入库
│   │   ├── exiftool.go              # 批量读取 ContentIdentifier（含标签组优先级）
│   │   ├── pair.go                  # 纯函数配对算法（同目录优先 + 一对一贪心）
│   │   └── hash.go                  # MD5
│   ├── convert/                     # 转换流水线
│   │   ├── pipeline.go              # HEIC→JPG、XMP 迭代写入、拼接、校验、原子落盘
│   │   └── tools.go                 # 外部命令执行与错误封装
│   ├── engine/                      # 调度
│   │   ├── engine.go                # 扫描循环、worker 池、状态机、统计
│   │   ├── sem.go                   # 可动态调整上限的信号量（并发数热生效）
│   │   └── events.go                # 事件总线 + 吞吐/ETA 估算
│   └── web/                         # 内嵌 Web 面板
│       ├── server.go                # 路由、模板装载、格式化函数
│       ├── handlers.go              # 页面与操作接口
│       ├── sse.go                   # SSE 实时推送
│       ├── templates/               # html/template（4 页面 + 局部片段）
│       └── assets/                  # go:embed：app.css / app.js / htmx.min.js
├── web/input.css                    # Tailwind 源文件
├── tailwind.config.js
├── Dockerfile                       # 多阶段构建
├── docker-compose.yml
└── .dockerignore
```

---

## 自动构建（GitHub Actions）

`.github/workflows/release.yml` 已配好：推送到 `main` 自动构建并推送到 Docker Hub（linux/amd64）。

需要在仓库 **Settings → Secrets and variables → Actions** 里加两个 secret：

| Secret | 说明 |
| --- | --- |
| `DOCKER_USERNAME` | Docker Hub 用户名 |
| `DOCKERHUB_TOKEN` | Docker Hub **Access Token**（不是登录密码） |

镜像名写在 `docker/metadata-action` 的 `images:` 一行，当前是 `${{ secrets.DOCKER_USERNAME }}/immich2motion`，
即发布到 **`andreasgyh/immich2motion`**。换仓库只改这一行。

标签规则：

| 触发 | 产出的标签 |
| --- | --- |
| 推送 `main` | `latest`、`sha-1a2b3c4` |
| 推送 `v1.2.3` 标签 | `1.2.3`、`1.2`、`1`、`sha-…` |
| 手动 Run workflow | 同上（按当前 ref 判定） |

CI 里显式传了 `REGISTRY=docker.io/library/` 和 `GOPROXY=https://proxy.golang.org,direct`，
所以构建直接走官方源、不绕国内加速；本地构建仍默认用 `docker.m.daocloud.io`。

---

## 开发

```bash
# 依赖（纯 Go，无 CGO）
go mod download

# 构建（前端资源已提交到仓库，无需 node 也能构建）
CGO_ENABLED=0 go build -o immich2motion ./cmd/immich2motion

# 改样式后重新编译 Tailwind
npx tailwindcss@3.4.17 -c tailwind.config.js -i web/input.css -o internal/web/assets/app.css --minify

# 单元测试（配对算法、标签解析等，无需外部工具）
go test ./...

# 集成测试：需要真实 exiftool + 一个 heif 解码器
I2M_TEST_EXIF_BIN=/usr/bin/exiftool \
I2M_TEST_HEIF_BIN=/usr/bin/heif-convert \
go test -tags integration ./... -v
```

集成测试覆盖：JPG+MOV 拼接字节级一致性、`MicroVideoOffset` 收敛、EXIF/GPS 保留、MD5、
源文件零改动、临时文件清理、扫描→配对→入队→转换→落盘的完整链路、重复扫描幂等。

---

## 设计取舍与已知限制

- **运行镜像基于 Debian trixie，不是 bookworm**。bookworm 自带 libheif 1.15.1 / exiftool 12.57（2022 年），
  实测在 iPhone 16 Pro Max 的照片上有两个硬伤：
  exiftool 读不出 HEIC 里 MakerNotes 的 `ContentIdentifier`（配对恒为 0）；
  libheif 解不了带多个辅助图（HDR gain map / sky matte / style delta map）的 HEIC，
  报 `Too many auxiliary image references`。trixie 的 libheif 1.23 + exiftool 13.25 均正常。
  这两个问题只在真实新机型照片上暴露，小图测试不会发现——如果你的图库全是老机型，
  bookworm 也能跑，但不建议。
- 附带的兼容处理：exiftool 13.x 会把大整数（如 `MicroVideoOffset`）的 JSON 输出成科学计数法，
  校验逻辑同时接受整数与浮点两种写法。
- **只输出 JPG**：HEIC 作为 Motion Photo 容器在 Google 相册侧不可靠，固定输出 `.jpg`。
- **视频不转码**：直接原流拼接，速度取决于磁盘 IO。若 Immich 里存的是 `.mp4`，按扩展名配置扫描即可（默认已包含）。
- **Orientation**：`heif-convert` 会把 `irot` 旋转变换直接作用到像素上，因此默认**不复制**原始 `EXIF:Orientation`，避免看图软件二次旋转。若你的 `heif-convert` 版本不应用旋转，可在设置页打开「保留原始 EXIF Orientation」。
- **配对不做模糊匹配**：UUID 不一致就是配不上，宁可报「未配对」也不猜。孤儿文件（有 HEIC 无视频 / 有视频无 HEIC）只统计不生成任务。
- **普通照片不入库任务**：没有 `ContentIdentifier` 的 HEIC 标记为 `no_cid`，不参与配对、不计孤儿，扫描时日志采样输出。
- **不读 Immich 数据库**：纯磁盘扫描。`internal/library/scanner.go` 的 `ScanOptions` 是扩展点，后续可加 Immich API 实现增量拉取。
- **权限**：容器默认以 root 运行以便写入宿主挂载目录。需要降权时在 compose 里加 `user: "1000:1000"`，并确保 `/output`、`/data` 对该 UID 可写。
- **healthcheck**：不依赖镜像里的 curl/wget，用二进制自身的 `--health-check` 子命令探测 `/healthz`。
- **源目录不自动创建**：见上文「目录行为」——宁可报错，也不要用一个空目录掩盖挂载错误。

### Syncthing 侧建议

在同步文件夹的 `.stignore` 里加：

```
(?i)*.tmp
(?).i2m-*
```

程序的临时文件是同目录的 `.i2m-*.jpg` / `.i2m-*.tmp`，转换完成后才原子 `rename` 成最终文件，正常情况下 Syncthing 不会看到半成品，加上忽略规则更保险。

Pixel 端建议让 Google 相册只**备份**该目录（选择「设备文件夹」），并保留原图；上传后 Google 相册会把 `MicroVideo` 识别为动态照片。

---

## License

MIT
