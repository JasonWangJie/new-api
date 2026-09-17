export NEWAPI_DEPLOY_DIR=${NEWAPI_DEPLOY_DIR:-/opt/new-api}
dc() {
  docker compose --project-name "${NEWAPI_PROJECT_NAME:-new-api-fork}" --project-directory "$NEWAPI_DEPLOY_DIR" --env-file "$NEWAPI_DEPLOY_DIR/.env" --env-file "$NEWAPI_DEPLOY_DIR/image.env" -f "$NEWAPI_DEPLOY_DIR/compose.fork.yml" "$@"
}
