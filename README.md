# 广域网大文件传输系统

一个基于 `tus` 断点续传协议实现的广域网大文件传输项目，适用于大文件、弱网、长时间上传等场景。项目提供前端上传/取件界面、Go 后端服务、分享码机制，以及基于 Nginx + Docker Compose 的一体化部署方案。

项目仓库：`wan-large-file-transfer`

## 项目特点

- 支持超大文件上传，基于 `tus` 协议实现断点续传
- 适合广域网传输，网络中断后可继续上传
- 上传完成后自动生成分享码，便于文件分发
- 支持 S3 兼容对象存储，可直接接入 MinIO / OSS / 其他 S3 服务
- 支持 PostgreSQL 持久化分享码与 API Key 信息
- 支持 API Key / Admin Key 鉴权
- 支持 Docker Compose 一键部署
- 提供 Nginx 反向代理与 HTTPS 接入能力

## 技术架构

- 前端：Vue 3 + Vite + Element Plus + `tus-js-client`
- 后端：Go + Gin + `tusd`
- 数据库：PostgreSQL
- 对象存储：MinIO（默认）或其他 S3 兼容存储
- 网关：Nginx

## 目录结构

```text
.
├─backend/              # Go 后端服务
├─frontend/             # Vue 前端页面
├─nginx/                # Nginx 配置与证书目录
├─docker-compose.yml    # 容器编排
├─deploy.sh             # 简单部署脚本
├─Dockerfile            # 根级镜像文件（预留）
└─nginx.conf            # 额外 Nginx 配置文件
```

## 核心流程

1. 用户通过前端选择文件上传。
2. 前端使用 `tus-js-client` 调用 `/files/` 发起断点续传上传。
3. 后端接收上传并写入本地存储或 S3 兼容对象存储。
4. 上传完成后，后端生成分享码并保存到 PostgreSQL。
5. 下载方输入分享码后，系统查询文件信息并返回下载链接。
6. 若使用 S3 存储，后端会生成带时效的预签名下载地址。

## 快速开始

### 1. 环境要求

- Docker
- Docker Compose

### 2. 本地启动

在项目根目录执行：

```bash
docker-compose up -d --build
```

启动后可访问：

- 前端首页：`http://localhost`
- MinIO 控制台：`http://localhost:9001`
- MinIO 默认账号：`minioadmin`
- MinIO 默认密码：`minioadmin`

### 3. 停止服务

```bash
docker-compose down
```

如果需要连同数据卷一起删除：

```bash
docker-compose down -v
```

## 默认部署说明

当前 `docker-compose.yml` 默认包含以下服务：

- `gateway`：Nginx 网关，对外暴露 `80/443`
- `frontend`：前端页面
- `backend`：上传与取件 API
- `postgres`：存储分享码、API Key 等元数据
- `minio`：对象存储

后端默认环境变量包括：

- `DB_HOST=postgres`
- `DB_USER=filecodebox`
- `DB_PASSWORD=filecodebox`
- `DB_NAME=filecodebox`
- `S3_ENDPOINT=http://minio:9000`
- `S3_PUBLIC_ENDPOINT=http://8.145.57.148:9000`
- `S3_BUCKET=uploads`
- `S3_ACCESS_KEY=minioadmin`
- `S3_SECRET_KEY=minioadmin`
- `S3_REGION=us-east-1`
- `TUSD_UPLOAD_DIR=/data/uploads`
- `SHARE_CODE_TTL_MINUTES=120`
- `SHARE_CODE_MAX_DOWNLOADS=10000`
- `CORS_ORIGINS=*`
- `ENABLE_DEBUG_ENDPOINTS=false`
- `API_KEY=`（可选）
- `ADMIN_KEY=`（可选）

## 部署方式

## 方式一：本机或测试环境部署

适用于开发机、测试机或内网演示。

```bash
docker-compose up -d --build
```

这种方式直接使用默认配置即可，上传文件会进入 MinIO，对象元数据和分享码会保存在 PostgreSQL。

## 方式二：生产环境部署

适用于公网服务器、正式广域网文件传输场景。

### 1. 准备服务器

建议准备：

- 一台 Linux 服务器
- 已安装 Docker 与 Docker Compose
- 一个可解析到服务器公网 IP 的域名
- HTTPS 证书

### 2. 修改 Nginx 域名与证书配置

当前 [nginx/nginx.conf](/d:/Code/filecodebox-tus/nginx/nginx.conf:1) 中写死了示例域名和证书文件名，部署前必须改成你自己的。

需要重点检查：

- `server_name`
- `ssl_certificate`
- `ssl_certificate_key`

例如：

```nginx
server_name your-domain.com;
ssl_certificate /etc/nginx/certs/your-domain.com.pem;
ssl_certificate_key /etc/nginx/certs/your-domain.com.key;
```

然后把对应证书文件放到 `nginx/` 目录中。

注意：证书私钥不要提交到 GitHub。

### 3. 修改对象存储配置

如果你继续使用 MinIO，可以保留默认配置。

如果你接入阿里云 OSS、腾讯云 COS、AWS S3 或其他 S3 兼容服务，需要修改 `backend` 服务环境变量：

- `S3_ENDPOINT`
- `S3_PUBLIC_ENDPOINT`
- `S3_BUCKET`
- `S3_ACCESS_KEY`
- `S3_SECRET_KEY`
- `S3_REGION`

说明：

- `S3_ENDPOINT`：后端服务访问对象存储的内网或服务端地址
- `S3_PUBLIC_ENDPOINT`：下载预签名地址对外使用的访问地址

如果 `S3_PUBLIC_ENDPOINT` 配置不正确，下载链接可能在公网不可访问。

### 4. 修改数据库密码和鉴权配置

建议至少调整以下内容：

- `POSTGRES_PASSWORD`
- `API_KEY`
- `ADMIN_KEY`

其中：

- `API_KEY`：普通上传/下载接口认证用
- `ADMIN_KEY`：管理 API Key、后台管理功能用

如果不设置 `ADMIN_KEY`，后端会仅允许本地网络访问部分管理能力。

### 5. 启动服务

```bash
docker-compose up -d --build
```

### 6. 验证服务状态

可访问健康检查接口：

```bash
curl http://localhost/api/health
```

如果通过 Nginx 对外发布，也可以使用：

```bash
curl https://your-domain.com/api/health
```

## 前端与后端开发

## 前端开发

```bash
cd frontend
npm install
npm run dev
```

前端使用：

- Vue 3
- Vite
- Element Plus
- `tus-js-client`

前端支持通过环境变量注入：

- `VITE_API_KEY`
- `VITE_API_BASE_URL`

默认情况下，项目通过同源路径访问 `/api` 和 `/files`，因此走 Nginx 反向代理时一般不需要额外配置前端 API 地址。

## 后端开发

```bash
cd backend
go run main.go
```

后端启动后默认监听：

- `PORT`，默认 `8080`

## 接口说明

### 上传接口

- `POST /files/`
- `PATCH /files/:id`
- `HEAD /files/:id`

说明：

- 这组接口由 `tus` 协议使用，通常由前端自动调用
- 需要在请求头中携带 `X-API-Key` 或 `X-Admin-Key`，除非服务端未启用鉴权

### 获取分享码

- `POST /api/get-code`

请求体：

```json
{
  "upload_id": "上传完成后的文件ID"
}
```

返回示例：

```json
{
  "code": "Ab3K9xQe",
  "filename": "demo.zip"
}
```

### 根据分享码查询文件

- `GET /api/retrieve/:code`

返回示例：

```json
{
  "upload_id": "xxxx",
  "filename": "demo.zip",
  "url": "/files/Ab3K9xQe"
}
```

### 健康检查

- `GET /api/health`

### 鉴权校验

- `POST /api/verify-key`

用于验证当前输入的 `API Key` 或 `Admin Key` 是否有效。

## API Key 管理

后端支持 API Key 管理能力，管理员可通过前端界面或接口管理。

常见管理接口包括：

- `GET /api/admin/keys`
- `POST /api/admin/keys`
- `PATCH /api/admin/keys/:id`
- `DELETE /api/admin/keys/:id`

这些接口需要提供有效的 `Admin Key`。

## 配置建议

生产环境建议至少完成以下调整：

- 修改 PostgreSQL 默认密码
- 修改 MinIO 默认账号密码
- 配置自己的 `API_KEY` 和 `ADMIN_KEY`
- 配置自己的域名和 HTTPS 证书
- 将 `CORS_ORIGINS` 从 `*` 改为明确的域名列表
- 将 `S3_PUBLIC_ENDPOINT` 改成真实可访问的公网地址

## 注意事项

- 项目中 `nginx/*.key`、`nginx/*.pem` 已被 `.gitignore` 忽略，不建议上传私钥和证书
- 使用 S3 对象存储时，下载功能依赖预签名 URL，请确保对象存储外网地址可达
- Nginx 中已设置 `client_max_body_size 0` 与长超时配置，适合大文件上传
- `proxy_request_buffering off` 对 `tus` 上传非常关键，不建议去掉
- 如果分享码过期或达到下载次数上限，后端会拒绝继续下载

## 后续可优化方向

- 增加完整的初始化脚本与环境变量模板
- 区分开发、测试、生产三套 Compose 配置
- 增加 CI/CD 自动部署流程
- 增加对象存储生命周期管理与清理策略
- 增加上传任务列表、下载统计和审计日志

## 许可说明

如需开源发布，建议后续补充正式 License 文件。
