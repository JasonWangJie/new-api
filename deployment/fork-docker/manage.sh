#!/usr/bin/env bash
set -Eeuo pipefail
umask 077
NEWAPI_DEPLOY_DIR=${NEWAPI_DEPLOY_DIR:-/opt/new-api}
export NEWAPI_DEPLOY_DIR
cd "$NEWAPI_DEPLOY_DIR"
. ./compose-env.sh
unset APP_IMAGE_TAG

action=${1:-status}
app_stopped=0
redis_stopped=0
switched=0
backup_dir=''

on_error() {
  local result=${1:-$?}
  trap - ERR
  echo '操作失败，已停止后续步骤。'
  if (( redis_stopped )); then
    dc start redis || true
  fi
  if (( app_stopped && ! switched )); then
    echo '尚未切换镜像，正在恢复原应用。'
    dc start new-api || true
  fi
  if (( switched )); then
    echo '已尝试启动新版本。请检查日志；数据库可能已迁移，不会自动切回旧版本。'
  fi
  if [[ -n $backup_dir ]]; then
    echo "本次备份目录：$backup_dir（只有存在 COMPLETE 才表示备份完成）"
  fi
  exit "$result"
}
trap on_error ERR
trap 'on_error 130' INT
trap 'on_error 143' TERM HUP

case "$action" in
  start|restart|backup|update|resume)
    exec 9>"$NEWAPI_DEPLOY_DIR/.manage.lock"
    flock -n 9 || { echo '另一个启动、备份或更新正在执行，请稍后重试。'; exit 1; }
    ;;
esac

backup_runtime() {
  local app_container old_image
  app_container=$(dc ps --status running -q new-api)
  [[ -n $app_container ]] || { echo '应用未运行，先检查 status 和 logs。'; return 1; }
  old_image=$(docker inspect --format '{{.Config.Image}}' "$app_container")
  backup_dir="$NEWAPI_DEPLOY_DIR/backups/$(date -u +%Y%m%dT%H%M%SZ)"
  mkdir -m 700 "$backup_dir"
  echo "备份到：$backup_dir"
  cp image.env "$backup_dir/image.env"
  printf '%s\n' "$old_image" > "$backup_dir/old-image.txt"
  docker image inspect --format '{{.Id}}' "$old_image" > "$backup_dir/old-image-id.txt"
  dc stop --timeout 60 new-api
  app_stopped=1
  dc exec -T postgres pg_dump -U newapi -d newapi -Fc > "$backup_dir/main-db.dump"
  test -s "$backup_dir/main-db.dump"
  dc exec -T postgres pg_restore --list < "$backup_dir/main-db.dump" > "$backup_dir/dump-contents.txt"
  dc exec -T redis sh -c 'REDISCLI_AUTH="$REDIS_PASSWORD" redis-cli SAVE' | grep -q OK
  dc stop --timeout 30 redis
  redis_stopped=1
  tar -czf "$backup_dir/runtime-files.tar.gz" .env image.env compose.fork.yml compose-env.sh manage.sh data logs images redis-data
  tar -tzf "$backup_dir/runtime-files.tar.gz" > "$backup_dir/runtime-files-list.txt"
  dc up -d --no-deps --no-build --pull never --wait --wait-timeout 120 redis
  redis_stopped=0
  sha256sum "$backup_dir/main-db.dump" "$backup_dir/runtime-files.tar.gz" > "$backup_dir/SHA256SUMS"
  touch "$backup_dir/COMPLETE"
}

case "$action" in
  status)
    dc ps
    curl --fail --silent --show-error --max-time 15 "http://127.0.0.1:${NEWAPI_PORT:-3000}/api/status" | python3 -c 'import json,sys; d=json.load(sys.stdin); print("应用健康：",d.get("success"),"；服务器地址：",d.get("data",{}).get("server_address"))'
    ;;
  version)
    printf '源码提交：'; git -C src rev-parse HEAD
    container=$(dc ps -q new-api)
    test -n "$container"
    image=$(docker inspect --format '{{.Config.Image}}' "$container")
    echo "运行镜像：$image"
    docker image inspect --format '源码来源：{{ index .Config.Labels "org.opencontainers.image.source" }}
构建提交：{{ index .Config.Labels "org.opencontainers.image.revision" }}' "$image"
    ;;
  logs)
    dc logs --tail 80 new-api
    ;;
  start|resume)
    dc up -d --no-build --pull never --wait --wait-timeout 300
    echo '服务启动成功。'
    ;;
  restart)
    dc stop --timeout 60 new-api
    app_stopped=1
    dc up -d --no-deps --no-build --pull never --wait --wait-timeout 300 new-api
    app_stopped=0
    echo '应用重启成功。'
    ;;
  backup)
    [[ ${2:-} == "" || ${2:-} == --keep-stopped ]] || { echo "备份参数错误"; exit 2; }
    backup_runtime
    if [[ ${2:-} == --keep-stopped ]]; then
      echo "备份完成，应用保持停止：$backup_dir"
      exit 0
    fi
    dc up -d --no-deps --no-build --pull never --wait --wait-timeout 300 new-api
    app_stopped=0
    echo "备份完成，应用已恢复。备份目录：$backup_dir"
    ;;
  update)
    if [[ ${2:-} != --ready ]]; then
      echo '请先暂停新任务并等待现有任务完成，然后执行：bash /opt/new-api/manage.sh update --ready'
      exit 1
    fi
    [[ $(git -C src remote get-url origin) == https://github.com/JasonWangJie/new-api.git ]] || { echo '源码来源不是自己的 Fork，停止更新。'; exit 1; }
    [[ $(git -C src branch --show-current) == main && -z $(git -C src status --porcelain) ]] || { echo '源码不在 main 或存在修改；已停止，不会覆盖文件。'; exit 1; }
    dc config --quiet
    container=$(dc ps --status running -q new-api)
    test -n "$container"
    old_image=$(docker inspect --format '{{.Config.Image}}' "$container")
    git -C src fetch origin main
    git -C src merge --ff-only origin/main
    next_sha=$(git -C src rev-parse HEAD)
    [[ $next_sha == $(git -C src rev-parse origin/main) ]] || { echo '服务器源码含有未推送的提交，停止更新。'; exit 1; }
    image_repository=${old_image%:*}
    next_image="$image_repository:$next_sha"
    if [[ $old_image == "$next_image" ]]; then
      echo '服务器已经运行自己的仓库最新提交，无需更新。'
      exit 0
    fi
    echo '先构建新镜像；这一阶段原应用继续运行。'
    APP_IMAGE_TAG="$next_sha" dc build --pull new-api
    backup_runtime
    printf 'APP_IMAGE_TAG=%s\nAPP_IMAGE_REPOSITORY=%s\n' "$next_sha" "$image_repository" > image.env.new
    chmod 600 image.env.new
    mv image.env.new image.env
    switched=1
    dc up -d --no-deps --no-build --pull never --wait --wait-timeout 300 new-api
    app_stopped=0
    curl --fail --silent --show-error --max-time 15 "http://127.0.0.1:${NEWAPI_PORT:-3000}/api/status" | python3 -c 'import json,sys; assert json.load(sys.stdin).get("success") is True'
    echo "更新成功。当前提交：$next_sha"
    echo "更新前备份：$backup_dir"
    ;;
  *)
    echo '可用指令：status、version、logs、start、restart、backup、update --ready、resume'
    exit 2
    ;;
esac
