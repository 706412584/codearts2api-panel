# 华为云 CodeArts 智能编程 API 接口文档

提取日期：2026-11-01。

本文从 `references/` 四个开源仓库的当前源码、测试及历史逆向记录中提取接口。范围是 CodeArts Agent / 智能编程服务，不覆盖华为云 CodeArts 全产品线的项目、代码托管、流水线等 API。

**本次完成的是静态源码交叉核对，没有使用真实账号调用云端。** 下文“历史观测”指仓库维护者的记录；示例 JSON 仅表达实现已发送或读取的字段，不是完整官方 Schema，也不代表这些字段全部必填。客户端默认值、兼容处理和重试策略会明确标注。

## 1. 来源与版本

| 标记 | 本地目录 | origin | 提取时 HEAD | 贡献 |
| --- | --- | --- | --- | --- |
| N | `references/codearts2api/` | [zhuweiyou/codearts2api](https://github.com/zhuweiyou/codearts2api) | `a20d1145cfd2847caf537b904062d07b04a8aa3b` | 独立宿主、管理 RPC、OpenAI 网关；华为云协议交给依赖插件 |
| G | `references/codearts2api-go/` | [HITZY2002/codearts2api](https://github.com/HITZY2002/codearts2api) | `8d1e43d6f31279ac20cf94b92eff20bdba784b76` | OAuth、DPoP、HMAC、聊天、模型、活动、身份 |
| P | `references/codearts2api-py/` | [Johnsheng1/codearts2api](https://github.com/Johnsheng1/codearts2api) | `d813ca0456f23cd73bb3bbee16627bc2a40036f9` | 永久 AK/SK、聊天、模型、福利状态及余额 |
| H | `references/deepseek-harness-codearts/` | [iJetLi/deepseek-harness-codearts](https://gitee.com/iJetLi/deepseek-harness-codearts) | `2bf76ce66f1214f18ee3202238fb0f2d02cc791f` | OAuth、模型能力、聊天/图片、排队、套餐、积分 |

四个参考仓库提取时工作树均干净。文末来源链接固定到上述提交。

N 的 `package.json` 固定依赖 `dsh-providers-hub` 的 `eb09210423ace12edf4e2793a4c3780386a2f63f`，`src/host.js` 直接装载该插件。**不能把 N 固定的插件版本与 H 当前提交视作完全相同的协议版本。** 本文 N 部分仅描述本仓能直接核实的职责。[N-package] [N-host]

## 2. 基础地址与接口索引

| 简称 | 基础地址 | 用途 |
| --- | --- | --- |
| SNAP | `https://snap-access.cn-north-4.myhuaweicloud.com` | 聊天、Agent、模型、套餐、积分、ticket |
| STS | `https://sts.cn-north-4.myhuaweicloud.com` | OAuth 凭据换取/续期、身份 |
| BENEFIT | `https://opengw.developer.huaweicloud.com` | 免费模型目录、Token 福利额度 |
| PORTAL | `https://codearts.huaweicloud.com` | 浏览器授权与结果页 |

来源：[G-const] [H-login] [P-server] [P-benefit]。

### 2.1 当前代码有调用实现的接口

“HMAC”指第 4 节的 AK/SK 签名；临时凭据还需 `X-Security-Token`。不同端点的路由头与签名范围见第 4.2 节。

| 方法 | 基础地址 | 路径 | 用途 | 证据 |
| --- | --- | --- | --- | --- |
| POST | STS | `/v1/oauth2/tokens` | 授权码换凭据 / refresh_token 续期，form + DPoP | [G-client] [H-oauth] |
| GET | SNAP | `/snap-manager/v1/login/ticket` | ticket 登录轮询，插件身份头 | [G-client] [H-login] |
| GET | STS | `/v5/caller-identity` | 身份查询，HMAC | [G-client] |
| GET | SNAP | `/v1/agent-center/agents/useragents` | 用户 Agent 列表，HMAC | [G-models] [P-server] [H-models] |
| GET | SNAP | `/v1/agent-center/agents/detail` | Agent 模型与上下文参数，HMAC | [G-models] [P-server] [H-models] |
| GET | SNAP | `/v1/model/builtin` | 内置模型、图片能力、倍率，HMAC | [G-models] [H-models] |
| GET | BENEFIT | `/api/v1/gateway/config` | 福利模型目录，HMAC | [G-models] [P-server] [H-models] |
| POST | SNAP | `/api/v2/chat/completions` | 对话、工具、图片，HMAC | [G-client] [P-server] [H-chat] |
| GET | SNAP | `/api/v1/queue/status` | 会话排队状态，HMAC | [H-chat] |
| GET | SNAP | `/snap-manager/v1/statistics/plugin` | 套餐与积分余额，HMAC | [H-credits] |
| GET | SNAP | `/v1/ops/delivery` | 活动列表，HMAC | [G-checkin] [H-credits] |
| POST | SNAP | `/v1/ops/claim` | 领取活动积分，HMAC | [G-checkin] [H-credits] |
| POST | SNAP | `/v1/ops/confirm` | 确认活动领取，HMAC | [G-checkin] [H-credits] |
| GET | BENEFIT | `/api/v1/benefit/claim` | 最近领取状态，HMAC | [P-benefit] |
| POST | BENEFIT | `/api/v1/benefit/claim` | 领取免费模型 Token 额度，HMAC | [G-models] [P-benefit] [H-credits] |
| GET | BENEFIT | `/api/v1/user/tokens/balance` | Token 福利余额，HMAC | [P-benefit] |

另有 G 的 `GET SNAP/v1/current/user` 实现，但它与同仓历史记录的 `/snap-manager/v1/current/user` 冲突，不能当作已确认可用接口；详见第 10 节。

浏览器入口另列：`GET PORTAL/portal/authorize`、`GET PORTAL/portal/login`，以及旧流程的 `GET https://auth.huaweicloud.com/authui/login.html`、`GET https://devcloud.cn-north-4.huaweicloud.com/doer/redirect`。它们是交互登录页面，不返回普通业务 API JSON。[H-login]

## 3. 登录与凭据

### 3.1 两种凭据来源

| 方式 | 已有实现 | 调用材料 | 续期方式 |
| --- | --- | --- | --- |
| 永久 AK/SK | P | IAM 访问密钥 AK、SK | P 未实现换取或续期 |
| OAuth 临时凭据 | G、H | 临时 AK、SK、security_token、到期时间 | refresh_token + 原 PKCE verifier + 原 DPoP 私钥 |

P 从环境变量读取 `CODEARTS_AK` / `CODEARTS_SK`，不实现 OAuth/PKCE/DPoP。G、H 的云端业务接口使用临时凭据签名，不能把 AK 或 refresh_token 直接作为 OpenAI Bearer Key。[P-server] [P-benefit] [G-client] [H-oauth]

### 3.2 新式 OAuth 授权入口

```http
GET https://codearts.huaweicloud.com/portal/authorize
```

| Query | 源码发送值 |
| --- | --- |
| `theme` | `2` |
| `locale` | `zh-cn` |
| `uri_scheme`、`client_id` | 均为 `codearts-agent` |
| `port` | 本地回调监听端口 |
| `code_challenge` | `BASE64URL(SHA256(code_verifier))`，不补 `=` |
| `code_challenge_method` | 字面量 `SHA-256` |
| `ticket_id` | 客户端生成的随机关联标识 |
| `plugin-name` | `snap_AIIDE` |
| `plugin-version` | `5.2.0` |

算法与 PKCE S256 对应，但门户参数的**字面值是 `SHA-256`**。H 的维护者记录称错用 `S256` 或额外添加 `auth_callback_url` 可能回退旧 ticket 流程。G 用随机 hex ticket，H 用 UUID；这是客户端选择。[G-client] `BuildAuthorizeURL` L222；[H-login] `buildOAuthLoginUrl` L544。

本地回调为 `http://127.0.0.1:<port>/oauth/callback`，收到 `code` 后执行下一节；本地回调不是华为云 API。G 兼容 GET query 和 POST form；H 当前回调读取 URL query。H 依据维护者记录将监听端口选为至少 10000，避免门户拒绝低端口。门户也可能先回调 `secret + redirect`，此时由客户端继续跳转或走 ticket 分支；应使用门户下发的 secret。[G-login] `serveCallback` L167；[H-login] `startOAuthCallbackServer` L556、端口选择 L620–640。

H 回调完成后跳转结果页：

```text
https://codearts.huaweicloud.com/portal/login?login_succeed=true&uri_scheme=codearts-agent&locale=zh-cn
```

失败时 `login_succeed=false`。该页面不负责换取凭据。[H-login] `buildPortalLoginResultUrl` L531。

### 3.3 授权码换取及刷新

```http
POST https://sts.cn-north-4.myhuaweicloud.com/v1/oauth2/tokens
Content-Type: application/x-www-form-urlencoded
DPoP: <本次请求的 ES256 JWS>
```

请求体为 URL 编码表单，不是 JSON，也不使用 AK/SK HMAC。

| 字段 | 授权码换取 | 刷新 |
| --- | --- | --- |
| `client_id` | `codearts-agent` | 原 client_id |
| `grant_type` | `authorization_code` | `refresh_token` |
| `code` | 授权码 | 不发送 |
| `code_verifier` | 原始 PKCE verifier | 原始 PKCE verifier |
| `redirect_uri` | `http://127.0.0.1:<port>/oauth/callback` | 不发送 |
| `refresh_token` | 不发送 | 最新 refresh_token |

响应读取字段：

```json
{
  "credentials": {
    "access_key_id": "<TEMP_AK>",
    "secret_access_key": "<TEMP_SK>",
    "security_token": "<SECURITY_TOKEN>",
    "expiration": "<ISO8601>"
  },
  "refresh_token": "<NEW_REFRESH_TOKEN>"
}
```

G 还读取顶层 `user_id`、`user_name`、`domain_id`。来源：[G-client] `ExchangeCode` L243、`RefreshToken` L261、`requestToken` L291；[H-oauth] `requestToken` L157、`exchangeAuthorizationCode` L229、`exchangeRefreshToken` L246。

DPoP protected header 与 payload：

```json
{
  "alg": "ES256",
  "typ": "dpop+jwt",
  "jwk": {"kty": "EC", "crv": "P-256", "x": "<PUBLIC_X>", "y": "<PUBLIC_Y>"}
}
```

```json
{
  "htm": "POST",
  "htu": "https://sts.cn-north-4.myhuaweicloud.com/v1/oauth2/tokens",
  "iat": 0,
  "jti": "<NEW_RANDOM_ID>"
}
```

`iat` 实际取当前 Unix 秒，示例的 0 仅表示数值类型。ES256 使用 P-256，JOSE 签名为 32 字节 `r` 与 32 字节 `s` 拼接后 base64url 编码；不是 DER。每次生成新的 proof，沿用原密钥；发送的公钥 JWK 不含私钥 `d`。[G-dpop] `signDpopProof` L96；[H-oauth] `signDpopJws` L74。

需持久化最新 refresh_token、原 client_id、原 code_verifier 和原 DPoP 私钥，以及当前临时凭据。历史记录显示 refresh_token 会一次性轮换；同一凭据的刷新应串行，成功后保存新值。H 记录过 access 约 2 小时、refresh 约 30 天且刷新不延长最终期限，这些是历史观察，不是固定有效期保证；实际应读取到期字段。[G-history] §3；[H-oauth] `refreshTokenExpiresAt` L145；[H-service] `renewCredential` L377。

错误体使用 `error` / `error_code` / `error_msg`。明确 token 已过期、已消费或 client_id 不匹配时，应修复凭据材料或重新登录。`InvalidDPoPHeader` 的处理存在版本差异：G 将其归入续期终态；H 保留为请求级失败，避免时钟偏差、proof 重放或网关异常导致误判。不能仅凭该字符串证明 refresh_token 永久失效。[G-client] `requestToken` L309；[H-oauth] L191。

### 3.4 旧 ticket 登录

H 构造旧浏览器页面：

```text
https://auth.huaweicloud.com/authui/login.html?service=<URL编码的redirectUrl>

redirectUrl =
https://devcloud.cn-north-4.huaweicloud.com/doer/redirect
  ?IdeaType=jetbrains
  &auth_callback_url=<URL编码的http://127.0.0.1:<port>/authentication>
  &plugin-name=snap_jetbrains
  &plugin-version=26.3.3
  &ticket_id=<TICKET_ID>
```

收到回调 secret 后轮询：

```http
GET https://snap-access.cn-north-4.myhuaweicloud.com/snap-manager/v1/login/ticket?ticket_id=<TICKET_ID>&secret=<PORTAL_SECRET>
plugin-name: <本次登录使用的插件名>
plugin-version: <本次登录使用的插件版本>
```

旧流程用 `snap_jetbrains / 26.3.3`；新 portal 的 fallback 用 `snap_AIIDE / 5.2.0`。H 还发送 `Content-Type: application/json;charset=UTF-8`。未见该请求使用 AK/SK 签名。兼容响应：

```json
{
  "credential": {
    "access": "<AK>", "secret": "<SK>",
    "securitytoken": "<TOKEN>", "expires_at": "<ISO8601>"
  },
  "user_id": "<USER>", "user_name": "<NAME>", "domain_id": "<DOMAIN>"
}
```

H 另兼容 `result.{accessKeyId,secretAccessKey,securityToken,expiration}` 及部分大小写变体。此分支未提供完整 refresh_token/DPoP 续期材料，不能据此承诺静默续期。轮询间隔和次数是各客户端策略，未提取到服务端 SLA。[G-client] `PollTicket` L348；[H-login] `parseCredentialResponse` L36、`pollForCredential` L80。

## 4. AK/SK 请求签名

### 4.1 公共算法

业务 API 使用 `SDK-HMAC-SHA256`。先确定要实际发送的 body bytes，再对同一字节串哈希和传输。

```text
CanonicalRequest =
  METHOD + LF + CanonicalURI + LF + CanonicalQuery + LF
  + CanonicalHeaders + LF + SignedHeaders + LF + SHA256HEX(body_bytes)

StringToSign =
  "SDK-HMAC-SHA256" + LF + XSdkDate + LF + SHA256HEX(CanonicalRequest)

Signature = HEX(HMAC-SHA256(UTF8(SK), UTF8(StringToSign)))

Authorization: SDK-HMAC-SHA256 Access=<AK>, SignedHeaders=<头名列表>, Signature=<hex>
```

- `X-Sdk-Date` 为 UTC `YYYYMMDDTHHMMSSZ`。
- CanonicalURI 在签名侧补尾斜杠，例如实际路径 `/api/v2/chat/completions` 对应签名路径 `/api/v2/chat/completions/`。
- CanonicalQuery 按参数名、值排序后规范编码；不要直接使用未排序的 URL 查询串。例：wire `offset=0&limit=100` 对应 canonical `limit=100&offset=0`。
- 待签名头名采用小写并排序，规范头每行 `name:value\n`；`CanonicalHeaders` 已有末尾换行，之后再加 LF，即头块与 SignedHeaders 间有空行。
- `SignedHeaders` 用 `;` 连接。SK 直接作 HMAC key；这里没有 region/service 多级密钥派生。
- GET 按零字节 body 签名；POST 的空字节和 `{}` 哈希不同。不要在签名后重新格式化 JSON。
- G/P/H 对 URI 与特殊 query 字符的转义实现并不完全相同。H 会拒绝可能无法稳定往返的 query 编码。本文已出现的字母数字/数字分页示例可直接按对应 signer 复现，特殊字符不要自行混搭三个实现。

来源：[G-sign] `signRequest` L31；[P-server] `_signed_request` L73、`_hmac_headers` L119；[P-benefit] `signed_request` L46；[H-sign] `canonicalQueryOf` L127、`signRequestHuawei` L189。

### 4.2 逐端点签名头差异

| 场景 | G | P | H |
| --- | --- | --- | --- |
| 基础签名头 | 签名时全部已设置的请求头，含 host/date/body hash，临时凭据含 security token | 通常 `content-type;host;x-sdk-date` | `host;x-sdk-content-sha256;x-sdk-date;x-security-token`；非 GET 加 content-type |
| AgentCenter | `Agent-Type: AgentCenter`、`X-Language: zh-cn` 入签 | 两头及 `accept: application/json` 入签 | 小写 `agent-type: AgentCenter`、`x-language: zh-cn` 入签 |
| builtin | `Agent-Type: PromptCenter`、`X-Language: zh-cn` 入签 | 无此实现 | 两头及 GET Content-Type 在签名后追加 |
| 套餐/ops 积分 | ops 两路由头入签 | 无此实现 | `PromptCenter` / `zh-cn` 两路由头在签名后追加 |
| 福利模型聊天 | `maas_type: benefit` 入签 | `maas_type: benefit`、`model-id`、`model-name` 均入签 | `maas_type: benefit` 入签 |
| 普通聊天附加头 | 还发送并签 `x-auth-token`、`app-id` 等；Chat-Id/Session-Id 签后追加 | 基础 HMAC；不发 x-auth-token | Chat-Id/Session-Id/lang 签后追加；不发 x-auth-token |
| BENEFIT 目录 | Content-Type、X-Language 参与签名，临时凭据带 security token | 基础 HMAC | 基础 HMAC，无额外路由头 |
| BENEFIT 领取 | Content-Type、X-Language 参与签名，临时凭据带 security token | 基础 HMAC | X-Language 在签名后追加，无 PromptCenter |
| queue/status | 无此实现 | 无此实现 | INFERHUB_AGENT、X-Language=en、traceid 在签名后追加 |

这张表记录实现差异，不代表服务端只允许其中一种签名集合。尤其 **PromptCenter 的入签方式在 G 与 H 间尚未统一**；H 保存了将附加头入签后验签失败的历史记录。不能把“全部 HTTP 头都必须参与签名”当成统一规则，也不能把全部 Agent-Type 都排除。HTTP 头名本身大小写不敏感，复用具体 signer 时仍须遵循其规范化实现。[G-client] [G-models] [G-checkin] [H-models] L523、L733；[H-credits] L130、`signedSnapRequest` L371。

## 5. Agent 与模型目录

### 5.1 用户 Agent 列表

```http
GET SNAP/v1/agent-center/agents/useragents?offset=0&limit=100
agent-type: AgentCenter
x-language: zh-cn
```

无 body，HMAC；两路由头参与签名。G 额外发送 `is_primary_agent=true`。这些客户端固定请求首批，不能从代码推导分页最大值。

```json
{
  "agents": [{
    "agent_id": "<AGENT_ID>",
    "agent_name": "CodeAgent",
    "is_primary_agent": true,
    "agent_order": 1,
    "show_in_ide": true,
    "alias": {"alias_zh_cn": "智能体"}
  }]
}
```

实际关联字段优先使用 `agent_id`；H 对 `id` 只是兼容。选择哪个 Agent 是客户端策略：G 优先可见 CodeAgent；H 优先 primary；P 按 primary/agent_order 尝试详情。[G-models] `defaultAgentID` L382；[P-server] `_refresh_models` L234；[H-models] `fetchAgentCenterWindows` L510。

### 5.2 Agent 详情与模型参数

```http
GET SNAP/v1/agent-center/agents/detail?agent_id=<URL编码的AGENT_ID>
agent-type: AgentCenter
x-language: zh-cn
```

无 body，HMAC，同上。实现读取 `gpts.models[]`：

```json
{
  "gpts": {
    "models": [{
      "model_alias": "<MODEL_ALIAS>",
      "model_name": "<MODEL_NAME>",
      "model_parameters": {
        "context_window": 1048576,
        "max_tokens": 65536,
        "supports_images": false,
        "enable_queue": true
      }
    }]
  }
}
```

数值仅示意，按实际响应读取。H 的后续观测记录表明条目通常没有 `model_id`，而有 `model_alias/model_name`。G/P 兼容读取 model_id，但候选顺序不同；使用模型标识时要结合具体返回字段，保持大小写，不能假定一定存在 model_id。详情里的 `context_window` 可补充 builtin 缺失的窗口信息；`max_tokens` 是容量元数据，不应直接等同每次请求的默认输出预算。[G-models] `fetchAgentModels` L332；[P-server] L285；[H-models] L559–638。

### 5.3 内置模型

```http
GET SNAP/v1/model/builtin
Agent-Type: PromptCenter
X-Language: zh-cn
```

无 body，HMAC，签名范围见矩阵。响应顶层为 `builtinModels`：

```json
{
  "builtinModels": [{
    "model_id": "<MODEL_ID>",
    "model_name": "<NAME>",
    "model_desc": "<DESCRIPTION>",
    "supports_images": false,
    "enable": true,
    "display_enabled": true
  }]
}
```

H 还读取 `credit`、`intervals`、`time_slots` 等倍率与时段字段；`ratio_display` 用于显示倍率，不能直接拿另一个 `ratio` 字段替代。历史响应未下发上下文窗口；G 虽定义了 `context_window/max_tokens` 接收字段，不证明云端实际返回它们。模型目录、倍率及可用时段可能随账号和时间变化。[G-models] `fetchBuiltinModels` L418；[H-models] `parseModelInfo` L223、builtin 请求 L733；[H-notes] AgentCenter 章节。

### 5.4 福利模型目录

```http
GET BENEFIT/api/v1/gateway/config
```

无 query/body，HMAC，无 Agent-Type：

```json
{
  "error_code": "0000",
  "result": {
    "base_url": "<GATEWAY_BASE_URL>",
    "models": [{
      "model_id": "<BENEFIT_MODEL_ID>",
      "model_name": "<NAME>",
      "context_window": 1048576,
      "max_tokens": 65536
    }]
  }
}
```

`base_url` 是 G 读取的字段；当前三套聊天实现均使用 SNAP 的 `/api/v2/chat/completions`。成功业务码为字符串 `"0000"`。H 记录部分账号返回 `4000 invalid channel, group not found`，而同账号 builtin 仍成功；不要仅凭此码把全部凭据标记失效。[G-models] `fetchBenefitModels` L513；[P-server] `_refresh_benefit_models` L200；[H-models] L705；[H-notes]。

福利通道身份应按账号和动态目录记录。P 的静态兜底、G 的全局模型别名以及 H 的目录展示策略均属于客户端选择，不能当作全账号永久可用清单。

## 6. 对话、工具与图片

### 6.1 请求

```http
POST https://snap-access.cn-north-4.myhuaweicloud.com/api/v2/chat/completions
Content-Type: application/json
Authorization: SDK-HMAC-SHA256 Access=<AK>, SignedHeaders=<实际签名头列表>, Signature=<计算值>
```

鉴权按第 4 节实现选择；临时凭据携带 `X-Security-Token`。当前 G 另带 `x-auth-token: <security_token>`、`Accept: text/event-stream`、`x-snap-traceid`、`app-id: CodeAgent3.0`、`is_confidential: false`、`X-Language: zh-cn`，H/P 未要求这些额外归属头。不要把 G 发送了某个头误写成三仓共同证明的必填要求。[G-client] `ChatHeadersV2` L442、`sendChatV2` L553；[P-server] `chat_completions` L537；[H-chat] L2323。

最小结构示意：

```json
{
  "model": "<EXACT_REMOTE_MODEL_ID>",
  "messages": [{"role": "user", "content": "你好"}],
  "stream": true
}
```

源码发送或透传的扩展字段：

| 字段 | 含义 / 已有实现 |
| --- | --- |
| `messages` | OpenAI 风格角色消息；历史内容需随请求发送 |
| `stream` | 是否请求流式；实现仍兼容服务端返回类型与该值不一致 |
| `stream_options.include_usage` | H 当前设为 true，请求 usage 帧 |
| `enable_thinking` | H 当前发送 boolean；维护者记录可双向控制思考 |
| `reasoning_effort` | G 可透传；不证明每模型均接受或生效 |
| `max_tokens`、`temperature`、`top_p` | 生成参数，具体限制按模型 |
| `tools`、`tool_choice` | 工具 schema 和选择策略 |
| `tool_stream` | G/H 设为 true，请求工具参数流式输出 |
| `prompt_cache_key` | G/H 设置的会话缓存关联标识 |
| `chat_id` | G 在非空时发送的关联字段 |
| `user_prompt` | H 发送最后一条真实用户消息的纯文本；未知时省略，服务端用途未确证 |

H 当前典型请求补充如下；它是**客户端默认配置，不是所有模型的服务端必填集或统一上限**：

```json
{
  "model": "<MODEL_ID>",
  "messages": [{"role": "user", "content": "<PROMPT>"}],
  "stream": true,
  "stream_options": {"include_usage": true},
  "enable_thinking": true,
  "temperature": 0,
  "tool_stream": true,
  "max_tokens": 32000,
  "prompt_cache_key": "<SESSION_ID>",
  "user_prompt": "<PROMPT>"
}
```

H 默认输出预算 32000、客户端钳制上限 65536，不能据此推导所有模型的最大输出。`Chat-Id/Session-Id` 用于会话关联，不代替 messages 历史；H 默认使用去连字符的 UUID，即 32 位十六进制，也允许调用方传入；G 使用自己的稳定派生算法。H 维护者记录旧 `thinking: {type:"disabled"}` 也可关闭思考，当前改发 `enable_thinking`。[G-client] `chatBodyV2` L476；[H-chat] L1449–1450、L2096–2207。

### 6.2 福利模型路由

三仓使用同一个聊天 URL。根据当前账号的福利目录决定是否加 `maas_type: benefit`，且该头参与签名。P 还发送并签名 `model-id: <body.model>`、`model-name: <body.model>`；G/H 没有这两个额外头。

H 对“无福利头且模型未注册”和“带福利头但 benefit not found”分别有限尝试补头/去头；仅在已有福利身份证据时尝试领取额度。它是客户端自愈策略，不是可以无限重复领取/重放的 API 契约。[P-server] `_hmac_headers` L119；[G-client] L541；[H-chat] L2280–2441。

### 6.3 工具调用

```json
{
  "tools": [{
    "type": "function",
    "function": {
      "name": "get_weather",
      "description": "查询天气",
      "parameters": {
        "type": "object",
        "properties": {"city": {"type": "string"}},
        "required": ["city"]
      }
    }
  }],
  "tool_choice": "auto"
}
```

assistant 工具调用使用 `tool_calls[].{id,type,function:{name,arguments}}`，其中 `arguments` 是 JSON 字符串；工具结果使用 `role:"tool"` 和 `tool_call_id`。G 维护者记录 tool_choice 可用字符串 `auto/none/required`，指定函数的对象形式曾被拒绝；G 代理采用缩减工具列表后发送 required 的兼容处理。H 对部分 DeepSeek V4 改用 DSML 提示协议，这是客户端适配，不是云端唯一工具格式。[G-client] L375–414、L510；[G-tools] `nativeToolChoice` L1028；[H-chat] DSML 注释及请求构造。

### 6.4 图片输入

仍调用同一聊天端点，没有提取到独立上传 API：

```json
{
  "model": "<SUPPORTS_IMAGES_MODEL_ID>",
  "messages": [{
    "role": "user",
    "content": [
      {"type": "text", "text": "描述图片"},
      {"type": "image_url", "image_url": {"url": "data:image/png;base64,<BASE64>"}}
    ]
  }],
  "stream": true
}
```

`image_url` 的值是含 url 的对象。H 的历史比较测试中，直接用字符串、Anthropic image/source、Responses input_image 均失败；可用输入是上述 data URL 形式。本次未确认远程 HTTP 图片 URL 能否直传。模型图片能力读 builtin/AgentCenter 的 `supports_images`；文本模型即使格式正确也可能返回非多模态错误。[H-chat] `codeartsImagePart` L682。

H 的 `analyzeImage` 是本地工具，内部另发一次 VL 模型聊天；它不是另一条华为云 API。该工具最多 8 图是本地限制，不是已确认的上游硬限制。[H-vision] [H-analyze]

### 6.5 响应与 SSE

普通响应/流式 chunk 使用 OpenAI 风格 `choices`。流式结构示意：

```text
data: {"id":"<ID>","model":"<MODEL_ID>","choices":[{"index":0,"delta":{"reasoning_content":"<THINKING>"},"finish_reason":null}]}

data: {"choices":[{"index":0,"delta":{"content":"<TEXT>"},"finish_reason":null}]}

data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120}}

data: [DONE]
```

实现还消费：

- `choices[].delta.tool_calls[]`，按 index 拼接函数名与 arguments 增量。
- `finish_reason` 的 stop / tool_calls / length 等值；P 对 other/缺失结束原因做本地归一。
- `usage.prompt_tokens_details.cached_tokens`，兼容 `prompt_cache_hit_tokens`；H 另读取 cache_write_tokens、completion_tokens_details.reasoning_tokens。
- 旧网关顶层 delta、累计 text/output：累计全文应用替换，不能与标准 delta 一样直接追加。

不要要求每个 data 行之间一定有空行，G 的解析器兼容逐行读取。也不要把收到 `[DONE]` 或 HTTP 200 本身当作业务成功：数据帧可能带 `error_code/error_msg`。没有可信终态的断流，应保留为未完成，不能无条件补成成功。[G-sse] `scanLine` L50、`applyEvent` L209；[P-server] `_parse_sse_bytes` L359、流转发 L606；[H-chat] `consumeSse` L2950。

## 7. 排队状态

```http
GET SNAP/api/v1/queue/status?model=<URL编码的模型>&task_id=<Session-Id>
Agent-Type: INFERHUB_AGENT
X-Language: en
x-snap-traceid: <UUID>
```

GET 无 body，HMAC；以上三个附加头在 H 中签名后追加，GET 不发送 Content-Type。

```json
{"status": "waiting", "queue_position": 2, "message": "<MESSAGE>"}
```

代码识别 `waiting / working / error / queue_full`；不能由字段名推导队列位置一定从 0 或 1 开始。H 在并发/TPM错误后每 10 秒重发 chat，最多 180 轮，并查询此接口辅助展示/终止判断；它不要求一定等到 working 才重发。间隔和次数是 H 的策略，不是服务端保证。[H-chat] `QUEUE_STATUS_BASE` L745、`queryQueueStatus` L3390。

## 8. 套餐与活动积分

本节是 SNAP 积分账本；第 9 节是 BENEFIT 免费模型 Token 账本，两者接口、请求体和成功码均不同。

### 8.1 账户套餐及积分余额

```http
GET SNAP/snap-manager/v1/statistics/plugin
Agent-Type: PromptCenter
X-Language: zh-cn
```

无 query/body，HMAC；H 在签名后追加路由头。响应为**裸对象**，不要强制先取 data：

```json
{
  "package": {
    "is_credit_package": true,
    "is_token_package": false,
    "spec_code": "<SPEC>",
    "package_name_cn": "<NAME>",
    "package_name_en": "<NAME>",
    "status": "<STATUS>"
  },
  "end_date": "<YYYY-MM-DD>",
  "metrics": [{
    "name": "usageTotalPackageCredit",
    "package_credit_amount": 1000,
    "package_credit_used": 200,
    "package_credit_remain": 800,
    "package_credit_expiring_amount": 100
  }]
}
```

数值为示意。已识别 metric：

| name | 含义 |
| --- | --- |
| `usageTotalPackageCredit` | 总额 |
| `usageBasicPackageCredit` | 基础包 |
| `usageOnDemandPackageCredit` | 按需包 |
| `usageBonusPackageCredit` | 赠送包 |

总额与分类是同一批积分的两种视图，不能相加。H 优先读总额 remain，缺总额才累加分类；没有 credit metric 表示没有该口径，不等于余额 0。`end_date` 只能作为已知套餐周期信息，不可套成每笔赠送额度的到期时间；expiring_amount 也不提供逐笔日期或可靠的未来 N 天区间定义。[H-credits] `parseCreditBalance` L454、`fetchCodeArtsAccountInfoDetailed` L622。

### 8.2 活动发现

```http
GET SNAP/v1/ops/delivery?channel=IDE
Agent-Type: PromptCenter
X-Language: zh-cn
```

无 body，HMAC；G/H 对附加头入签方式见矩阵。响应：

```json
{
  "code": 0,
  "message": "<MESSAGE>",
  "data": {
    "items": [{
      "campaignId": 1,
      "type": "USER_LOGIN",
      "title": "<TITLE>",
      "claimable": true,
      "status": "ELIGIBLE",
      "benefitUnit": "CREDIT",
      "benefitAmount": 1000,
      "pendingTotalAmount": 0,
      "extra": {"startTime": "<ISO8601>", "endTime": "<ISO8601>"}
    }]
  }
}
```

状态、金额均示意，不是固定规则。历史 campaignId 可能为数字，H 解析后转字符串。H 按 `type=USER_LOGIN` 找每日活动；G 当前选择 ID 1/4，属于历史活动选择策略。是否能领以本账号 delivery 返回的 claimable/状态为准，不应仅因套餐标志为 false 就忽略明确可领活动。[G-checkin] `FetchCreditCampaigns` L100；[H-credits] `parseActivity` L650、`fetchCodeArtsOpsActivitiesDetailed` L694。

### 8.3 领取与确认

```http
POST SNAP/v1/ops/claim
Content-Type: application/json
Agent-Type: PromptCenter
X-Language: zh-cn
```

G 请求体：

```json
{"campaignId": 1, "channel": "IDE", "idempotentKey": "claim_1_<UNIX_MILLISECONDS>"}
```

H 请求体：

```json
{"campaignId": "1", "channel": "IDE"}
```

均 HMAC。活动 ID 来自 delivery；H 转字符串，G 保留 ID。两种 body 是实现差异，不能据此断言 idempotentKey 为服务端必填。成功 envelope 为 `code:0`；H 读取 data.id 和可能的 benefitAmount 等金额字段。[G-checkin] `claimCreditCampaign` L148；[H-credits] `claimCodeArtsDailyCheckin` L749。

确认请求：

```http
POST SNAP/v1/ops/confirm
Content-Type: application/json
Agent-Type: PromptCenter
X-Language: zh-cn
```

```json
{"campaignId": "<来自delivery的活动ID>"}
```

H 在 claim.data.id 非 null/undefined 时补确认；发送的仍是 campaignId，不是 benefit id。G 在 claim 后确认，并能对 CLAIMED 状态只补 confirm。已领取待确认和完整成功应区分；H 忽略确认失败仍报告 claimed，是客户端返回策略，不能用来证明额度已完整到账。[G-checkin] L172–199；[H-credits] L803–815。

G 的状态流程是：`ELIGIBLE && claimable=true` 才 claim → confirm；`CLAIMED` 只 confirm；`CONFIRMED/CONSUMED` 跳过。活动未开始或已结束也跳过。[G-checkin] L148–175。

ops 的成功判据是业务数值 `code=0`；HTTP 200 不够。H 将 `40001`（维护者记录“尚未到达权益刷新时间，暂时无法领取”）处理为本次已领。不同活动不要硬编码金额或刷新时间。[H-credits] `unwrapSnapEnvelope` L347、已领取分支 L793。

## 9. 免费模型 Token 福利

### 9.1 最近领取状态

```http
GET BENEFIT/api/v1/benefit/claim
```

无 body，P 使用永久 AK/SK HMAC。读取 `result.create_time`，按 Unix 毫秒时间戳显示最近一次领取时间；未提取到每日刷新时区、冷却时长或次数边界。[P-benefit] `claim_status` L93、`_fmt_time` L113。

### 9.2 领取额度

```http
POST BENEFIT/api/v1/benefit/claim
Content-Type: application/json
```

三套实现都签名，但 body 有差异：

| 实现 | 实际 body | 附加头 |
| --- | --- | --- |
| G | UTF-8 `{}` | X-Language 入签，临时凭据带 security token |
| H | UTF-8 `{}` | X-Language 签名后追加，临时凭据带 security token |
| P benefit.py | 零字节 | 基础 HMAC，无临时 security token |

**body 与 payload hash 必须配套。** 本次未测试云端对两种 body 的接受范围，也未执行领取。[G-models] `ClaimBenefit` L476；[H-credits] `claimCodeArtsBenefit` L858；[P-benefit] `claim` L98。

成功 envelope：

```json
{"error_code": "0000", "result": {"create_time": 0}}
```

create_time 是 P 消费的字段，0 只表示数值类型。G/H 严格检查 `error_code === "0000"`；P 的通用解包器还宽松接受缺省或数字 0。不要用 SNAP ops 的 `code=0` 判据混判。本仓历史记录称可重复调用，但未提取出适用于所有情况的幂等/补偿承诺。[P-benefit] `_unwrap` L83；[G-models] [H-credits]

### 9.3 Token 余额

```http
GET BENEFIT/api/v1/user/tokens/balance
```

无 query/body，P 基础 HMAC。成功结果：

```json
{
  "error_code": "0000",
  "result": {"total_quota": 1000000, "total_balance": 800000, "used_amount": 200000}
}
```

分别是总额度、剩余额度、已用量。数值为示意；无法仅由这些字段确定各模型扣减倍率或计费单位。它们不是 SNAP 的 CREDIT 积分余额。[P-benefit] `balance` L103、`show_balance` L120。

## 10. 身份查询

### 10.1 STS caller identity

```http
GET https://sts.cn-north-4.myhuaweicloud.com/v5/caller-identity
```

无 query/body，AK/SK HMAC，临时凭据带 X-Security-Token。G 另签 Content-Type JSON、X-Language zh-cn：

```json
{"account_id": "<ACCOUNT_ID>", "principal_id": "<PRINCIPAL_ID>", "principal_urn": "<PRINCIPAL_URN>"}
```

G 从 principal_urn 的 `:user:` 尾部提取用户名。旧文档记录把主机改成 `iam.myhuaweicloud.com` 会报 API 不存在；不要因常量中出现 IAMHost 就替换域名。[G-client] `CallerIdentity` L726、`signedGet` L763；[G-history] §2。

### 10.2 CurrentUser：源码与历史记录冲突

| 证据 | 地址 |
| --- | --- |
| G 当前代码实际拼接 | `GET SNAP/v1/current/user` |
| G 函数注释及逆向文档 | `GET SNAP/snap-manager/v1/current/user` |

历史文档明确记录省略 snap-manager 会返回 `APIG.0101 The API does not exist`，但当前 `CurrentUser` 仍使用 SnapEngineApiHost + EpCurrentUser。H/P 没有提供独立交叉实现。

G 代码按 HMAC signedGet 调用，并读取 `user_id/user_name/domain_id`；历史文档另提 domain_name。**本次无法从静态证据确认有效路径，保留为待验证项**，不把任一路径包装成实测成功示例。[G-client] `CurrentUser` L746；[G-const] L41；[G-history] §2。

## 11. 错误处理

至少处理三层：HTTP 状态、JSON 业务 envelope、HTTP 200 内的 SSE 业务错误。保留上游原始错误码/消息，避免把所有失败都转成“额度不足”。

| 上游码 / 消息 | 代码或历史记录对应含义 | 接入注意 |
| --- | --- | --- |
| `APIG.0301` | 签名/鉴权失败，可能带 body hash mismatch 或 verify ak sk signature fail | 检查 body bytes、签名 query、头范围；只有明确可重试的子类才有限重签 |
| `APIG.0602`、HTTP 401/403 | 临时鉴权可能失效 | G 对 401 刷新一次；H 按认证分类处理 401/403；不能统一当额度耗尽 |
| `TM.00001001` | AgentCenter 路由头不正确的历史返回 | 核查 AgentCenter / zh-cn 及其签名 |
| `TM.00001041` | 并发会话上限 / 排队 | H 使用排队查询与有限重发 |
| `InferHub.ModelArts.81111.429` | TPM/限流 | 可能在 200 SSE 内出现 |
| `InferHub.002002009.404` | 模型未注册 | 核查模型标识及账号/福利通道 |
| `InferHub.4004.200` | benefit not found | 区分未领取和模型本来不走福利路由 |
| `InferHub.4291.200` | H 识别为该模型/账号福利额度耗尽 | 不要因名字含 429 就当普通限流反复重试 |
| `InferHub.001001005.400` | 参数不合法 | 曾见于 tool_choice 对象形式 |
| `InferHub.001001020.406` | 请求模型非多模态 | 改用声明 supports_images 的模型 |
| `InferHub.001001046` | 多模态内容格式不正确 | 检查 image_url 对象结构 |
| ops `code=40001` | 活动权益尚未刷新 | H 将其视作本次已领取 |
| opengw `error_code=4000` | invalid channel, group not found | 不能单独证明整个账号凭据失效 |
| STS `invalid_grant` / `ExpiredRefreshToken` / 明确 has been used | 刷新材料失效或已消费 | 保存轮转后的 token；必要时重新登录 |
| STS `InvalidDPoPHeader` | proof 被拒绝 | 检查原密钥、当前时间和新 proof，见第 3.3 节实现差异 |

上述码是提取到的枚举片段，不是完整错误目录；错误码后缀不一定等于本次 HTTP 状态。10 秒重试、180 次上限、401 刷新一次等均是客户端行为。[G-client] `embeddedSSEError` L619；[G-tools]；[H-chat] L920–1017、L2663；[H-oauth]；[H-credits]；[H-sign-test]

## 12. 旧接口及尚无完整实现的记录

以下保留用于查阅历史，不与当前原生 OpenAI 聊天接口混用。

| 方法（历史记录） | 地址 | 证据范围 |
| --- | --- | --- |
| POST | `SNAP/v1/chat/chat` | G 常量、旧逆向文档、旧 SSE 兼容；当前聊天客户端已用 /api/v2 |
| POST | `SNAP/v1/chat/start-chat` | G 常量与旧文档，无完整调用实现 |
| GET | `SNAP/v1/chat/agents` | 同上；不是 AgentCenter useragents |
| POST | `SNAP/v1/chat/management/event` | G 常量与旧文档，无完整请求/响应 |
| POST | `SNAP/v1/chat/record-request` | 同上 |
| GET | `SNAP/v1/benefit-gateway-config` | G 文档记载 PromptCenter、响应 enabled；当前无调用 |
| 未提取到 | `SNAP/v1/ops/credit/usage` | H 仅注释提及 credits/cacheHitRatio，称为单次消耗；未找到调用、方法及完整 Schema |

旧 `/v1/chat/chat` 记录的请求形状是：

```json
{
  "chat_id": "<32_HEX>",
  "client": "IDE",
  "task": "chat",
  "messages": [{"type": "text", "text": "<PROMPT>"}],
  "task_parameters": {"ide": "CodeArts Agent"},
  "batch_task_parameters": [],
  "attempt": 1,
  "not_allow_external_model": true,
  "user_id": "<USER_NAME>"
}
```

它的 messages 没有 role，不能发送给当前 `/api/v2/chat/completions`。旧文档对 x-auth-token 前后矛盾，后段记录改用 HMAC；不能把旧结论套到新路径。[G-const] L23；[G-history] §2、§4、§5；[H-credit-note] L375。

## 13. 本地代理接口边界与提取结论

四个项目出现的以下路径属于它们自己的兼容服务，不是华为云上游路径：

| 本地路径 | 用途 |
| --- | --- |
| `GET /v1/models`、`GET /v1/models/<id>` | 代理整理/缓存后的模型目录，具体支持视项目 |
| `POST /v1/chat/completions` | 代理入口，映射到 SNAP /api/v2/chat/completions |
| `POST /v1/responses` | N/H 兼容网关入口，不能直接拼到 SNAP 主机 |
| `POST /api/jet-hub` | N 的插件管理 RPC |
| `GET /admin`、`GET /healthz` | N 的管理页及健康检查 |
| `/oauth/callback`、`/authentication` | 客户端本地登录回调 |

N 的 `src/server.js` 将 /v1/* 透明转发给本机插件网关；管理 RPC 信封为 `{type:"client-request",rpcId,method:"jet-hub",payload:{method,payload}}`。这些协议不应当成华为云的鉴权或业务 body。[N-proxy] `proxyToGateway` L360；[N-contract] `rpcEnvelope` L59；[P-server] Flask 路由。

还需保留的证据边界：

- 旧 OAuth 文档出现 snap-manager token 地址、无 /portal 授权路径、S256 字面值；当前 G/H 实现是 STS token 地址、/portal/authorize、SHA-256。
- AgentCenter 的 agent_id/model_alias 实际使用与部分老注释的 id/model_id 不一致；builtin 声明了接收字段也不代表服务端实际返回窗口。
- 模型列表、静态种子、缓存 JSON、README 中的并发/绕过排队说明，均不能作为当前账号能力保证。
- H 的 `/v2/billing/meter/*` 是其它 provider 的接口；`/cag/global/config` 是本地 IDE 内核来源，均未列为华为云远端 API。
- 本次未执行登录、续期、签到、额度领取、聊天或 E2E。测试文件仅作结构和历史行为证据；没有把 mock 测试通过等同于云端实测。

## 14. 源码索引

下列链接均固定版本；正文给出关键符号和一基行号，便于在本地参考仓库中定位。

| 引用 | 文件 | 重点 |
| --- | --- | --- |
| N-package | [package.json][N-package] | 固定插件依赖 L29 |
| N-host | [src/host.js][N-host] | 插件装载 L63 |
| N-proxy | [src/server.js][N-proxy] | 本机网关反代 L360 |
| N-contract | [src/upstream.js][N-contract] | 管理 RPC 信封 L59 |
| G-const | [internal/upstream/const.go][G-const] | 主机、路径、client_id |
| G-client | [internal/upstream/client.go][G-client] | OAuth、聊天、身份 |
| G-login | [cmd/login/main.go][G-login] | 本地回调 |
| G-dpop | [internal/upstream/dpop.go][G-dpop] | ES256 proof |
| G-sign | [internal/upstream/signer.go][G-sign] | HMAC signer |
| G-models | [internal/upstream/models.go][G-models] | 三路模型发现、福利领取 |
| G-checkin | [internal/upstream/checkin.go][G-checkin] | 活动状态与领取/确认 |
| G-sse | [internal/upstream/sse.go][G-sse] | SSE 兼容解析 |
| G-tools | [internal/server/handler.go][G-tools] | 工具参数及重试适配 |
| G-history | [docs/reverse-engineering.md][G-history] | 历史逆向记录，不能替代现行实现 |
| P-server | [server.py][P-server] | 永久 AK/SK、模型、聊天 |
| P-benefit | [benefit.py][P-benefit] | 福利状态、空 body 领取、Token 余额 |
| H-login | [src/login.ts][H-login] | 新旧浏览器登录及 ticket |
| H-oauth | [src/oauth.ts][H-oauth] | PKCE、DPoP、凭据换取 |
| H-service | [src/service.ts][H-service] | 凭据续期与持久化编排 |
| H-sign | [src/sign.ts][H-sign] | 签名及 query 编码 |
| H-models | [src/models.ts][H-models] | 目录、窗口、头矩阵 |
| H-chat | [src/llm-adapter.ts][H-chat] | 聊天、SSE、图片、队列 |
| H-vision | [src/codearts-vision.ts][H-vision] | 图片能力及本地工具范围 |
| H-analyze | [src/analyze-image.ts][H-analyze] | 本地 analyzeImage 工具 |
| H-credits | [src/codearts-credits.ts][H-credits] | 套餐、积分与福利领取 |
| H-credit-note | [src/credits.ts][H-credit-note] | 信用消耗历史注释；主体属于其它 provider |
| H-notes | [docs/agent-notes/providers-codearts.md][H-notes] | 模型窗口和近期协议观测 |
| H-sign-test | [tests/unit/codearts-signature-retry.spec.ts][H-sign-test] | 区分签名故障子类 |

[N-package]: https://github.com/zhuweiyou/codearts2api/blob/a20d1145cfd2847caf537b904062d07b04a8aa3b/package.json#L29
[N-host]: https://github.com/zhuweiyou/codearts2api/blob/a20d1145cfd2847caf537b904062d07b04a8aa3b/src/host.js#L63
[N-proxy]: https://github.com/zhuweiyou/codearts2api/blob/a20d1145cfd2847caf537b904062d07b04a8aa3b/src/server.js#L360
[N-contract]: https://github.com/zhuweiyou/codearts2api/blob/a20d1145cfd2847caf537b904062d07b04a8aa3b/src/upstream.js#L59
[G-const]: https://github.com/HITZY2002/codearts2api/blob/8d1e43d6f31279ac20cf94b92eff20bdba784b76/internal/upstream/const.go
[G-client]: https://github.com/HITZY2002/codearts2api/blob/8d1e43d6f31279ac20cf94b92eff20bdba784b76/internal/upstream/client.go
[G-login]: https://github.com/HITZY2002/codearts2api/blob/8d1e43d6f31279ac20cf94b92eff20bdba784b76/cmd/login/main.go
[G-dpop]: https://github.com/HITZY2002/codearts2api/blob/8d1e43d6f31279ac20cf94b92eff20bdba784b76/internal/upstream/dpop.go
[G-sign]: https://github.com/HITZY2002/codearts2api/blob/8d1e43d6f31279ac20cf94b92eff20bdba784b76/internal/upstream/signer.go
[G-models]: https://github.com/HITZY2002/codearts2api/blob/8d1e43d6f31279ac20cf94b92eff20bdba784b76/internal/upstream/models.go
[G-checkin]: https://github.com/HITZY2002/codearts2api/blob/8d1e43d6f31279ac20cf94b92eff20bdba784b76/internal/upstream/checkin.go
[G-sse]: https://github.com/HITZY2002/codearts2api/blob/8d1e43d6f31279ac20cf94b92eff20bdba784b76/internal/upstream/sse.go
[G-tools]: https://github.com/HITZY2002/codearts2api/blob/8d1e43d6f31279ac20cf94b92eff20bdba784b76/internal/server/handler.go
[G-history]: https://github.com/HITZY2002/codearts2api/blob/8d1e43d6f31279ac20cf94b92eff20bdba784b76/docs/reverse-engineering.md
[P-server]: https://github.com/Johnsheng1/codearts2api/blob/d813ca0456f23cd73bb3bbee16627bc2a40036f9/server.py
[P-benefit]: https://github.com/Johnsheng1/codearts2api/blob/d813ca0456f23cd73bb3bbee16627bc2a40036f9/benefit.py
[H-login]: https://gitee.com/iJetLi/deepseek-harness-codearts/blob/2bf76ce66f1214f18ee3202238fb0f2d02cc791f/src/login.ts
[H-oauth]: https://gitee.com/iJetLi/deepseek-harness-codearts/blob/2bf76ce66f1214f18ee3202238fb0f2d02cc791f/src/oauth.ts
[H-service]: https://gitee.com/iJetLi/deepseek-harness-codearts/blob/2bf76ce66f1214f18ee3202238fb0f2d02cc791f/src/service.ts
[H-sign]: https://gitee.com/iJetLi/deepseek-harness-codearts/blob/2bf76ce66f1214f18ee3202238fb0f2d02cc791f/src/sign.ts
[H-models]: https://gitee.com/iJetLi/deepseek-harness-codearts/blob/2bf76ce66f1214f18ee3202238fb0f2d02cc791f/src/models.ts
[H-chat]: https://gitee.com/iJetLi/deepseek-harness-codearts/blob/2bf76ce66f1214f18ee3202238fb0f2d02cc791f/src/llm-adapter.ts
[H-vision]: https://gitee.com/iJetLi/deepseek-harness-codearts/blob/2bf76ce66f1214f18ee3202238fb0f2d02cc791f/src/codearts-vision.ts
[H-analyze]: https://gitee.com/iJetLi/deepseek-harness-codearts/blob/2bf76ce66f1214f18ee3202238fb0f2d02cc791f/src/analyze-image.ts
[H-credits]: https://gitee.com/iJetLi/deepseek-harness-codearts/blob/2bf76ce66f1214f18ee3202238fb0f2d02cc791f/src/codearts-credits.ts
[H-credit-note]: https://gitee.com/iJetLi/deepseek-harness-codearts/blob/2bf76ce66f1214f18ee3202238fb0f2d02cc791f/src/credits.ts
[H-notes]: https://gitee.com/iJetLi/deepseek-harness-codearts/blob/2bf76ce66f1214f18ee3202238fb0f2d02cc791f/docs/agent-notes/providers-codearts.md
[H-sign-test]: https://gitee.com/iJetLi/deepseek-harness-codearts/blob/2bf76ce66f1214f18ee3202238fb0f2d02cc791f/tests/unit/codearts-signature-retry.spec.ts
