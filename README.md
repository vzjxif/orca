# Orca

面向飞牛 NAS 的 Docker Compose 管理台。Go 后端、React + HeroUI 前端，通过单个容器管理现有 Compose 项目。

## 目录约定

```text
/vol1/1000/docker/
  jellyfin/
    compose.yaml
    .env
    config/
    data/
  redis/
    docker-compose.yml
    config/
```

在设置页面添加 `/vol1/1000/docker` 并扫描。只扫描一级子目录，优先识别 compose.yaml、compose.yml、docker-compose.yaml、docker-compose.yml。每个目录一个主文件，第一版不合并 override 文件。遇到配置错误或项目身份不明确时，先修正后再操作。

## 飞牛部署

1. 将本项目放到飞牛独立目录，例如 `/vol1/1000/apps/orca`。
2. 复制 `.env.example` 为 `.env`，填写真实项目根目录、独立备份目录和长随机初始管理员密码。内网直接访问需将 `ORCA_BIND_ADDRESS` 改为飞牛内网 IP。
3. 执行 `docker compose up -d --build`。
4. 浏览器访问 `http://飞牛内网IP:7788`，使用设置的管理员账号密码登录。
5. 设置扫描根目录并执行扫描。
6. 在“账户设置”填写当前密码、新密码与确认密码。修改成功会注销所有会话，请用新密码重新登录。

环境变量 `ORCA_ADMIN_USER` 和 `ORCA_ADMIN_PASSWORD` 仅在数据库没有管理员时初始化账号。首次登录并修改密码后，可从 `.env` 移除 `ORCA_ADMIN_PASSWORD`；密码以 bcrypt 哈希保存于 Orca 数据卷，后续环境变量不会覆盖数据库账号。新密码要求 12–72 个 UTF-8 字节；请保留 Orca 数据卷。

项目目录挂载必须保持宿主机与容器内绝对路径相同，例如 `/vol1/1000/docker:/vol1/1000/docker`。否则 Compose 的相对绑定目录会指向错误位置。多个扫描根目录需要在 Orca 自身 Compose 中增加对应的同路径挂载。

Orca 数据保存在 `orca-data` 卷，备份保存在单独挂载的目录。管理功能需要 Docker socket，因此管理员等同于拥有宿主机 Docker 管理权限。仅向可信内网或受 HTTPS 保护的反向代理开放。

## 日志与管理

项目详情中选择容器查看实时日志，可筛选、暂停自动滚动和调整历史尾部行数。启动、停止、重启和更新均产生后台任务，可查看操作结果。停止不会删除数据卷。

更新流程为 Compose 校验、配置备份、拉取镜像、重建服务、检查容器运行与已有 healthcheck。镜像标签为固定摘要或固定版本时，拉取并不会自动切换版本。失败后查看任务中的失败步骤和日志，再决定如何处理。

## 配置备份与恢复

默认备份只包括所选主 Compose 文件、`.env` 与项目详情中单独指定的配置目录（默认 `config/`）。每个项目还可设置数据目录（默认 `data/`）；数据默认不进入配置备份。手动备份可勾选“同时备份数据”，也可选择“仅备份数据”；更新前是否包含数据同样可选，默认关闭。数据目录必须是项目内存在的相对路径，并与配置目录不重叠；配置目录不能包含 `data` 路径段。目录设置只影响归档范围，不会修改 Compose 挂载。归档不跟随符号链接，也不包括 Docker 命名卷或项目外绑定目录。备份不暂停服务，正在变化的文件可能需要应用自身的一致性机制。归档可能包含密码或业务数据，仅管理员可下载，请保护备份目录。

恢复前先停止对应服务，下载并检查归档清单，将当前配置另行保存，再把归档中的配置解压回原项目位置，检查权限与 Compose 校验结果，最后启动服务。不要直接对不明归档执行覆盖解压。第一版不提供自动恢复。

配置归档不能恢复数据库或镜像版本。业务数据应使用数据库自身备份工具或飞牛快照单独备份。更新失败不会自动回滚镜像或数据。

## 开发

需要 Go 1.25+、Node.js 22+ 与 Docker Compose v2。

```sh
cd frontend
npm ci
npm run build
cd ../backend
# 前端构建后将 frontend/dist 内容复制到 backend/web/dist
# 设置 ORCA_ADMIN_PASSWORD、ORCA_DATA_DIR、ORCA_BACKUP_DIR
 go test ./...
 go run .
```

Vite 开发服务代理 `/api` 到后端。生产构建把前端资源嵌入 Go 二进制。完整打包使用根目录 Dockerfile。

## 第一版边界

管理当前主机 Docker Engine；不包含自动定时更新、多主机、在线编辑 Compose、自动恢复或业务数据备份。容器需使用支持读取日志的 Docker 日志驱动。真实 Docker 生命周期验证需要在具备 Docker Engine 的环境执行。
