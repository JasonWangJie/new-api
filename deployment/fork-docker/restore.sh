#!/usr/bin/env bash
set -Eeuo pipefail
umask 077
package_dir=$(cd -- "${1:-$(dirname -- "${BASH_SOURCE[0]}")}" && pwd)
export NEWAPI_DEPLOY_DIR=${NEWAPI_DEPLOY_DIR:-/opt/new-api}
test -f "$package_dir/COMPLETE"
(
  cd "$package_dir"
  sha256sum -c PORTABLE_SHA256SUMS
)
case $(uname -m) in
  x86_64) architecture=amd64 ;;
  aarch64) architecture=arm64 ;;
  *) echo '不支持当前服务器架构。'; exit 1 ;;
esac
[[ $architecture == $(cat "$package_dir/architecture.txt") ]] || { echo '新旧服务器 CPU 架构不同，请使用与旧服务器相同的架构。'; exit 1; }
docker compose version >/dev/null
mkdir -p "$NEWAPI_DEPLOY_DIR"
cd "$NEWAPI_DEPLOY_DIR"
exec 9>.manage.lock
flock -n 9 || { echo '另一个部署操作正在执行。'; exit 1; }
if [[ -n $(find . -mindepth 1 -maxdepth 1 ! -name .manage.lock -print -quit) ]]; then
  echo "$NEWAPI_DEPLOY_DIR 不是空目录，停止恢复，不会覆盖已有部署。"
  exit 1
fi
project=${NEWAPI_PROJECT_NAME:-new-api-fork}
if [[ -n $(docker ps -aq --filter "label=com.docker.compose.project=$project") ]]; then
  echo '这个 Compose 项目已有容器，请先按照指南保留并停止旧部署。'
  exit 1
fi
docker image load < "$package_dir/images.tar.gz"
expected_image=$(cat "$package_dir/old-image.txt")
fingerprint=$(docker image inspect --format '{{json .Config}} {{json .RootFS}} {{.Created}} {{.Architecture}} {{.Os}}' "$expected_image" | sha256sum | awk '{print $1}')
[[ $fingerprint == $(cat "$package_dir/app-image-fingerprint.txt") ]] || { echo '应用镜像配置或文件系统校验失败。'; exit 1; }
tar -xzf "$package_dir/runtime-files.tar.gz" -C "$NEWAPI_DEPLOY_DIR"
chmod 600 .env image.env compose.fork.yml compose-env.sh
chmod 700 manage.sh
git clone --branch main "$package_dir/source.bundle" src
git -C src remote set-url origin https://github.com/JasonWangJie/new-api.git
revision=$(cat "$package_dir/source.commit")
[[ $revision =~ ^[a-f0-9]{40}$ ]]
git -C src switch -C main "$revision"
git -C src branch --set-upstream-to=origin/main main
. ./compose-env.sh
unset APP_IMAGE_TAG
dc config --quiet
configured_image=$(dc config --format json | python3 -c 'import json,sys; print(json.load(sys.stdin)["services"]["new-api"]["image"])')
[[ $configured_image == "$expected_image" ]] || { echo '备份配置与运行镜像不同，停止恢复。'; exit 1; }
tools_dir=${NEWAPI_TOOLS_DIR:-/root/new-api-tools}
mkdir -p "$tools_dir"
for item in install.sh manage.sh export.sh restore.sh compose-env.sh compose.fork.yml; do
  cp "$package_dir/tools/$item" "$tools_dir/$item"
done
chmod 700 "$tools_dir"/*.sh
dc up -d --no-build --pull never --wait --wait-timeout 180 postgres redis
table_count=$(dc exec -T postgres psql -U newapi -d newapi -Atc "SELECT count(*) FROM information_schema.tables WHERE table_schema='public';")
[[ $table_count == 0 ]] || { echo '目标数据库不为空，停止恢复。'; exit 1; }
dc exec -T postgres pg_restore --single-transaction --no-owner -U newapi -d newapi < "$package_dir/main-db.dump"
dc exec -T postgres psql -U newapi -d newapi -Atc 'SELECT COUNT(*) FROM users;'
echo '数据恢复成功，数据库和 Redis 已运行；网站尚未启动。'
echo "确认旧服务器应用已停止后执行：bash $NEWAPI_DEPLOY_DIR/manage.sh start"
echo '随后配置宝塔反向代理和域名解析，并使用原网站账号登录。'
