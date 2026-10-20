#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

if (( $# )); then
  echo '用法：bash /opt/new-api/update-confirm.sh'
  [[ $# == 1 && ( $1 == --help || $1 == -h ) ]] && exit 0
  exit 2
fi

export NEWAPI_DEPLOY_DIR=${NEWAPI_DEPLOY_DIR:-/opt/new-api}
cd "$NEWAPI_DEPLOY_DIR"
. ./compose-env.sh
unset APP_IMAGE_TAG APP_IMAGE_REPOSITORY

app_stopped=0
redis_stopped=0
switched=0
backup_dir=''

on_error() {
  local result=${1:-$?}
  trap - ERR INT TERM HUP
  set +e
  echo '更新已中止。'
  if (( redis_stopped )); then
    dc start redis || echo 'Redis 恢复失败，请检查服务状态。'
  fi
  # The signal may arrive immediately before or after the atomic replacement.
  if (( switched )) && cmp -s image.env "$backup_dir/image.env"; then
    switched=0
  fi
  if (( app_stopped && ! switched )); then
    echo '尚未切换镜像，正在恢复原应用。'
    dc start new-api || echo '原应用恢复失败，请检查服务状态。'
  fi
  if (( switched )); then
    echo '镜像配置已切换，数据库可能已迁移；不会自动回滚，请检查 status 和 logs。'
  fi
  if [[ -n $backup_dir ]]; then
    echo "本次备份：$backup_dir（只有存在 COMPLETE 才表示备份完成）"
  fi
  exit "$result"
}
trap on_error ERR
trap 'on_error 130' INT
trap 'on_error 143' TERM HUP

for command in docker git flock sha256sum tar gzip python3 curl cmp mktemp; do
  command -v "$command" >/dev/null || { echo "缺少 $command，停止更新。"; exit 1; }
done
exec 9>.manage.lock
flock -n 9 || { echo '另一个启动、备份或更新正在执行，请稍后重试。'; exit 1; }

[[ $(git -C src remote get-url origin) == https://github.com/JasonWangJie/new-api.git ]] || { echo '源码来源不是自己的 Fork，停止更新。'; exit 1; }
[[ $(git -C src branch --show-current) == main && -z $(git -C src status --porcelain) ]] || { echo '源码不在 main 或存在修改，停止更新。'; exit 1; }
configured_image=$(dc config --format json | python3 -c 'import json,sys; print(json.load(sys.stdin)["services"]["new-api"]["image"])')
runtime_checksums=$(sha256sum .env image.env compose.fork.yml compose-env.sh manage.sh)
container=$(dc ps --status running -q new-api)
[[ -n $container ]] || { echo '应用未运行，请先检查 status 和 logs。'; exit 1; }
old_image=$(docker inspect --format '{{.Config.Image}}' "$container")
old_image_id=$(docker inspect --format '{{.Image}}' "$container")
[[ $configured_image == "$old_image" ]] || { echo '镜像配置与运行中的应用不一致，请先检查现有部署。'; exit 1; }

git -C src fetch origin main
git -C src merge --ff-only origin/main
next_sha=$(git -C src rev-parse HEAD)
[[ $next_sha == $(git -C src rev-parse origin/main) ]] || { echo '服务器源码含有未推送的提交，停止更新。'; exit 1; }
image_repository=${old_image%@*}
if [[ ${image_repository##*/} == *:* ]]; then
  image_repository=${image_repository%:*}
fi
next_image="$image_repository:$next_sha"
if [[ $old_image == "$next_image" ]]; then
  echo '服务器已经运行自己的仓库最新提交，无需更新。'
  exit 0
fi

echo '正在构建新镜像；原应用继续运行，构建会占用服务器资源。'
APP_IMAGE_REPOSITORY="$image_repository" APP_IMAGE_TAG="$next_sha" dc build --pull new-api
next_image_id=$(docker image inspect --format '{{.Id}}' "$next_image")
[[ -n $next_image_id ]] || { echo '未找到构建镜像，停止更新。'; exit 1; }

echo "构建完成：$next_image"
echo "镜像 ID：$next_image_id"
echo '现在等待你的确认，尚未停服或开始备份；等待没有超时。'
echo '请自行暂停新任务提交，等待已有任务完成（包括结果保存、结算）。脚本不会自动排空任务。'
echo '等待期间持有部署锁，其他管理脚本的启动、备份和更新操作会被阻止。'
echo '确认现在停服、备份并更新，请输入 UPDATE 后回车；其他输入取消：'
if ! IFS= TMOUT=0 read -r confirmation; then
  echo '未收到完整确认，已取消；原服务继续运行。'
  exit 0
fi
if [[ $confirmation != UPDATE ]]; then
  echo '已取消；原服务继续运行。'
  exit 0
fi

# Refuse stale approval if another operator changed the deployment while waiting.
sha256sum --check --status <<< "$runtime_checksums" || { echo '部署配置已变化，停止更新，请重新运行脚本。'; exit 1; }
[[ $(dc ps --status running -q new-api) == "$container" &&
   $(docker inspect --format '{{.Config.Image}}' "$container") == "$old_image" &&
   $(docker inspect --format '{{.Image}}' "$container") == "$old_image_id" ]] || { echo '运行中的应用已变化，停止更新，请重新运行脚本。'; exit 1; }
[[ $(docker image inspect --format '{{.Id}}' "$next_image") == "$next_image_id" ]] || { echo '构建镜像已变化，停止更新，请重新运行脚本。'; exit 1; }
[[ $(git -C src rev-parse HEAD) == "$next_sha" && -z $(git -C src status --porcelain) ]] || { echo '源码已变化，停止更新，请重新运行脚本。'; exit 1; }

mkdir -p backups
backup_dir=$(mktemp -d "$NEWAPI_DEPLOY_DIR/backups/$(date -u +%Y%m%dT%H%M%SZ)-XXXXXX")
echo "开始停服备份：$backup_dir"
cp image.env "$backup_dir/image.env"
printf '%s\n' "$old_image" > "$backup_dir/old-image.txt"
printf '%s\n' "$old_image_id" > "$backup_dir/old-image-id.txt"
app_stopped=1
dc stop --timeout 60 new-api
dc exec -T postgres pg_dump -U newapi -d newapi -Fc > "$backup_dir/main-db.dump"
test -s "$backup_dir/main-db.dump"
dc exec -T postgres pg_restore --list < "$backup_dir/main-db.dump" > "$backup_dir/dump-contents.txt"
dc exec -T redis sh -c 'REDISCLI_AUTH="$REDIS_PASSWORD" redis-cli SAVE' | grep -q OK
redis_stopped=1
dc stop --timeout 30 redis
backup_files=(.env image.env compose.fork.yml compose-env.sh manage.sh data logs redis-data)
if [[ -f update-confirm.sh ]]; then
  backup_files+=(update-confirm.sh)
fi
tar -czf "$backup_dir/runtime-files.tar.gz" "${backup_files[@]}"
tar -tzf "$backup_dir/runtime-files.tar.gz" > "$backup_dir/runtime-files-list.txt"
dc up -d --no-deps --no-build --pull never --wait --wait-timeout 120 redis
redis_stopped=0
sha256sum "$backup_dir/main-db.dump" "$backup_dir/runtime-files.tar.gz" > "$backup_dir/SHA256SUMS"
touch "$backup_dir/COMPLETE"

printf 'APP_IMAGE_TAG=%s\nAPP_IMAGE_REPOSITORY=%s\n' "$next_sha" "$image_repository" > image.env.new
chmod 600 image.env.new
switched=1
mv image.env.new image.env
dc up -d --no-deps --no-build --pull never --wait --wait-timeout 300 new-api
app_stopped=0
address=$(dc port --index 1 new-api 3000)
[[ -n $address ]] || { echo '未找到应用端口。'; on_error 1; }
curl --fail --silent --show-error --max-time 15 "http://$address/api/status" | python3 -c 'import json,sys; assert json.load(sys.stdin).get("success") is True'
echo "更新成功。当前提交：$next_sha"
echo "更新前备份：$backup_dir"
echo '正在清理 Docker 构建缓存；保留应用镜像、容器和数据卷。'
cache_prune=(docker builder prune --all --force)
if docker buildx version >/dev/null 2>&1; then
  cache_prune=(docker buildx prune --all --force)
fi
if ! "${cache_prune[@]}"; then
  echo '警告：Docker 构建缓存清理失败，更新已成功；请检查上方错误并稍后手动清理。' >&2
fi
