# Project Instructions

## Generate Commit Message

- Use Simplified Chinese for all commit messages.
- Prefer Conventional Commits (`feat` / `fix` / `docs` / `chore` / …).

## Fork 发行版（强制）

本仓库是可一键安装的发行版 Fork（`JasonWangJie/sub2api`），不是仅本地开发仓库。

完整约定见：

- `.cursor/rules/fork-release-deploy.mdc`（alwaysApply）
- `deploy/FORK_RELEASE.md`

合并 `upstream`（原作者）时，不得把 `deploy/install.sh`、`upgrade.sh`、Release CI、仓库 URL、`DATA_DIR=/etc/sub2api` 等 Fork 身份恢复为 `Wei-Shaw/sub2api`。
