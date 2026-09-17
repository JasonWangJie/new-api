# new-api Docker 部署与更新指南（这台服务器专用）

更新日期：2026-09-17。适用于已经部署好的 **40.160.139.185**。

源码来自自己的 [JasonWangJie/new-api](https://github.com/JasonWangJie/new-api)，保留 new-api / QuantumNous 的项目身份和版权信息。

## 1. 以后主要用这几条命令

打开宝塔左侧 **终端**，使用 root 登录。下面的命令都在服务器终端执行，一行一行复制即可，**不是在自己电脑的 PowerShell 执行**。

| 想做什么 | 复制这条命令 |
| --- | --- |
| 检查服务是否正常 | `bash /opt/new-api/manage.sh status` |
| 查看正在运行的代码版本 | `bash /opt/new-api/manage.sh version` |
| 查看最近的应用日志 | `bash /opt/new-api/manage.sh logs` |
| 重启应用 | `bash /opt/new-api/manage.sh restart` |
| 备份数据库、配置和图片 | `bash /opt/new-api/manage.sh backup` |
| 更新自己的仓库最新代码 | `bash /opt/new-api/manage.sh update --ready` |
| 启动已有部署 | `bash /opt/new-api/manage.sh start` |

脚本已固定部署目录和配置文件，不需要每次手动输入一大串 Docker 参数。

**更新、备份、重启前，暂停新提交，等任务中心正在执行的任务完成。** `--ready` 表示你已做好这一步，脚本不会替你暂停业务。备份和切换版本时网站会短暂停止响应。

## 2. 这台服务器的实际配置

| 项目 | 配置 |
| --- | --- |
| 网站域名 | `https://api.aiimg.lol` |
| 宝塔反向代理目标 | `http://127.0.0.1:3000` |
| 部署目录 | `/opt/new-api` |
| Git 源码目录 | `/opt/new-api/src` |
| 更新来源 | 自己的 `origin/main` |
| 应用镜像 | `new-api-fork:<完整提交号>`，在服务器从源码构建 |
| 数据库 | PostgreSQL 15，本次实际版本 15.19 |
| 缓存与异步队列 | Redis 7，本次实际版本 7.4.11，开启持久化 |
| 当前部署提交 | `c211e365027e79e0a195125227af87d7e9015642` |
| Docker / Compose | 本次实际版本 29.8.0 / 5.5.1 |

原仓库默认 Compose 使用 `calciumion/new-api:latest`。**本服务器使用独立的 `compose.fork.yml`，构建并运行自己的 Fork。** 只拉 Git 代码，不会自动更新正在运行的程序。

应用只绑定本机端口，直接访问 `http://40.160.139.185:3000` 不会打开网站，正常入口是 HTTPS 域名。数据库和 Redis 也没有公开端口。

```text
/opt/new-api/
  src/                  Git 源码，日常更新只更新这里
  manage.sh             启动、查看、备份、更新入口
  compose-env.sh        固定的 Docker Compose 入口
  compose.fork.yml      实际部署配置
  .env                  密码、稳定密钥、域名等
  image.env             当前运行镜像标签
  data/                 应用数据
  logs/                 应用日志
  images/temporary/     临时图片
  images/durable/       长期图片
  postgres-data/        数据库文件
  redis-data/           Redis 持久化文件
  backups/              更新前及手动备份
```

源码与生产数据分开保存。图片目录也已挂载到容器的 `/images`，重建应用容器后保留。

## 3. 宝塔代理和首次打开网站

宝塔 **网站 → api.aiimg.lol → 反向代理**：

1. 代理目录填 `/`。
2. 目标地址填 **`http://127.0.0.1:3000`**，这里使用 HTTP。
3. 发送域名填 **`$host`**。
4. 启用网站 SSL，通过 **`https://api.aiimg.lol`** 访问。
5. 关闭反向代理缓存。

本次发现宝塔已经有指向 `http://localhost:3000` 的代理，应用启动后域名已能访问。现有配置可用时，无需再创建重复代理。

长时间请求或流式聊天可检查以下设置，已有同名项直接修改，不要重复粘贴；保存后在宝塔检查配置并重载 Nginx：

```nginx
proxy_set_header Host $host;
proxy_set_header X-Forwarded-Proto $scheme;
proxy_buffering off;
proxy_cache off;
proxy_read_timeout 1200s;
proxy_send_timeout 1200s;
client_max_body_size 100m;
```

首次打开网站，按初始化页面设置 **new-api 管理员账号和密码**。网站账号与服务器 root 账号是两回事，本次没有替你创建网站管理员。

服务器地址及 Cookie 可信地址已配置为 `https://api.aiimg.lol`，请通过该 HTTPS 域名登录。

后台还需自行填写渠道密钥、模型价格和 Token。异步生图在 `/system-settings/operations/images` 配置平台分组、模型、渠道池，启用全局和对应分组的异步开关。服务部署不会自动获得上游账号。

## 4. 日常更新：只做这三步

**第一步：把代码推送到自己的 GitHub。** 在开发电脑提交要发布的修改，并推送到自己的 `origin/main`。本机未提交、未推送的修改，服务器收不到。

**第二步：等待现有任务完成。** 暂停新提交，等任务中心正在执行的任务完成。不要直接关闭全局异步开关来排空任务，可能影响已经排队的任务。中断已发送到上游的请求可能留下 `execution_unknown`，需要人工核对，不能盲目重复生成。

**第三步：宝塔终端执行这一条。**

```bash
bash /opt/new-api/manage.sh update --ready
```

脚本自动检查源码和分支 → 获取自己的 `origin/main` → 构建新镜像 → 停应用并备份 → 切换新镜像 → 等健康检查通过。

构建期间原网站继续运行；备份及切换时短暂中断。正常更新只更新应用，不顺便升级 PostgreSQL、Redis。

看到 **“更新成功”** 后，检查网站、登录、余额和任务中心，再恢复新提交。脚本会显示当前提交号和更新前备份目录。

看到 **“服务器已经运行自己的仓库最新提交，无需更新”**，表示没有新提交，不用重复操作。

看到错误就先停止，执行：

```bash
bash /opt/new-api/manage.sh status
bash /opt/new-api/manage.sh logs
```

构建失败时旧应用继续运行。备份失败且尚未切换镜像时，脚本尝试恢复旧应用；切换后启动失败时，不会自动回退可能已经迁移的数据库。

需要同步原项目更新时，先在开发电脑合并、验证并保留自己的业务逻辑，再推送自己的仓库。生产服务器只更新自己的 Fork。

## 5. 手动备份与恢复

没有正在执行的任务时执行：

```bash
bash /opt/new-api/manage.sh backup
```

脚本短暂停止应用和本部署的 Redis，完成后恢复运行。备份在 `/opt/new-api/backups/日期时间/`，按 UTC 时间命名：

| 文件 | 用途 |
| --- | --- |
| `main-db.dump` | PostgreSQL 数据库备份 |
| `runtime-files.tar.gz` | 配置、密钥、脚本、数据、日志、图片及 Redis 文件 |
| `image.env` / `old-image.txt` | 备份时运行的镜像记录 |
| `SHA256SUMS` | 备份校验记录 |
| `COMPLETE` | 存在此文件，表示备份步骤全部结束 |

可以在宝塔文件管理器中下载完整备份到自己电脑保存。备份包含密码和密钥，不能放在网站公开目录；只有同服务器备份，不能应对整台服务器丢失。

当前脚本适用于单应用、一个 PostgreSQL 数据库、本地图片。以后增加独立日志数据库、S3 或多个实例时，需要相应调整备份。

更新失败后，确认可以继续启动当前配置选定的镜像时执行：

```bash
bash /opt/new-api/manage.sh resume
```

这只启动现有配置，不恢复数据、不回退版本。

回滚前先确认数据库结构能被旧版本读取。恢复数据库会丢失备份后的用户、充值、消费和任务记录；需要回滚时，先保留当前数据和报错，再制定恢复步骤，不要直接覆盖数据库。

## 6. 常见问题

| 现象 | 先做什么 |
| --- | --- |
| 域名出现 502 | 执行 `status`；应用健康时检查宝塔代理地址 |
| 服务没有运行 | 先看 `logs`，再运行 `bash /opt/new-api/manage.sh start` |
| 构建失败 | 保留报错，检查网络和磁盘，不要删数据库重试 |
| 登录提示地址或 Origin 错误 | 使用正确 HTTPS 域名，核对 Cookie 可信地址 |
| 新功能没出现 | 执行 `version`，核对代码是否已推送自己的 `main` |
| 首页仍是旧内容 | 先核对运行提交，再刷新缓存；本机未推送的页面修改不会出现在服务器 |
| 更新提示源码有修改 | 保留 `/opt/new-api/src` 的修改，处理后再更新 |
| 接口客户端返回 Cloudflare `403 / 1010` | 按下面步骤调整这个域名的浏览器完整性检查 |

**本次实际发现的 Cloudflare 设置问题：** 使用浏览器请求头访问网页、前端脚本及状态接口正常；Python 默认请求头被 Cloudflare 返回 `403 / 1010`。使用浏览器请求头访问 `/v1/models` 时，应用正常返回未提供有效 Token 的 `401`。

如果你的 API 客户端也遇到 `403 / 1010`：

1. 登录 Cloudflare，选择 `aiimg.lol`。
2. 在规则中为主机名 **`api.aiimg.lol`** 创建配置规则，关闭 **Browser Integrity Check / 浏览器完整性检查**；或者在自定义规则中对该主机名跳过这一项。
3. 保存后用原来的 API 客户端重试。

这条规则只针对该子域名的这一项检查。配置方式见 [Cloudflare 官方说明](https://developers.cloudflare.com/waf/tools/browser-integrity-check/)。本次没有修改你的 Cloudflare 账号设置。

## 7. 这几件事不要做

- 不要在 `src` 中直接运行默认的 `docker compose up -d`，会使用另一套部署配置。
- 不要把 `docker compose pull` 当成自己的源码更新，本部署需要构建应用。
- 不要执行 `docker compose down -v`，不要清空数据库、图片或 Redis 目录。
- 不要重新生成 `.env` 的数据库密码、`SESSION_SECRET`、`CRYPTO_SECRET` 和图片密钥，已有数据可能依赖它们。
- 不要公开 `.env`，不要清理仍需回滚的旧镜像。

需要手动操作 Compose 时使用：

```bash
cd /opt/new-api
. ./compose-env.sh
unset APP_IMAGE_TAG
dc ps
```

本指南针对已部署的这台服务器。`manage.sh start` 需要已有配置、数据和镜像，不能当作另一台空服务器的一键安装脚本。

## 8. 本次验证范围

本次在服务器实际完成源码镜像构建、三个容器健康检查、HTTPS 域名访问、镜像更新切换、重建后数据和图片挂载保留，以及数据库和 Redis 备份的隔离恢复检查。主要命令为 `manage.sh start`、`status`、`version`、`update --ready`、`backup`，以及 `pg_restore` 恢复至独立测试数据库。本次更新演练使用同一源码版本切换镜像，不能证明未来所有数据库迁移均兼容。

数据库恢复检查实际使用 `createdb -U newapi newapi_restore_check_20260917` 和 `pg_restore --exit-on-error --no-owner -U newapi -d newapi_restore_check_20260917`，核对恢复后 63 张业务表和服务器地址，再删除测试库。Redis 使用无网络的独立测试容器加载备份的 AOF 文件，验证测试键恢复后删除测试容器；生产数据库和队列未被恢复覆盖。

本次最新完整备份：`/opt/new-api/backups/20260917T015643Z`。

管理员初始化由你通过网站完成；未进行付费上游调用，真实聊天、生图与计费需完成后台配置后验证。

命令参考：[Docker 构建](https://docs.docker.com/reference/cli/docker/compose/build/)、[启动及健康等待](https://docs.docker.com/reference/cli/docker/compose/up/)、[环境文件](https://docs.docker.com/compose/how-tos/environment-variables/variable-interpolation/)。HTTPS 与 Cookie 配置参考 [OWASP Authentication](https://cheatsheetseries.owasp.org/cheatsheets/Authentication_Cheat_Sheet.html) 和 [Session Management](https://cheatsheetseries.owasp.org/cheatsheets/Session_Management_Cheat_Sheet.html)；未修改认证代码，也未做全应用认证合规审计。
