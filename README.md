# 广域网大文件传输系统

一个基于 `tus` 断点续传协议的大文件中转站项目，适合广域网大文件上传、弱网续传、临时文件分享等场景。

项目提供 Vue 前端、Go 后端、MinIO/S3 对象存储、PostgreSQL 元数据存储、Nginx 网关和 Docker Compose 一体化部署。

## 功能特性

- 基于 `tus-js-client` + `tusd` 支持大文件断点续传。
- 前端上传页显示上传开始时间、结束时间、耗时、平均速度。
- 前端上传过程中显示实时速度，按最近 10 秒窗口计算 Mbps。
- 上传分片大小固定为 64MB，减少大文件上传时的小请求开销。
- 上传完成后自动生成分享码。
- 支持 API Key / Admin Key 鉴权。
- 支持管理员查看全部传输记录。
- 支持普通 API Key 查看自己的传输记录。
- 支持复制分享码、按记录直接下载。
- 支持 PostgreSQL 持久化分享码、下载次数、过期时间和 API Key 信息。
- 支持 MinIO 或其他 S3 兼容对象存储。
- Nginx 已针对大文件上传关闭请求缓冲并配置长超时。
- `deploy.sh` 自动读取 `.env.production` 并执行 Docker Compose 部署。

## 技术栈

- 前端：Vue 3 + Vite + Element Plus + `tus-js-client`
- 后端：Go + Gin + `tusd`
- 存储：MinIO / S3 兼容对象存储
- 数据库：PostgreSQL
- 网关：Nginx
- 部署：Docker Compose

## 目录结构

```text
.
├── backend/              # Go 后端服务
├── frontend/             # Vue 前端
├── nginx/                # Nginx 配置和证书目录
├── docker-compose.yml    # 容器编排
├── deploy.sh             # 一键部署脚本
└── README.md             # 项目文档
```

## 核心流程

1. 用户通过前端选择文件上传。
2. 前端通过 `/files/` 发起 tus 断点续传。
3. Nginx 将上传请求转发到 Go 后端。
4. 后端 `tusd` 将文件写入 MinIO/S3。
5. 上传完成后后端生成分享码并写入 PostgreSQL。
6. 用户可通过分享码查询文件并下载。
7. 管理员可查看所有传输记录；普通 API Key 用户可查看自己的传输记录。

## 快速启动

开发或测试环境可以直接运行：

```bash
docker compose up -d --build
```

旧版 Docker Compose 可使用：

```bash
docker-compose up -d --build
```

访问地址：

- 前端：`http://localhost`
- MinIO 控制台：`http://localhost:9001`

默认 MinIO 账号密码见 `docker-compose.yml`，生产环境必须修改。

## 生产部署

生产环境推荐使用仓库根目录的 `.env.production` 管理密钥和部署变量。

`.env.production` 示例：

```env
API_KEY=change-me-api-key
ADMIN_KEY=change-me-admin-key
DATA_ROOT=/data

DB_HOST=postgres
DB_USER=filecodebox
DB_PASSWORD=change-me-db-password
DB_NAME=filecodebox

S3_ENDPOINT=http://minio:9000
S3_PUBLIC_ENDPOINT=https://your-domain.com:9000
S3_BUCKET=uploads
S3_ACCESS_KEY=change-me-minio-user
S3_SECRET_KEY=change-me-minio-password
S3_REGION=us-east-1
```

部署前确认：

- 服务器已安装 Docker 和 Docker Compose。
- 数据盘已挂载到 `DATA_ROOT`，默认 `/data`。
- Nginx 域名、证书、私钥已经放到 `nginx/` 目录并在 `nginx/nginx.conf` 中配置正确。
- `.env.production` 已设置 `API_KEY` 和 `ADMIN_KEY`。

一键部署：

```bash
chmod +x deploy.sh
./deploy.sh
```

脚本会自动执行：

```bash
docker compose --env-file .env.production down
docker compose --env-file .env.production up -d --build
```

如果当前环境只有旧版 `docker-compose`，脚本会自动兼容。

## 数据目录

`docker-compose.yml` 默认使用数据盘路径：

- PostgreSQL：`${DATA_ROOT:-/data}/postgres`
- MinIO：`${DATA_ROOT:-/data}/minio`

后端不再额外挂载 `/data/uploads` 作为大文件持久化目录。当前 S3/MinIO 模式下，大文件最终写入 MinIO。

## 鉴权说明

系统支持两类密钥：

- `API_KEY`：普通上传、下载、查询文件使用。
- `ADMIN_KEY`：管理员功能使用，例如 API Key 管理和全量传输记录。

前端登录后会把密钥保存在浏览器本地存储中。访问管理员接口时，前端会优先携带 `Admin Key`。

## 上传优化

当前上传链路：

```text
浏览器 -> Nginx -> Go backend/tusd -> MinIO
```

已落地的上传优化：

- 前端 `chunkSize` 设置为 `64 * 1024 * 1024`。
- 前端实时显示最近 10 秒上传速度。
- 暂停/继续不会污染实时速度窗口。
- Nginx `/files/` 已设置：
  - `proxy_request_buffering off`
  - `proxy_buffering off`
  - `proxy_http_version 1.1`
  - 长上传超时
- Nginx 增加：
  - `worker_processes auto`
  - `worker_connections 8192`
  - `tcp_nopush on`
  - `tcp_nodelay on`
  - `proxy_socket_keepalive on`

后续如果 100Mbps/200Mbps 仍跑不满，可以继续测试 tus 并行上传、后端 S3 HTTP 连接池和对象存储直传。

## 传输记录

前端新增“传输记录”页：

- 文件名
- 分享码
- 上传时间
- 过期时间
- 下载次数 / 最大下载次数
- 状态：有效、已过期、已达上限
- 操作：复制分享码、下载

后端接口：

- `GET /api/files`：普通 API Key 查看自己的传输记录。
- `GET /api/admin/files`：管理员查看全部传输记录。

说明：

- 新上传文件会绑定当前 API Key 的 `owner_key_hash`。
- 历史上传记录如果没有归属信息，普通 API Key 可能看不到，只能由管理员查看。
- Admin Key 可查看全量记录。

## 常用接口

### 健康检查

```http
GET /api/health
```

### 验证密钥

```http
POST /api/verify-key
```

### tus 上传

```http
POST /files/
PATCH /files/:id
HEAD /files/:id
```

### 获取分享码

```http
POST /api/get-code
```

请求示例：

```json
{
  "upload_id": "upload-id"
}
```

### 根据分享码查询文件

```http
GET /api/retrieve/:code
```

返回示例：

```json
{
  "upload_id": "upload-id",
  "filename": "demo.zip",
  "url": "/files/Ab3K9xQe"
}
```

### API Key 管理

管理员接口：

```http
GET /api/admin/keys
POST /api/admin/keys
PATCH /api/admin/keys/:id
DELETE /api/admin/keys/:id
```

## Nginx 注意事项

大文件上传依赖以下配置，不建议移除：

```nginx
client_max_body_size 0;
proxy_request_buffering off;
proxy_buffering off;
proxy_read_timeout 86400s;
proxy_send_timeout 86400s;
```

`/files/` 是普通 HTTP 上传链路，不是 WebSocket，因此当前使用：

```nginx
proxy_set_header Connection "";
```

## 服务器配置建议

测试阶段：

- 2C4G
- 40G 系统盘
- 600G 或 1T 数据盘
- 100Mbps 固定带宽

更稳妥的大文件传输测试：

- 4C8G
- 1T 数据盘
- 200Mbps 固定带宽

500GB 文件理论传输时间：

- 100Mbps：约 11.1 小时
- 200Mbps：约 5.6 小时

实际速度还会受到客户端网络、运营商链路、Nginx、后端、MinIO、磁盘和上传协议并发能力影响。

## 生产安全建议

- 修改 PostgreSQL 默认密码。
- 修改 MinIO 默认账号和密码。
- 设置强 `API_KEY` 和 `ADMIN_KEY`。
- 证书私钥不要提交到 GitHub。
- `.env.production` 不要提交到 GitHub。
- 将 `CORS_ORIGINS=*` 改成明确域名。
- 将 `S3_PUBLIC_ENDPOINT` 改成真实可公网访问的地址。

## 后续可优化方向

- 增加下载明细日志表，记录每次下载时间、IP、User-Agent 和下载者 Key。
- 增加 tus 并行上传配置项。
- 给后端 S3 Client 增加自定义 HTTP 连接池。
- 支持 S3 multipart 预签名直传，减少后端中转压力。
- 增加定期清理过期文件和分享码的任务。
