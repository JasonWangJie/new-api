#!/usr/bin/env bash
set -Eeuo pipefail
umask 077
tools_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
export NEWAPI_DEPLOY_DIR=${NEWAPI_DEPLOY_DIR:-/opt/new-api}
origin=${1:-https://api.aiimg.lol}
for command in docker git openssl python3 curl tar gzip flock; do
  command -v "$command" >/dev/null || { echo "缺少 $command，请先按指南安装环境。"; exit 1; }
done
docker compose version >/dev/null
docker buildx version >/dev/null
python3 - "$origin" <<'PY'
import sys
from urllib.parse import urlsplit
u = urlsplit(sys.argv[1])
if u.scheme != 'https' or not u.hostname or u.username or u.password or u.path or u.query or u.fragment or '*' in u.netloc:
    sys.exit('域名必须是 https://域名，不能带路径或末尾斜杠')
PY
mkdir -p "$NEWAPI_DEPLOY_DIR"
cd "$NEWAPI_DEPLOY_DIR"
exec 9>.manage.lock
flock -n 9 || { echo '另一个部署操作正在执行。'; exit 1; }
for item in .env image.env compose.fork.yml compose-env.sh manage.sh .git; do
  [[ ! -e $item ]] || { echo "已有 $item，停止首次安装，避免覆盖现有部署。"; exit 1; }
done
for item in data logs images postgres-data redis-data; do
  if [[ -d $item && -n $(find "$item" -mindepth 1 -print -quit) ]]; then
    echo "$item 已有数据，首次安装不会覆盖它。"
    exit 1
  fi
done
project=${NEWAPI_PROJECT_NAME:-new-api-fork}
if [[ -n $(docker ps -aq --filter "label=com.docker.compose.project=$project") ]]; then
  echo '这个部署项目已有容器，停止首次安装，请先检查原部署。'
  exit 1
fi
if [[ ! -d src ]]; then
  git clone --branch main https://github.com/JasonWangJie/new-api.git src
else
  [[ $(git -C src remote get-url origin) == https://github.com/JasonWangJie/new-api.git && $(git -C src branch --show-current) == main && -z $(git -C src status --porcelain) ]] || { echo 'src 不是干净的 Fork main，停止安装。'; exit 1; }
fi
mkdir -p data logs images/temporary images/durable postgres-data redis-data backups
cp "$tools_dir/compose.fork.yml" compose.fork.yml
cp "$tools_dir/compose-env.sh" compose-env.sh
cp "$tools_dir/manage.sh" manage.sh
chmod 700 manage.sh
cat > .env <<EOF
POSTGRES_PASSWORD=$(openssl rand -hex 24)
REDIS_PASSWORD=$(openssl rand -hex 24)
SESSION_SECRET=$(openssl rand -hex 32)
CRYPTO_SECRET=$(openssl rand -hex 32)
ASYNC_IMAGE_ACTIVE_KEY_ID=images-1
ASYNC_IMAGE_PAYLOAD_KEYS=images-1:$(openssl rand -base64 32)
ASYNC_IMAGE_REDIS_PREFIX=new-api:images
REDIS_KEY_PREFIX=new-api
SESSION_COOKIE_SECURE=true
SESSION_COOKIE_TRUSTED_URL=$origin
EOF
printf 'APP_IMAGE_TAG=%s\nAPP_IMAGE_REPOSITORY=%s\n' "$(git -C src rev-parse HEAD)" "${NEWAPI_IMAGE_REPOSITORY:-new-api-fork}" > image.env
chmod 600 .env image.env compose.fork.yml compose-env.sh
. ./compose-env.sh
unset APP_IMAGE_TAG
dc config --quiet
dc pull postgres redis
dc build --pull new-api
dc up -d --no-build --pull never --wait --wait-timeout 300
echo "首次部署成功。宝塔代理目标：http://127.0.0.1:${NEWAPI_PORT:-3000}"
echo "请通过 $origin 初始化管理员，并在后台把服务器地址设为 $origin"
