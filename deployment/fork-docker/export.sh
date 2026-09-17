#!/usr/bin/env bash
set -Eeuo pipefail
umask 077
tools_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
export NEWAPI_DEPLOY_DIR=${NEWAPI_DEPLOY_DIR:-/opt/new-api}
[[ ${1:-} == backup || ${1:-} == migration ]] || { echo '用法：export.sh backup 或 export.sh migration'; exit 2; }
mode=$1
cd "$NEWAPI_DEPLOY_DIR"
. ./compose-env.sh
unset APP_IMAGE_TAG
exec 9>.manage.lock
flock -n 9 || { echo '另一个操作正在执行。'; exit 1; }
app_container=$(dc ps --status running -q new-api)
test -n "$app_container"
app_image=$(docker inspect --format '{{.Config.Image}}' "$app_container")
revision=$(docker image inspect --format '{{ index .Config.Labels "org.opencontainers.image.revision" }}' "$app_image")
[[ $revision =~ ^[a-f0-9]{40}$ ]] || { echo '运行镜像缺少有效源码提交标签。'; exit 1; }
git -C src cat-file -e "$revision^{commit}"
for service in new-api postgres redis; do
  container=$(dc ps --status running -q "$service")
  test -n "$container"
  image=$(docker inspect --format '{{.Config.Image}}' "$container")
  configured_image=$(dc config --format json | python3 -c 'import json,sys; print(json.load(sys.stdin)["services"][sys.argv[1]]["image"])' "$service")
  [[ $configured_image == "$image" ]] || { echo "$service 的配置与运行镜像不同，请先处理配置。"; exit 1; }
  if [[ $(docker image inspect --format '{{.Id}}' "$image") != $(docker inspect --format '{{.Image}}' "$container") ]]; then
    live_manifest=$(docker inspect --format '{{if .ImageManifestDescriptor}}{{.ImageManifestDescriptor.Digest}}{{end}}' "$container")
    tagged_manifest=$(docker image inspect --format '{{if .Descriptor}}{{.Descriptor.Digest}}{{end}}' "$image")
    [[ -n $live_manifest && $live_manifest == "$tagged_manifest" ]] || { echo "$service 镜像标签与运行镜像不一致，请先检查。"; exit 1; }
  fi
done
postgres_image=$(docker inspect --format '{{.Config.Image}}' "$(dc ps -q postgres)")
redis_image=$(docker inspect --format '{{.Config.Image}}' "$(dc ps -q redis)")
mkdir -p backups
package_dir="$NEWAPI_DEPLOY_DIR/backups/package-$(date -u +%Y%m%dT%H%M%SZ)"
mkdir -m 700 "$package_dir"
echo '保存运行镜像和源码；这一阶段网站仍在运行。'
docker image save "$app_image" "$postgres_image" "$redis_image" | gzip > "$package_dir/images.tar.gz"
git -C src bundle create "$package_dir/source.bundle" --all
git -C src bundle verify "$package_dir/source.bundle" >/dev/null
printf '%s\n' "$revision" > "$package_dir/source.commit"
docker image inspect --format '{{.Architecture}}' "$app_image" > "$package_dir/architecture.txt"
docker image inspect --format '{{.Id}}' "$app_image" > "$package_dir/app-image-id.txt"
docker image inspect --format '{{json .Config}} {{json .RootFS}} {{.Created}} {{.Architecture}} {{.Os}}' "$app_image" | sha256sum | awk '{print $1}' > "$package_dir/app-image-fingerprint.txt"
printf '%s\n' "$app_image" > "$package_dir/old-image.txt"
cp "$tools_dir/restore.sh" "$package_dir/restore.sh"
mkdir "$package_dir/tools"
for item in install.sh manage.sh export.sh restore.sh compose-env.sh compose.fork.yml; do
  cp "$tools_dir/$item" "$package_dir/tools/$item"
done
echo '暂停应用，备份数据库、密钥、图片和队列。'
app_stopped=0
redis_stopped=0
on_error() {
  local result=${1:-$?}
  trap - ERR INT TERM HUP
  echo "打包失败。工作目录：$package_dir"
  if (( redis_stopped )); then dc start redis || true; fi
  if (( app_stopped )); then dc start new-api || true; fi
  exit "$result"
}
trap on_error ERR
trap 'on_error 130' INT
trap 'on_error 143' TERM HUP
dc stop --timeout 60 new-api
app_stopped=1
dc exec -T postgres pg_dump -U newapi -d newapi -Fc > "$package_dir/main-db.dump"
test -s "$package_dir/main-db.dump"
dc exec -T postgres pg_restore --list < "$package_dir/main-db.dump" > "$package_dir/dump-contents.txt"
dc exec -T redis sh -c 'REDISCLI_AUTH="$REDIS_PASSWORD" redis-cli SAVE' | grep -q OK
dc stop --timeout 30 redis
redis_stopped=1
tar -czf "$package_dir/runtime-files.tar.gz" .env image.env compose.fork.yml compose-env.sh manage.sh data logs images redis-data
tar -tzf "$package_dir/runtime-files.tar.gz" >/dev/null
dc up -d --no-deps --no-build --pull never --wait --wait-timeout 120 redis
redis_stopped=0
(
  cd "$package_dir"
  sha256sum main-db.dump runtime-files.tar.gz images.tar.gz source.bundle source.commit architecture.txt app-image-id.txt app-image-fingerprint.txt old-image.txt restore.sh tools/* > PORTABLE_SHA256SUMS
  touch COMPLETE
)
archive_name="new-api-$mode-$(date -u +%Y%m%dT%H%M%SZ).tar.gz"
tar -czf "$NEWAPI_DEPLOY_DIR/backups/$archive_name" -C "$package_dir" .
(
  cd "$NEWAPI_DEPLOY_DIR/backups"
  sha256sum "$archive_name" > "$archive_name.sha256"
)
if [[ $mode == backup ]]; then
  dc up -d --no-deps --no-build --pull never --wait --wait-timeout 300 new-api
  app_stopped=0
  echo '完整备份成功，网站已恢复运行。'
else
  echo '迁移包生成成功，旧服务器应用保持停止。不要在新旧两台同时启动。'
fi
echo "请下载：$NEWAPI_DEPLOY_DIR/backups/$archive_name"
echo "同时下载：$NEWAPI_DEPLOY_DIR/backups/$archive_name.sha256"
