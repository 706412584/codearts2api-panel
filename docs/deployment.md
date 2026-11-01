# GitHub Actions → GHCR → Debian 自动部署

可以直接在你有管理权限的项目仓库配置部署；本文以 **先在自己的 fork 验证，再给上游提 PR** 为示例路线，并非部署必须先 fork。示例 fork 为 `yourname/codearts2api-panel`，上游为 `qwe5283/codearts2api-panel`；把全文 `yourname` 替换成你的 GitHub 用户名，换仓库时也替换对应地址。镜像名统一小写。

> 这是一份操作指南，不表示你的服务器已经部署，也不表示 GitHub Actions、SSH 或真实账号调用已经测试通过。4 核 4 GB 服务器不需要本地编译镜像，但是否足够承载业务仍取决于并发和上游响应，不能仅凭规格保证。

## 1. 先理解这条流水线

```text
push / PR → GitHub 托管 Runner：测试、构建、容器烟测
                    ↓ 仅 main 的 push / main 上手动运行，且测试成功
                 发布多架构镜像到 GHCR
                    ↓ 仅仓库变量 DEPLOY_ENABLED=true
                 production 环境 → SSH 到 Debian → 拉镜像、等待健康、必要时回退
```

- 工作流是 [`.github/workflows/ci-cd.yml`](../.github/workflows/ci-cd.yml)。任意分支 push 和 PR 做检查；**PR 不发布、不部署，也不使用生产 SSH secrets**。不要为了让 PR 通过而开放写 token 或生产密钥。
- 发布 `ghcr.io/<小写 github.repository>:prod` 和 `:sha-<完整提交 SHA>`；后者不是短 SHA。例如你的稳定入口为 `ghcr.io/yourname/codearts2api-panel:prod`。
- 发布架构为 `linux/amd64`、`linux/arm64`，Docker 拉取时自动选择宿主机架构。构建成功不等于两种架构都做过真实服务器或账号联调。
- 自动部署使用**本次构建返回的 digest**（`镜像名@sha256:…`），不靠会移动的 `prod` 标签判断版本。手动首次启动可以传标签，部署脚本会先拉取并解析成 digest。
- 生产使用 [`deploy/compose.yml`](../deploy/compose.yml)，不在服务器 `git pull`，不使用根目录供本地构建的 `docker-compose.yml`。服务器的配置与数据也不在 Git checkout 中。
- Dockerfile 用 Go 1.25 构建；源码 `go.mod` 的最低版本为 Go 1.22。前端嵌入 Go 程序，服务器不需要 Go、Node.js 或 npm。

**边界：** 这是单容器更新，会短暂中断，长连接/SSE 也可能被截断，不是零停机发布。`/healthz` 只说明进程能响应，不证明华为云上游、账号、模型或额度可用。

## 2. 先检查服务器，尤其是有没有旧服务

以下服务器命令使用 Debian 的 Bash。本机 SSH 命令也按 Bash / Git Bash 编写；Windows 用户可在 Git Bash 中执行。本机与服务器是两台机器，不要在错误的终端粘贴命令。

在**服务器现有管理员会话**中运行：

```sh
uname -m
docker version
docker compose version
docker compose ls -a
docker ps -a --format 'table {{.ID}}\t{{.Names}}\t{{.Image}}\t{{.Ports}}'
command -v bash flock curl openssl
sudo ss -ltnp 'sport = :7866'
```

- `x86_64` 对应 `linux/amd64`；`aarch64` 对应 `linux/arm64`。其他架构先停下来确认，不能强行选一个平台。
- 需要 Docker Engine 和 **Compose V2 插件（`docker compose`，至少 2.20，建议安装当前稳定版）**，以支持健康等待等选项。旧的 `docker-compose` V1 不能替代。
- `flock` 属于 `util-linux`。缺少工具时由管理员安装，例如 `sudo apt-get update && sudo apt-get install -y util-linux curl openssl ca-certificates`。
- Docker 已有，不要盲目重装。Compose 缺失时按 [Docker 官方插件安装说明](https://docs.docker.com/compose/install/linux/)处理；已配置 Docker 官方 apt 源的 Debian 通常可安装 `docker-compose-plugin`，不应直接混装来源不明的包。
- SSH 端口需能被 GitHub 托管 Runner 访问；只允许自己家庭 IP 的防火墙规则可能使自动部署超时。不要因此开放 Docker daemon 端口。

> **如果已经运行过本项目，不能直接执行首次部署。** 脚本不会自动迁移旧容器或识别它的持久化目录。先确认旧 Compose 项目名、配置、`auths/`、`data/` 的真实位置和 UID/GID，做好可恢复的备份；在维护窗口按旧项目的原 Compose 文件/项目名执行 `docker compose … down`（**不加 `-v`**），停止并移除旧容器，再迁入新目录和接管。只 `stop` 仍留有容器，脚本会拒绝接管未管理的同名项目。可用 `docker inspect --format '{{json .Mounts}}' <旧容器ID>` 核对挂载，用 `docker compose ls -a` 核对项目；示意中的省略号/占位符必须换成已核实的实际参数。**不要同时运行两个项目访问同一份凭据/状态，不要复制示例覆盖真实 `config.json`，不要使用 `down -v` 或 prune 清场。** 有旧数据时，下面的新安装权限命令也要先核对路径再用。

## 3. 在自己的 fork 先发布镜像，不开部署

1. 在 GitHub 打开 [`qwe5283/codearts2api-panel`](https://github.com/qwe5283/codearts2api-panel)，Fork 到 `yourname`。确认流水线和 `deploy/` 文件已经包含在自己的分支中；上游尚未合并时，先把这组变更放入自己的 fork。
2. 在 fork 的 **Actions** 页面启用工作流。fork 中的工作流可能默认未启用。
3. 此时**不要设置 `DEPLOY_ENABLED=true`，也不需要任何 SSH secret**。将变更推到自己的 `main`，或在 Actions 中手动运行该工作流并选 `main`。
4. 打开本次运行，确认 `Tests`、`Image smoke test` 和 `Publish GHCR image` 成功，`Deploy production` 被跳过。如果只是推到功能分支或运行 PR 检查，只会检查，不会产生生产镜像。
5. 在你的个人主页 **Packages** 找到 `codearts2api-panel`，确认有 `prod` 和 `sha-<该次完整提交 SHA>`。记录这次提交 SHA，后续下载部署文件时使用同一个版本。

发布登录使用 GitHub 每个 job 自动提供的短期 `GITHUB_TOKEN`，工作流为发布 job 授予 `packages: write`；**不需要自己创建推送用 PAT**。如果因仓库/组织策略禁止包写入而失败，应检查 Actions 权限和包关联，不要先塞入一个全权限 token。

## 4. 处理 GHCR 拉取权限

**公开源码不等于公开镜像。GHCR 新包默认 private。** 两种方式任选其一：

### 推荐：把镜像包设为 public

打开**你的包**的 Package settings，找到可见性设置并改为 Public。修改的是 Packages 中的包，不是仓库可见性。公开后服务器可匿名拉取；注意公开镜像任何人都可下载，镜像中绝不能包含真实配置或账号。

### 必须私有时：服务器单独登录

给服务器准备专用、可轮换且有到期时间的 **classic PAT**，只授予拉取所需的 `read:packages`，并确保所属账号能访问该包；组织启用 SSO 时还需授权。这个凭据与工作流发布使用的 token 是两回事。

完成下一节创建 `deploy` 用户后，**切换到实际运行部署的 `deploy` 用户**再登录。下面通过隐藏输入和标准输入传递 PAT，不把 token 放进命令参数或 shell 历史：

```sh
read -rsp 'GHCR read:packages PAT: ' CR_PAT; printf '\n'
printf '%s' "$CR_PAT" | docker login ghcr.io -u yourname --password-stdin
unset CR_PAT
```

用户名应是 PAT 所属账号。不要用 root 登录后指望 `deploy` 继承登录态，也不要把工作流短期 `GITHUB_TOKEN` 留在服务器。Docker 未配置凭据助手时可能把凭据以可解码形式放在该用户的 `~/.docker/config.json`，应保护其权限并考虑凭据助手。**PAT、SSH 私钥、真实配置都不要贴到聊天、Issue、PR 或 Git。**

## 5. 准备服务器用户、部署目录和真实配置

### 5.1 管理员创建部署用户

下列命令由现有管理员执行；如果已是 root，可省略 `sudo`。如果已有专用账户或目录，先核对，不要重复创建或覆盖。

```sh
sudo adduser --disabled-password --gecos '' deploy
sudo usermod -aG docker deploy
sudo install -d -o deploy -g deploy -m 700 /opt/codearts2api-panel
```

`deploy` 是专用非 root 账户，不必给它一般 sudo 权限。**但 docker 组实际上授予 root 级能力**，不是严格的权限隔离；应保护此账户及其 SSH 私钥。组变更在重新登录后生效。后续以该用户运行 `docker version` 应无需 sudo。

部署路径需为安全的**绝对路径**，建议保持 `/opt/codearts2api-panel`；不要包含空格、`..`、`~` 或 shell 元字符。部署目录本身归 `deploy`，使它能写发布记录和锁文件；敏感挂载另按容器 UID 设置权限。

### 5.2 首次下载固定版本的部署文件

仍在**管理员会话**，把 `SOURCE_REF` 替换成第 3 节成功发布对应的**完整提交 SHA**。不要原样执行占位符。

```sh
DEPLOY_PATH=/opt/codearts2api-panel
SOURCE_REF='<替换为成功发布的完整提交SHA>'
BASE="https://raw.githubusercontent.com/yourname/codearts2api-panel/$SOURCE_REF"

sudo curl --fail --show-error --location "$BASE/deploy/deploy.sh" -o "$DEPLOY_PATH/deploy.sh"
sudo curl --fail --show-error --location "$BASE/deploy/compose.yml" -o "$DEPLOY_PATH/compose.yml"
sudo curl --fail --show-error --location "$BASE/config.example.json" -o "$DEPLOY_PATH/config.example.json"
sudo chown deploy:deploy "$DEPLOY_PATH/deploy.sh" "$DEPLOY_PATH/compose.yml" "$DEPLOY_PATH/config.example.json"
sudo chmod 600 "$DEPLOY_PATH/deploy.sh" "$DEPLOY_PATH/compose.yml" "$DEPLOY_PATH/config.example.json"
```

每条下载都必须成功再继续；404 通常表示 fork/提交还没有这些文件。也可以在临时目录 clone 自己的 fork、checkout 对应 SHA 后复制这三个文件，**不要把 Git checkout 当生产数据目录**。

### 5.3 只为全新安装创建配置

确认这是新安装、`config.json` 不存在后执行。`cp -n` 作为额外保护，不覆盖已有文件；若已有配置，应保留、备份并人工核对，而不是重新套示例。

```sh
sudo cp -n "$DEPLOY_PATH/config.example.json" "$DEPLOY_PATH/config.json"
sudo mkdir -p "$DEPLOY_PATH/auths" "$DEPLOY_PATH/data"

# 各执行一次，得到两把不同的随机密钥；只保存在自己的安全位置。
openssl rand -hex 32
openssl rand -hex 32
sudo nano "$DEPLOY_PATH/config.json"
```

在编辑器中将两次结果分别填入 `api_key`、`admin_key`，不能留空或填成同一把。`api_key` 给 API 客户端；`admin_key` 只用于面板管理，不能分发给调用方。不要将真实文件提交到仓库。

生产 Compose 固定以 **`10001:10001`** 运行，覆盖以下环境：

| 环境项 | 生产值 |
| --- | --- |
| `CA2A_LISTEN` | `:7866` |
| `CA2A_AUTH_DIR` | `/app/auths` |
| `CA2A_STATE_FILE` | `/app/data/state.json` |
| `TZ` | `Asia/Shanghai` |

这些环境变量优先于 JSON；例如把 JSON 的 `listen` 改成别的值不会改变生产监听。宿主机仅发布 `127.0.0.1:7866`，不是直接开放公网。

保存配置后，由管理员设置**三处挂载**的所有权和权限：

```sh
sudo chown 10001:10001 "$DEPLOY_PATH/config.json" "$DEPLOY_PATH/auths" "$DEPLOY_PATH/data"
sudo chmod 600 "$DEPLOY_PATH/config.json"
sudo chmod 700 "$DEPLOY_PATH/auths" "$DEPLOY_PATH/data"
```

不要把整个部署根目录也改成 UID 10001。上述命令面向新建的空目录；迁入旧数据时还需确认内部文件可被 `10001:10001` 读写，不要对未核实的目录递归 chown。之后修改配置需由管理员使用 `sudo`，并在维护窗口重新部署/重启生效。

### 5.4 先手动启动一次

切换到部署用户（这会进入一个新 shell，变量需重新设置）。私有包在此会话按第 4 节登录：

```sh
sudo -iu deploy
DEPLOY_PATH=/opt/codearts2api-panel

docker version
docker compose version
docker pull ghcr.io/yourname/codearts2api-panel:prod
bash "$DEPLOY_PATH/deploy.sh" "$DEPLOY_PATH" \
  ghcr.io/yourname/codearts2api-panel:prod "$DEPLOY_PATH/compose.yml"
```

**首次安装建议直接使用该次 Actions 运行 Summary 中的 `ghcr.io/yourname/codearts2api-panel@sha256:…` 完整引用**替换上面的标签镜像；`:prod` 只是便捷示例，也可换成 `:sha-<同一次完整提交 SHA>`。即使使用 SHA 标签，Docker 本地同仓库的 `RepoDigests` 也可能不唯一；脚本遇到多个候选会拒绝部署，此时应使用 Summary 的精确 digest。脚本会锁定部署目录，将标签解析为 digest 后启动，最多等待 **120 秒**。不要另开终端对同一项目手动 `compose up` 绕过脚本和版本记录。

然后检查：

```sh
curl --fail --show-error http://127.0.0.1:7866/healthz
docker ps --format 'table {{.ID}}\t{{.Names}}\t{{.Status}}\t{{.Image}}'
```

首次账号池为空是正常现象，健康响应并不代表已经能调用模型。要查看面板，可用**本机日常管理员 SSH 身份**建立隧道：

```sh
ssh -N -L 17866:127.0.0.1:7866 -p 22 <日常SSH用户>@<服务器域名或IPv4>
```

在本机打开 `http://127.0.0.1:17866/panel`，使用 `admin_key` 登录，添加自己的 CodeArts 账号，再验证模型列表及实际调用。隧道保持运行；不要使用下面带 `restrict` 的 CI 部署密钥做端口转发。公网访问请自行配置 HTTPS 反向代理，不要为方便而把管理面板裸露到公网。

## 6. 配置专用 SSH 密钥与可信主机信息

### 6.1 在自己的可信机器生成部署密钥

以下命令仅供你在**本机**执行，不是在 Runner 或服务器生成。确保 `~/.ssh` 目录存在，并选择未使用过的文件名；如果提示覆盖已有密钥，应取消。

```sh
ssh-keygen -t ed25519 -f ~/.ssh/codearts2api-deploy -C 'codearts2api-panel-actions'
```

无人值守部署需要此专用密钥的 passphrase **留空**（两次直接回车），因此更要限制用途、保护私钥。`codearts2api-deploy.pub` 是可安装到服务器的公钥；没有 `.pub` 的文件是私钥。

回到**服务器管理员会话**（前面用了 `sudo -iu deploy` 的会话先 `exit` 返回），创建部署用户的 SSH 目录并编辑：

```sh
sudo install -d -o deploy -g deploy -m 700 /home/deploy/.ssh
sudo -u deploy nano /home/deploy/.ssh/authorized_keys
sudo chmod 600 /home/deploy/.ssh/authorized_keys
```

用本机文本编辑器打开 `.pub`，把完整单行公钥追加到 `authorized_keys`，建议在这一行最前面加 `restrict `，形成 `restrict ssh-ed25519 AAAA… codearts2api-panel-actions`。不要覆盖其他已有公钥；示例的 `AAAA…` 不是实际公钥。`restrict` 禁用转发、PTY 等功能，但此账户仍能执行部署命令，尤其仍具有 docker 组的强权限。

### 6.2 核对服务器 host key，而不是现场盲信

1. 通过云厂商控制台/VNC 等独立可信渠道进入服务器，取得指纹：

   ```sh
   sudo ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub
   ```

2. 在**可信本机**替换域名和 SSH 端口，采集候选 known_hosts 文件：

   ```sh
   ssh-keyscan -t ed25519 -p 22 -H your-server.example.com > codearts2api_known_hosts
   ssh-keygen -lf codearts2api_known_hosts
   ```

3. 将输出的 SHA256 指纹与控制台结果逐字核对。`ssh-keyscan` 本身不证明身份；不一致就停下来排查，不能关闭校验继续。
4. 用本机文本编辑器打开 `codearts2api_known_hosts`，后面把**完整内容**保存为 `DEPLOY_KNOWN_HOSTS`。这是包含 host/哈希 host、端口形式和公钥的 known_hosts 行，可有多行，**不是**单独的 `SHA256:…` 指纹。非 22 端口必须按实际端口重新采集。

可以先在本机验证这个专用身份是否能执行 Docker，替换占位符和端口：

```sh
ssh -i ~/.ssh/codearts2api-deploy -p 22 -o IdentitiesOnly=yes -o BatchMode=yes \
  -o StrictHostKeyChecking=yes -o UserKnownHostsFile=./codearts2api_known_hosts \
  deploy@your-server.example.com 'docker version && docker compose version'
```

自动部署的 [`scripts/deploy-remote.sh`](../scripts/deploy-remote.sh) 使用 `StrictHostKeyChecking=yes` 和预存 known_hosts，不在运行时 `ssh-keyscan` 自动接受主机。

## 7. 在 fork 设置 Variables 和 Secrets

在**自己的 fork**打开 **Settings → Environments**，创建名为 **`production`** 的环境。建议只允许 `main` 部署；如设置 Required reviewers，部署会等待人工批准，不再是完全无人值守。

### 仓库变量：Settings → Secrets and variables → Actions → Variables

| 名称 | 示例 / 默认 | 说明 |
| --- | --- | --- |
| `DEPLOY_ENABLED` | 先不设，最后才设 `true` | **必须是仓库级变量**；精确值 `true` 才启用 SSH，不能只放在 environment 中 |
| `DEPLOY_HOST` | `your-server.example.com` | 服务器域名或 IPv4；不带 `ssh://`、用户名、端口、路径，不支持 IPv6 字面量 |
| `DEPLOY_USER` | `deploy` | 专用部署用户 |
| `DEPLOY_PORT` | `22` | 可省略，默认 22；非默认 SSH 端口要和 known_hosts 对应 |
| `DEPLOY_PATH` | `/opt/codearts2api-panel` | 可省略；安全绝对路径，无空格、`..` 或 shell 元字符；须提前创建并归部署用户 |

为避免环境作用域混淆，首次按表把这些参数统一放在仓库 Variables。地址、端口、路径不是密码；私钥不能放 Variables。

### 环境 secrets：Settings → Environments → production → Environment secrets

| 名称 | 内容 |
| --- | --- |
| `DEPLOY_SSH_KEY` | 本机专用 ed25519 **私钥全文**，含 BEGIN/END 行和换行 |
| `DEPLOY_KNOWN_HOSTS` | 经独立渠道核验后的 known_hosts **全文**，不是指纹 |

也支持仓库 Actions secrets，但建议生产凭据放 `production` 环境统一保护。用**本机文本编辑器**打开私钥并复制到 GitHub Secret 界面，避免把它输出到日志、聊天或仓库；保存后 GitHub 不会回显原文，填错应重新保存。

真实 `config.json`、CodeArts 凭据和状态只留服务器，不上传 GitHub。私有镜像的拉取 PAT 也留在服务器 `deploy` 用户的 Docker 登录配置中，不需作为发布 secret。

## 8. 启用自动更新并逐项验收

手动部署和专用 SSH 都确认正常后，才把**仓库变量** `DEPLOY_ENABLED` 设为 `true`。修改变量本身不会触发发布，需要再推一次 `main`，或在 Actions 选择 `main` 手动运行。发布和部署前会核对提交是否仍为当前 `main` HEAD，过时 run 会被拒绝；重跑旧 run 不是回退方式，回退走第 9 节服务器端的固定 digest。

按顺序检查，不要只看绿色总状态：

1. 测试与容器烟测成功；发布 job 产出本次提交的 `prod`、完整 SHA 标签和 digest。
2. 部署 job 使用 `production` 环境且实际运行，而不是 skipped；若配置审核，先完成审核。
3. Runner 只向 `$DEPLOY_PATH/releases/${GITHUB_RUN_ID}-${GITHUB_RUN_ATTEMPT}` 上传本次 `deploy.sh`、`compose.yml`，再远程执行；不会 `git pull`，不会发送真实配置。
4. 在服务器查看容器状态、`/healthz` 和部署记录；`.current-image` 应为本次部署的 digest 引用，而不是 `:prod`。
5. 打开面板，确认已有账号和配置仍在；用自己的账号验证模型与一次真实请求。没有账号的首次安装不应期待已有模型。

可以先在自己的 fork 做一个容易观察、无敏感数据的变更，确认完整链路后再操作上游。建议为 `main` 启用分支保护/Ruleset，要求通过实际的 CI 检查、审查后合并，限制能改工作流和生产环境的人。**向 main 推送的代码会拿到发布能力，启用部署后还会影响服务器，不要把不可信代码直接推 main。** PR 检查始终不应接触生产 secrets；首次 fork PR 的检查可能需上游维护者批准运行。

## 9. 更新失败、回退与备份

[`deploy/deploy.sh`](../deploy/deploy.sh) 管理这些记录：

| 文件 | 用途 |
| --- | --- |
| `.current-image` / `.previous-image` | 当前 / 上一个成功管理版本的 digest 镜像引用 |
| `.current-compose.yml` / `.previous-compose.yml` | 与这两个版本配套的 Compose 快照 |

脚本使用部署目录锁串行更新，等待健康最多 120 秒。**更新失败且已有脚本管理的成功版本时**，会尝试用旧 **Compose + digest** 回退，并报告本次部署失败；回退也可能失败，必须检查输出和服务状态。**首次失败没有可回退版本**，脚本会停止并移除本次失败的服务容器（不删除挂载数据），然后报错；修复原因后可重试。若清理本身失败，先按日志人工核对项目和残留容器，不能绕过检查直接接管。它不会凭空恢复旧本地 Compose 项目。

手动回到上一个成功版本，在**部署用户**的 Bash 中执行：

```sh
DEPLOY_PATH=/opt/codearts2api-panel
bash "$DEPLOY_PATH/deploy.sh" "$DEPLOY_PATH" \
  "$(cat "$DEPLOY_PATH/.previous-image")" "$DEPLOY_PATH/.previous-compose.yml"
```

先确认两个 previous 文件都存在，保留所需 digest 镜像；如果正在排查，先把 `DEPLOY_ENABLED` 改为 `false`，防止下一次发布覆盖手动选择。关闭变量不等于取消已经启动的 job，必要时在 Actions 中取消该次运行。`previous` 表示上一次成功部署调用，不保证是上一个不同版本；重复部署同一镜像也会更新记录，需要长期保留的回退点应另行保存其镜像引用和 Compose。

**回退只恢复镜像与 Compose，不恢复 `config.json`、`auths/`、`data/`，也不逆转新版已经写入的数据。** 更新前应做好三处敏感数据的独立备份，必要时在停服窗口做一致性备份，恢复也应人工核对版本与权限。脚本不会覆盖这三处，不会执行 `down -v` 或 prune；不要自行清理仍需回退的包/digest。单容器重新创建期间有短暂中断，不能承诺 SSE 保持。

## 10. 常见问题

| 现象 | 优先检查 |
| --- | --- |
| GHCR `denied` / `unauthorized` | 包是否仍 private？镜像命名空间是否为你自己的小写 fork 名？私有包是否由 **deploy 用户**登录、classic PAT 有 `read:packages`、账号能读包且未过期？发布失败则另查 `GITHUB_TOKEN` 的 `packages:write`、组织策略和已有包是否关联本仓库；不要混淆服务器拉取与 Runner 发布凭据。 |
| `Host key verification failed` | Secret 必须是完整 known_hosts 行，host 和端口要一致；服务器重装会换 key，应重新通过控制台核验后更新。不要加 `StrictHostKeyChecking=no`，也不要把实时扫描结果当信任依据。 |
| SSH 超时 / `Permission denied (publickey)` | 云安全组、系统防火墙、实际端口、用户名、公私钥是否配对；`.ssh` 为 700、`authorized_keys` 为 600 且归 deploy，私钥无 passphrase。别把公钥填入 `DEPLOY_SSH_KEY`。 |
| Docker socket 权限不足 | deploy 是否在 docker 组、是否已重新登录、非交互 SSH 中 `docker version` 能否工作；不要改成 root 登录来掩盖问题。 |
| 7866 已占用 / 项目冲突 | 查 `docker ps -a`、`docker compose ls -a` 和 `ss`；旧本地 Compose 项目须先确认数据、备份并用原项目 `down` 移除旧容器（不加 `-v`），仅 stop 不足以接管。不要直接杀掉不明服务或并行挂同一份数据。 |
| `permission denied` / 配置路径是目录 | 三处挂载必须预建，`config.json` 是文件而非目录，容器固定 `10001:10001`；核对文件 600、目录 700 和内部文件属主。不要把敏感目录 chmod 777，也不要误把部署根目录 chown 成 10001。 |
| 新版健康失败，回退后旧版也失败 | 用 `docker ps -a` 找到对应容器后 `docker logs --tail 100 <容器ID>`，检查配置、端口、磁盘和数据兼容性；旧版也不能健康时自动回退并不保证恢复。先保护数据，必要时人工恢复备份；不要连续重跑掩盖首个错误。 |
| `no matching manifest` / `exec format error` | 核对 `uname -m`，包是否确实包含 `linux/amd64`、`linux/arm64`；不要把旧的单架构手工镜像当作本次多架构发布，也不要强设错误 `platform`。 |
| 部署总是 skipped / 旧 Compose 不认识 `--wait` | `DEPLOY_ENABLED` 要是仓库变量且等于 `true`，触发来自 main push 或 main 手动运行，前置测试发布成功；检查 environment 审核/分支规则。Compose 需 V2 ≥ 2.20，不是 V1。 |
| healthy，但没有模型或调用失败 | `/healthz` 只检查进程。首次账号池为空正常，仍需导入有效账号、检查套餐/额度并做上游实际请求。 |

日志可能含错误详情，求助前先脱敏，不要上传私钥、PAT、真实配置、凭据或完整 OAuth 回调 URL。

## 11. 在自己的 fork 验证后给朋友提 PR

1. 先在 `yourname/codearts2api-panel` 完成上述检查，区分“本地静态检查”“Actions 发布”“Debian 更新”“真实账号请求”分别做到哪一步；没有做过的明确标注未验证。
2. 从自己的变更分支向 `qwe5283/codearts2api-panel` 的目标分支提交 PR，写清测试结果、部署开关默认关闭、所需 Variables/Secrets、回退边界和维护注意事项，不携带任何生产资料。
3. 上游 PR 只做 CI；朋友合并后如要部署，应在**他自己的仓库**另设 production 环境、变量、专用密钥及包权限。上游镜像地址会是 `ghcr.io/qwe5283/codearts2api-panel`，不会共享你 fork 的 secrets 或服务器。

## 官方参考

- [GitHub：触发工作流的事件及 fork 限制](https://docs.github.com/en/actions/reference/workflows-and-actions/events-that-trigger-workflows)
- [GitHub：发布 Docker 镜像与 GITHUB_TOKEN](https://docs.github.com/en/actions/tutorials/publish-packages/publish-docker-images)
- [GitHub：Container registry、默认私有、匿名公开拉取和 classic PAT](https://docs.github.com/en/packages/working-with-a-github-packages-registry/working-with-the-container-registry)
- [GitHub：管理部署环境及 secrets](https://docs.github.com/en/actions/how-tos/deploy/configure-and-manage-deployments/manage-environments)
- [Docker：Compose 插件安装](https://docs.docker.com/compose/install/linux/)、[健康等待选项](https://docs.docker.com/reference/cli/docker/compose/up/)、[多架构镜像](https://docs.docker.com/build/building/multi-platform/)
- [Docker：非 root 用户与 docker 组风险](https://docs.docker.com/engine/install/linux-postinstall/)、[登录与凭据存储](https://docs.docker.com/reference/cli/docker/login/)
