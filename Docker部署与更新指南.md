# new-api Docker 部署、更新、迁移与恢复指南

适用于自己的 [JasonWangJie/new-api](https://github.com/JasonWangJie/new-api) Fork，保留 new-api / QuantumNous 原有项目身份和版权信息。更新日期：2026-09-17。

**需要配合本次提供的 `new-api-tools.zip` 使用。** 工具包不含任何密码。下载指南时，也请下载这个工具包；里面已经备齐部署配置和操作脚本，不需要自己编写文件，也不要求工具包已推送到 GitHub。

## 一、先选择你要做的事情

| 你的情况 | 看哪里 |
| --- | --- |
| 一台空服务器，第一次安装 | 第二章 |
| 已经安装好，想更新自己的代码 | 第四章 |
| 想完整备份，以后能恢复 | 第五章 |
| 想换一台服务器，保留原来的账号、余额和图片 | 第六章 |
| 服务器坏了，用完整备份重建 | 第七章 |
| 想在原服务器恢复旧备份 | 第八章 |
| 手里只有之前的数据库、文件备份，没有完整迁移包 | 第九章 |

**你的 40.160.139.185 已经安装好了，不需要再执行首次安装。** 域名是 `https://api.aiimg.lol`，宝塔代理目标是 `http://127.0.0.1:3000`。

所有命令都在 **宝塔左侧“终端”中，以 root 登录服务器后执行**。不是在自己电脑的 PowerShell 中执行。看到报错就先停下，不要跳过错误继续下一步。

后文说的“暂停新提交”，是指先通知使用者暂停使用，并暂停自己调用这个接口的软件或自动任务；让已经开始的任务继续完成，再进行备份或迁移。不要直接关闭异步开关来排空队列。

## 二、首次部署：从空服务器开始

### 第 1 步：准备服务器和域名

下面的环境安装命令适用于 **Ubuntu 24.04 / 26.04**，建议使用 x86_64 / amd64 服务器。源码编译建议准备 8GB 内存及足够磁盘空间。

先安装宝塔，把域名 `api.aiimg.lol` 解析到这台服务器 IP。使用 Cloudflare 时，DNS 记录指向服务器 IP，SSL 模式使用 [Full (strict)](https://developers.cloudflare.com/ssl/origin-configuration/ssl-modes/full-strict/)，宝塔网站也要有有效证书。

换成自己的其他域名时，后面所有 `api.aiimg.lol` 都相应替换。

### 第 2 步：安装 Docker 和基础工具

先执行：

```bash
docker version
docker compose version
docker buildx version
```

三条都有版本信息，说明 Docker 已就绪，**跳过下面的 Docker 安装段**。你的当前服务器可以跳过。

仅在没有安装 Docker 的空 Ubuntu 服务器上，整段复制执行：

```bash
(
  set -e
  apt-get update
  apt-get install -y ca-certificates curl git openssl python3 unzip util-linux
  mkdir -p /etc/apt/keyrings
  curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/newapi-docker.asc
  chmod 644 /etc/apt/keyrings/newapi-docker.asc
  . /etc/os-release
  printf 'deb [arch=%s signed-by=/etc/apt/keyrings/newapi-docker.asc] https://download.docker.com/linux/ubuntu %s stable\n' "$(dpkg --print-architecture)" "${UBUNTU_CODENAME:-$VERSION_CODENAME}" > /etc/apt/sources.list.d/newapi-docker.list
  apt-get update
  apt-get install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
  systemctl enable --now docker
  docker compose version
)
```

已有 Docker 时，基础工具还没装齐可以单独执行：

```bash
apt-get update
apt-get install -y git openssl python3 curl unzip util-linux
```

不要为了安装 Docker 卸载正在运行的其他服务。其他 Linux 系统请使用对应的 [Docker 官方安装步骤](https://docs.docker.com/engine/install/)。

### 第 3 步：上传并解压工具包

在宝塔 **文件** 中打开 `/root`，上传本次下载的 **`new-api-tools.zip`**。

终端执行：

```bash
mkdir -p /root/new-api-tools
unzip -o /root/new-api-tools.zip -d /root/new-api-tools
```

解压后应当有 `install.sh`、`manage.sh`、`export.sh`、`restore.sh`、`compose-env.sh`、`compose.fork.yml`。

### 第 4 步：执行首次安装

```bash
bash /root/new-api-tools/install.sh https://api.aiimg.lol
```

这一步自动下载自己的 Fork、生成随机密码和稳定密钥、保存图片目录、构建镜像并启动 PostgreSQL、Redis 和应用。首次构建需要时间，请等终端输出 **“首次部署成功”**。

脚本使用 `/opt/new-api/src` 保存源码。目录中已有干净的 Fork `src` 时可以使用它；已有 `.env`、数据库或运行配置时会停止，避免覆盖。不要在已经部署的服务器上重复首次安装。

### 第 5 步：确认三个服务正常

```bash
bash /opt/new-api/manage.sh status
```

三个服务应显示 `healthy`，并输出 **“应用健康：True”**。

### 第 6 步：在宝塔设置网站和反向代理

1. 宝塔 **网站 → 添加站点**，域名填 `api.aiimg.lol`。
2. 为网站配置 SSL 证书，使用 HTTPS。
3. 网站设置 **反向代理**：代理目录 `/`，目标地址 **`http://127.0.0.1:3000`**，发送域名 **`$host`**。
4. 关闭反向代理缓存。
5. 打开 **`https://api.aiimg.lol`**，按页面提示设置网站管理员账号和密码。

网站管理员与服务器 root 是两套账号。应用只监听本机，直接访问 `http://服务器IP:3000` 不会打开网站。

### 第 7 步：填写后台配置

在后台系统设置中，把 **服务器地址 / ServerAddress** 填成 `https://api.aiimg.lol`，不带末尾 `/`。这会影响图片签名链接和公共任务查询地址。

再填写渠道密钥、模型、价格和 Token。异步生图还需要在 `/system-settings/operations/images` 配置平台分组、模型和渠道池，并启用全局及分组的异步开关。

Cookie 的 HTTPS 和可信域名已由安装脚本配置。服务器地址仍需在后台填写，不能只改 Cookie 地址。

## 三、日常操作速查

| 想做什么 | 宝塔终端命令 |
| --- | --- |
| 查看服务是否正常 | `bash /opt/new-api/manage.sh status` |
| 查看运行版本 | `bash /opt/new-api/manage.sh version` |
| 查看最近日志 | `bash /opt/new-api/manage.sh logs` |
| 重启应用 | `bash /opt/new-api/manage.sh restart` |
| 启动已有部署 | `bash /opt/new-api/manage.sh start` |
| 快速备份数据库和文件 | `bash /opt/new-api/manage.sh backup` |
| 完整备份，包含镜像和源码 | `bash /root/new-api-tools/export.sh backup` |
| 生成迁移包，并保持旧应用停止 | `bash /root/new-api-tools/export.sh migration` |

目录固定为：源码 `/opt/new-api/src`，配置 `/opt/new-api/.env`，图片 `/opt/new-api/images`，备份 `/opt/new-api/backups`。PostgreSQL、Redis 各有独立持久化目录。

原仓库默认 Compose 使用 `calciumion/new-api:latest`。本方案使用 `compose.fork.yml` 和自己从源码构建的 `new-api-fork:<完整提交号>`，请始终使用本指南的操作入口。

## 四、更新：按这三步操作

### 第 1 步：把代码推送到自己的仓库

在开发电脑提交新代码，并推送到自己的 `origin/main`。本机没提交、没推送的修改，服务器不会收到。

需要同步原项目时，先在开发电脑合并、验证并保留自己的业务逻辑，再推送自己的仓库；服务器只从自己的 Fork 更新。

### 第 2 步：暂停新任务，等待正在执行的任务完成

查看任务中心，等图片及其他任务完成。不要直接关闭全局异步开关来排空任务，可能影响已经排队的任务。

### 第 3 步：执行更新

```bash
bash /opt/new-api/manage.sh update --ready
```

脚本获取自己的新代码，先构建，再停止应用、备份、切换镜像，最后等待健康检查。构建时网站继续运行，备份和切换时短暂中断。`--ready` 表示你已经完成第 2 步。

看到 **“更新成功”** 后，打开网站检查登录、余额和任务中心，再恢复新提交。看到 **“无需更新”** 表示运行版本已经是自己的仓库最新提交。

构建失败时原应用继续运行。切换后启动失败时，先看 `status` 和 `logs`；数据库可能已迁移，不能直接把旧程序连上去。`manage.sh resume` 只尝试启动当前选定镜像，不回退数据。

## 五、完整备份：以后恢复优先使用这个

### 第 1 步：先准备工具包

已做过第二章第 3 步就跳过。当前这台已部署服务器，也可以只上传、解压工具包，**不运行 install.sh**。

### 第 2 步：暂停新提交，等任务完成，然后备份

```bash
bash /root/new-api-tools/export.sh backup
```

看到 **“完整备份成功，网站已恢复运行”**，说明成功。备份过程中网站短暂停止，结束后自动恢复。

### 第 3 步：把终端提示的两个文件下载到自己电脑

在宝塔文件管理器打开 `/opt/new-api/backups`，下载本次输出的：

- `new-api-backup-日期时间.tar.gz`：完整恢复包。
- 同名 `.tar.gz.sha256`：校验文件。

完整包包含数据库、图片、Redis 队列、原密码和密钥、运行镜像，以及 Git 源码。因此恢复时不需要先升级、重新编译或去 GitHub 找旧版本。

**不要把这两个私密备份文件放在网站公开目录。** 数据库账号、管理员、余额、Token、渠道设置都在数据库中；密钥和图片必须一起恢复。

`manage.sh backup` 生成的目录备份不包含运行镜像和源码，它是旧格式，恢复方法见第九章。完整包适用于本指南的单应用、一个 PostgreSQL 数据库、本地图片存储；以后加入 S3、独立日志库或多实例，需要扩展备份范围。

## 六、迁移到另一台服务器：保留原网站全部数据

以下按 **域名不变，CPU 架构不变** 操作。新旧服务器都使用 x86_64 最方便。

### 第 1 步：先准备新服务器

在新服务器安装宝塔，按第二章第 2 步准备 Docker 和基础工具。先不要修改域名解析，也 **不要执行首次安装脚本**，避免创建空网站。

### 第 2 步：在旧服务器生成迁移包

暂停新提交，等任务中心正在执行的任务完成，在 **旧服务器终端** 执行：

```bash
bash /root/new-api-tools/export.sh migration
```

成功后输出 **“旧服务器应用保持停止”**。旧服务器数据仍保留，应用不会自动恢复运行，避免两台同时接收请求和处理任务。

在旧服务器宝塔文件管理器下载终端提示的 `.tar.gz` 和 `.tar.gz.sha256` 两个文件到自己电脑。

### 第 3 步：上传到新服务器

新服务器终端创建上传目录：

```bash
mkdir -p /root/new-api-transfer
chmod 700 /root/new-api-transfer
```

用新服务器宝塔文件管理器，把两个文件上传到 `/root/new-api-transfer`。

**为了下面命令不用改名字，在宝塔分别重命名：**

- `.tar.gz` 改为 `new-api-transfer.tar.gz`。
- `.tar.gz.sha256` 改为 `new-api-transfer.original.sha256`。

### 第 4 步：检查上传文件没有损坏，并解压

在 **新服务器终端** 整段执行：

```bash
(
  set -e
  cd /root/new-api-transfer
  expected=$(awk 'NR==1 {print $1}' new-api-transfer.original.sha256)
  actual=$(sha256sum new-api-transfer.tar.gz | awk '{print $1}')
  test "$expected" = "$actual"
  mkdir -p package
  tar -xzf new-api-transfer.tar.gz -C package
  echo '校验及解压成功'
)
```

只有看到 **“校验及解压成功”** 才继续。如果校验失败，重新上传，不要忽略。

### 第 5 步：恢复数据

```bash
bash /root/new-api-transfer/package/restore.sh /root/new-api-transfer/package
```

脚本载入原来的镜像、源码、配置、图片和队列，建立空 PostgreSQL 数据库并恢复数据，也会把操作工具还原到 `/root/new-api-tools`。**不会覆盖已有 `/opt/new-api`，不会自动启动网站。**

看到 **“数据恢复成功……网站尚未启动”** 才继续。恢复时保持原密码和密钥，不要重新生成 `.env`。出错时保留上传包和报错；不要先启动空网站。

### 第 6 步：确认旧服务器应用确实已经停止

在 **旧服务器终端** 执行：

```bash
bash /opt/new-api/manage.sh status
```

旧应用此时应没有运行，健康请求失败是预期结果，PostgreSQL 和 Redis 可以继续运行。**不要在旧服务器执行 start。**

确认后，在 **新服务器终端** 执行：

```bash
bash /opt/new-api/manage.sh start
bash /opt/new-api/manage.sh status
```

### 第 7 步：配置新宝塔网站，再切换域名解析

在新宝塔按第二章第 6 步设置同域名、SSL、反向代理 `http://127.0.0.1:3000`。证书可以重新申请，或把旧宝塔的证书和私钥安全导入新宝塔；它们不在 Docker 备份包内。

然后在域名 DNS / Cloudflare 中，把 `api.aiimg.lol` 的服务器 IP 改成 **新服务器 IP**，同时核对 IPv6 的 AAAA 记录。域名不变时，应用里的服务器地址和 Cookie 地址不用改。

### 第 8 步：用原网站账号登录，检查数据

确认管理员、用户、余额、渠道、Token、图库和任务记录都还在，再恢复新提交。

**迁移后应使用原账号登录，不应让你重新创建管理员。** 如果出现初始化页面，先停新应用并查看恢复报错，不要初始化一个空网站。

旧服务器先保留一段时间。新站接收充值、消费或生成新任务后，旧服务器数据已经过时；不能直接改回旧 IP，否则这些新数据会丢失。

## 七、服务器坏了：用完整备份重建

1. 找到之前下载到自己电脑的完整备份 `.tar.gz` 和校验文件。
2. 准备与原服务器 CPU 架构相同的新服务器，按第二章第 2 步安装环境，不运行首次安装脚本。
3. 按第六章 **第 3～5 步** 上传、校验、解压并恢复。
4. 确认旧服务器已关闭或旧应用不再运行，再执行第六章 **第 6～8 步** 启动新站、配置宝塔、切换 DNS 并检查数据。

恢复出来的是 **备份那一刻** 的数据，之后的新用户、充值、消费和任务不会凭空回来。恢复后先核对账务，再开放业务。

## 八、原服务器恢复旧备份：先留住当前数据

这会把网站退回旧备份的时间点，只在确实需要恢复时操作。先下载当前能保存的数据和旧备份，暂停新提交，等待执行中的任务完成。

### 第 1 步：上传并解压要恢复的完整包

按第六章第 3～4 步操作，包放在 `/root/new-api-transfer`，**不要放在 `/opt/new-api` 内**，因为下一步要保留并改名这个目录。

### 第 2 步：停止旧部署，保留整个旧目录

在原服务器整段执行：

```bash
(
  set -e
  cd /opt/new-api
  . ./compose-env.sh
  unset APP_IMAGE_TAG
  dc down
  cd /opt
  saved="/opt/new-api-before-recovery-$(date +%Y%m%d-%H%M%S)"
  mv /opt/new-api "$saved"
  echo "当前数据已保留在：$saved"
)
```

这里的 `down` **没有 `-v`**，只移除本部署容器和网络；数据库、图片、配置保留在改名后的旧目录。

### 第 3 步：恢复并启动

```bash
bash /root/new-api-transfer/package/restore.sh /root/new-api-transfer/package
```

只有看到 **“数据恢复成功”** 后，才执行：

```bash
bash /opt/new-api/manage.sh start
bash /opt/new-api/manage.sh status
```

同一台服务器、同一域名，宝塔代理和 DNS 通常不用改。用原账号检查恢复后的余额、渠道和图片。

恢复失败时不要删除改名后的旧目录。先保留失败现场和错误，再决定是否切回保留的数据；已经发生的付款和上游消费需人工核对。

## 九、之前的旧格式备份如何恢复

适用于之前 `manage.sh backup` 产生的目录：里面至少有 `main-db.dump`、`runtime-files.tar.gz`、`image.env`、`SHA256SUMS` 和 `COMPLETE`。

本次最初生成过这样的备份：`/opt/new-api/backups/20260917T015643Z`。这是当时的记录，不代表最新业务数据。

### 第 1 步：上传完整备份目录

准备空服务器并安装环境，把备份目录中的所有文件上传到 **`/root/new-api-legacy-backup`**。同机恢复时，先按第八章第 2 步保留旧部署。

### 第 2 步：校验旧备份

旧校验文件保存的是旧服务器绝对路径，因此不能直接照搬 `sha256sum -c`。在目标服务器整段执行：

```bash
(
  set -e
  cd /root/new-api-legacy-backup
  test -f COMPLETE
  awk '{n=$2; sub(/^.*\//,"",n); print $1 "  " n}' SHA256SUMS > SHA256SUMS.local
  sha256sum -c SHA256SUMS.local
  echo '旧备份校验成功'
)
```

### 第 3 步：还原文件、找回原版本源码并构建镜像

这段会拒绝覆盖现有部署。只有目标 `/opt/new-api` 不存在或是空目录时执行：

```bash
(
  set -e
  mkdir -p /opt/new-api
  test -z "$(find /opt/new-api -mindepth 1 -maxdepth 1 -print -quit)"
  cd /opt/new-api
  tar -xzf /root/new-api-legacy-backup/runtime-files.tar.gz
  chmod 600 .env image.env
  sha=$(sed -n 's/^APP_IMAGE_TAG=//p' image.env)
  [[ $sha =~ ^[a-f0-9]{40}$ ]]
  git clone --branch main https://github.com/JasonWangJie/new-api.git src
  git -C src switch -C main "$sha"
  . ./compose-env.sh
  unset APP_IMAGE_TAG
  dc pull postgres redis
  dc build new-api
  dc up -d --no-build --pull never --wait --wait-timeout 180 postgres redis
  test "$(dc exec -T postgres psql -U newapi -d newapi -Atc "SELECT count(*) FROM information_schema.tables WHERE table_schema='public';")" = 0
  dc exec -T postgres pg_restore --single-transaction --no-owner -U newapi -d newapi < /root/new-api-legacy-backup/main-db.dump
  echo '旧备份恢复成功，网站尚未启动'
)
```

只有成功后，才按第六章第 6～8 步启动、设置宝塔、切换 DNS 并登录检查。

旧格式没有镜像和源码归档，依赖 GitHub 仍保留原提交，以及构建依赖仍可下载。如果原提交已找不到或构建失败，不要换最新版硬接旧数据库；先保留备份查原因。带 `bootstrap-` 标签的测试备份不适用这段命令，优先选择正常完整提交号的备份。从现在开始建议使用第五章的完整恢复包。

## 十、常见问题

| 问题 | 处理方法 |
| --- | --- |
| 网站 502 | 先执行 `manage.sh status`；服务健康时检查宝塔代理地址 |
| 首次构建失败 | 保留已经生成的 `.env`，不要重新生成密钥；按下面“重试构建”操作 |
| 恢复提示目录不为空 | 不要清空；新服务器选空目录，同机按第八章保留旧部署 |
| 恢复提示项目已有容器 | 先保留旧数据，再按第八章执行 `dc down`，不能只停止应用 |
| 迁移后出现初始化页面 | 不要初始化，检查数据库恢复是否成功 |
| 新功能没有出现 | 执行 `manage.sh version`，核对代码是否已推送自己的 main |
| 图片丢失或打不开 | 核对 `images` 是否随备份恢复，以及后台服务器地址是否正确 |

首次安装在生成配置后构建失败，可以整段重试已有配置，不重新安装：

```bash
(
  set -e
  cd /opt/new-api
  . ./compose-env.sh
  unset APP_IMAGE_TAG
  dc pull postgres redis
  dc build new-api
  bash ./manage.sh start
)
```

长时间请求或流式聊天，可以在宝塔反向代理中设置 `proxy_buffering off;`、`proxy_cache off;`、`proxy_read_timeout 1200s;`、`proxy_send_timeout 1200s;`，按需要设置上传大小。已有同名项直接修改，保存后检查并重载 Nginx。

如果 API 客户端返回 Cloudflare `403 / 1010`，在 Cloudflare 对 **`api.aiimg.lol`** 单独关闭或跳过 Browser Integrity Check / 浏览器完整性检查，再使用原客户端重试。本次曾实测 Python 默认请求被拦截。操作参考 [Cloudflare 官方说明](https://developers.cloudflare.com/waf/tools/browser-integrity-check/)。

不要执行 `docker compose down -v`，不要清空图片、数据库或 Redis 目录，不要重生成已有密钥，不要在 `src` 里启动原仓库默认 Compose。

## 十一、验证记录与参考

原部署使用 Ubuntu 26.04、Docker 29.8.0、Compose 5.5.1、PostgreSQL 15.19、Redis 7.4.11。之后运行版本请以 `manage.sh version` 和容器实际信息为准。

工具包已在独立目录和 Compose 项目中完成演练：空目录首次安装成功，生成完整包并校验传输文件，恢复到空 PostgreSQL 数据库，核对测试用户和额度、temporary / durable 两类图片、应用数据、Redis 测试键及原密钥，再单独启动恢复后的应用并通过健康检查。迁移模式保持原测试应用停止，备份模式完成后恢复原测试应用运行。

本次演练使用端口 13001 / 13002，生产站点未参与恢复覆盖。工具包的恢复流程已实际验证；旧格式备份步骤另做命令语法核对，之前已验证其数据库和 Redis 备份的隔离恢复。本次没有进行付费上游调用，也不能保证未来任意版本的数据库迁移兼容。

参考：[Ubuntu 安装 Docker](https://docs.docker.com/engine/install/ubuntu/)、[PostgreSQL pg_restore](https://www.postgresql.org/docs/15/app-pgrestore.html)、[Redis 持久化](https://redis.io/docs/latest/operate/oss_and_stack/management/persistence/)。
