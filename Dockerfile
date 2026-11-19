# live2motion —— 多阶段构建：静态 Go 二进制 + heif-convert + exiftool，单容器部署
#
# 为什么运行阶段用 trixie（Debian 13）而不是 bookworm：
#   bookworm 自带 libheif 1.15.1 / exiftool 12.57（2022 年），实测两个致命问题：
#   1) exiftool 12.57 读不出 iOS 17+ 新 HEIC 的 Apple 元数据（连 EXIF 组都没有），
#      ContentIdentifier 无从获取 → 配对永远为 0；
#   2) libheif 1.15.1 解不了带多个辅助图的 HEIC（HDR gain map / sky matte /
#      style delta map 等），报 "Too many auxiliary image references"。
#   trixie 提供 libheif 1.23.x + exiftool 13.25，两者均正常。
#
# 关于 # syntax 指令：不写。该指令会先去 docker.io 拉 BuildKit 前端镜像，
# 国内网络常常拉不到（auth.docker.io 不可达）；Docker 内置前端同样支持 --mount=type=cache。

ARG GO_VERSION=1.23
# Go 模块代理：国内保留 goproxy.cn；海外可传 --build-arg GOPROXY=https://proxy.golang.org,direct
ARG GOPROXY=https://goproxy.cn,direct
# 基础镜像源：默认走国内加速；能直连 Docker Hub 时传 --build-arg REGISTRY=docker.io/library/
ARG REGISTRY=docker.m.daocloud.io/library/

# ============================================================================
# 构建阶段
#
# --platform=$BUILDPLATFORM：让编译阶段始终跑在宿主架构上（CI 里是 amd64），
# 通过 TARGETARCH 交叉编译到目标架构，避免用 QEMU 模拟整个 Go 工具链（慢 10 倍以上）。
# ============================================================================
FROM --platform=$BUILDPLATFORM ${REGISTRY}golang:${GO_VERSION}-bookworm AS builder

ARG GOPROXY
# buildx 多平台构建时自动注入；本地普通 docker build 不注入时留空，Go 回落到宿主架构
ARG TARGETOS
ARG TARGETARCH

WORKDIR /src

# 纯 Go 编译，不依赖 CGO（SQLite 使用 modernc.org/sqlite）。
# GOOS/GOARCH 在编译命令里显式求值：本地普通 docker build 不注入 TARGET*，
# 直接写 ENV GOARCH= 空值会让 Go 报 unsupported GOOS/GOARCH pair。
ENV CGO_ENABLED=0 \
    GOPROXY=${GOPROXY}

# 依赖层单独缓存，源码改动不会重新下载模块
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .

# 前端资源：仓库内已提交编译好的 app.css，可离线构建。
# REBUILD_CSS=true（默认）时从 GitHub 下载 Tailwind standalone 重新编译，
# 下载失败会自动回退到仓库内产物，不会让构建失败。
ARG REBUILD_CSS=true
ARG TAILWIND_VERSION=v3.4.17
RUN if [ "$REBUILD_CSS" = "true" ]; then \
      echo "→ 重新编译 Tailwind CSS ($TAILWIND_VERSION)"; \
      ( curl -fsSL --max-time 180 -o /tmp/tw \
          "https://github.com/tailwindlabs/tailwindcss/releases/download/${TAILWIND_VERSION}/tailwindcss-linux-x64" \
        && chmod +x /tmp/tw \
        && /tmp/tw -c tailwind.config.js -i web/input.css -o internal/web/assets/app.css --minify \
      ) || echo "⚠ Tailwind 重建失败，使用仓库内已编译的 app.css"; \
    fi

ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    export GOOS="${TARGETOS:-linux}" && \
    export GOARCH="${TARGETARCH:-$(go env GOHOSTARCH)}" && \
    echo "→ building for ${GOOS}/${GOARCH} (version=${VERSION})" && \
    go build -ldflags "-s -w -X github.com/AndreasByrsting/live2motion/internal/config.Version=${VERSION}" \
      -o /out/live2motion ./cmd/live2motion

# ============================================================================
# 运行阶段
# ============================================================================
FROM ${REGISTRY}debian:trixie-slim

LABEL org.opencontainers.image.title="live2motion" \
      org.opencontainers.image.description="Convert Apple Live Photos (HEIC+MOV) into Google-compatible Motion Photos" \
      org.opencontainers.image.source="https://github.com/AndreasByrsting/live2motion"

# libheif-examples 提供 heif-convert（1.23.x，能解 iOS 17+ 的多辅助图 HEIC）；
# libimage-exiftool-perl 提供 exiftool（13.25，能读新 HEIC 的 MakerNotes ContentIdentifier）。
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        libheif-examples \
        libimage-exiftool-perl \
        ca-certificates \
        tzdata \
    && rm -rf /var/lib/apt/lists/*

# 构建期验证外部工具可用性（heif-convert 的 --version 在部分版本返回非 0）
RUN (heif-convert --version || true) 2>&1 | head -1 \
    && echo -n "exiftool: " && exiftool -ver

RUN mkdir -p /source /output /data

COPY --from=builder /out/live2motion /usr/local/bin/live2motion

ENV L2M_SOURCE_DIR=/source \
    L2M_OUTPUT_DIR=/output \
    L2M_DATA_DIR=/data \
    L2M_WEB_ADDR=:8080 \
    L2M_LOG_LEVEL=info \
    TZ=Asia/Shanghai

# 仅 /data 需要持久化；/source 与 /output 由宿主目录挂载
VOLUME ["/data"]

EXPOSE 8080

# 不依赖 curl/wget，用程序自身的子命令探活
HEALTHCHECK --interval=30s --timeout=5s --start-period=25s --retries=3 \
    CMD ["/usr/local/bin/live2motion", "--health-check"]

ENTRYPOINT ["/usr/local/bin/live2motion"]
CMD ["--web"]
