# CodeArts2API Panel — Android

把同一套 Go 网关跑在手机上的原生壳：手机自己就是一台 API 服务器，向局域网/公网
提供 OpenAI / Anthropic 兼容接口。

## 与 loomy2api 安卓版的区别

参考实现（`../loomy2api/android`）用 **Chaquopy 在 App 内嵌一个 CPython**，网关跑在
**App 进程内**。本项目网关是 **纯 Go**，但不把这当成「exec 个子进程就行」：和 loomy
一样，**网关必须跑在 App 进程内**，只是换了实现方式——把 Go 编成 c-shared 库，
`System.loadLibrary` 加载后经 JNI 调用（见下）。

**不需要 proot 容器**（那是 `JianYunServerPro` 那种「跑完整 Linux 发行版」的方案）：
网关是单个静态二进制，不依赖发行版文件树。

### 为什么不能 exec 子进程（踩过的坑）

最初的实现是 `ProcessBuilder` exec 一个 android/arm64 可执行文件。构建、运行、
出站请求全都正常，但**退到后台几十秒后端口就不再响应**。真机诊断（PJA110，
ColorOS/Android 16；同机对照 loomy 正常）结论：

- 该子进程被 Android 登记为 **PhantomProcessRecord**（`dumpsys activity processes` 可见）；
- App 一进入 cached 状态，整个 **UID 的 cgroup 被冻结**（`/sys/fs/cgroup/apps/uid_N/cgroup.freeze = 1`）；
- 此时 Go 进程还在（`S sleeping`）但一个字节都不读，连接挂起、curl 超时；
- **电池白名单、前台服务通知、PARTIAL_WAKE_LOCK 全都挡不住这层**——通知/前台服务只保护
  **发它的那个进程**，保护不到 App spawn 出来的子进程。

loomy 不受影响，因为它的 HTTP server 就跑在 App 进程内。因此我们改成同样的形态：
**c-shared 库 + JNI，网关在 App 进程内启动**（`mobile/jni.go` + `GatewayNative.kt`）。
实测修复后后台 4 分钟连续探测全部 200。

## 构建

前置：JDK 17、Android SDK 34、NDK（用于 cgo 的 DNS，见下）。`local.properties`
里写 `sdk.dir=...`，可选 `ndk.dir=...`。

```bash
cd android
JAVA_HOME=/path/to/jdk17 ./gradlew :app:assembleDebug
adb install -r app/build/outputs/apk/debug/app-debug.apk
```

Gradle 任务 `buildGoBinary` 会在 `preBuild` 前自动把仓库根目录的 `./mobile` 编成
`app/src/main/jniLibs/arm64-v8a/libcodeartsgw.so`（c-shared 库，gitignore，每次重建）。

### 为什么必须走 NDK（cgo）

c-shared 天然要 cgo；而且 DNS 也非它不可：Android 没有 `/etc/resolv.conf`，Go 的纯
resolver 恰恰读它，`CGO_ENABLED=0` 一做出站请求就失败：

```
lookup codearts.cn on [::1]:53: read udp ...: connection refused
```

开 cgo 后 Go 改用 bionic 的 `getaddrinfo`，DNS 正常（黑鲨 SKW-A0 真机已实测：
`codearts.cn → 43.134.62.165`、HTTPS 200）。构建固定用
`CGO_ENABLED=1 GOOS=android GOARCH=arm64 CC=<ndk>/aarch64-linux-android29-clang`。

### 为什么手写 JNI 而不用 gomobile bind

gomobile bind 支持 Go 1.26 但要求把 `golang.org/x/mobile` 写进 `go.mod`，而本项目
目前**零依赖**，不值得为一个入口引入一条要持续维护的依赖链。JNI 桥只有两个函数，
在 `mobile/jni.go` 里用 cgo 直接写即可（注意 `JNIEnv` 的成员是函数指针宏
`(*env)->GetStringUTFChars`，cgo 取不到，要包一层 C 函数转发）。

## 运行结构

- `GatewayService`：前台服务，调用 `GatewayRunner.ensureRunning` 在 **App 进程内**
  启动网关；持有 `PARTIAL_WAKE_LOCK`、30s 看门狗探 `/healthz`、`START_STICKY` 与
  `onTaskRemoved` 保证被划掉/杀掉后自恢复。
- `GatewayNative`：`System.loadLibrary("codeartsgw")` + `external fun start/stop`，
  对应 `mobile/jni.go` 的 JNI 导出。
- `GatewayRunner`：状态与配置管理（端口、密钥、config.json 播种）。网关的
  config/auths/data 都在 `filesDir` 下；以 config 的**绝对路径**启动，网关自己按该
  目录解析 `./auths`、`./data`（`internal/gateway.resolveDataPaths`），**不改 App
  进程的工作目录**。
- `MainActivity`：WebView 加载网关自带的管理面板 `/panel`。面板已适配窄屏
  （`@media(max-width:760px)`），所以直接复用，不重写原生 UI。启动时从 config.json
  读出 admin_key 注入 `sessionStorage`，用户免登录。
- `BootReceiver`：开机 / 升级后自启。

WebView 补了两块默认缺失、否则手机上面板不可用的能力：`onShowFileChooser`
（「导入 JSON」选文件）与 `shouldOverrideUrlLoading`（OAuth 授权页转系统浏览器）。

## 移动端兼容性

设备的系统 WebView 可能很旧（测试机黑鲨 SKW-A0 是 **Chrome 81**）。面板曾用到若干
高版本特性导致导航失效，已加回退：

| 特性 | 需要版本 | 回退 |
|---|---|---|
| `Object.hasOwn` | Chrome 93 | `Object.prototype.hasOwnProperty.call` |
| CSS `inset` | Chrome 87 | `top/right/bottom/left` |
| `100dvh` | Chrome 108 | 先写 `100vh` 再写 `dvh` 覆盖 |
| `color-mix()` | Chrome 111 | 先写回退色，再写 `color-mix` 覆盖 |
| `padding-inline` | Chrome 87 | `padding-left/right` |

改面板 CSS/JS 时留意别再引入只有新浏览器才认的写法，否则老 WebView 上会整块失效。

## ColorOS / MIUI 的额外一步：允许后台运行

跑在 App 进程内 + 前台服务仍然不够：ColorOS 实测会在退到后台约 20s 后冻结整个 UID
的 cgroup（`/sys/fs/cgroup/apps/uid_N/cgroup.freeze = 1`），此时 Go 进程还在但一字节
都不读，网关静默。**电池白名单、前台服务通知、WAKE_LOCK 都挡不住**——它们保护的是
「发通知的那个进程」，保护不到 ROM 层的冻结。

唯一有效的是系统的「允许后台运行」开关，而它**按设计无法由 App 运行时自授**（否则任何
恶意软件都能把自己设成永不冻结）。因此 `BackgroundGuide` 只做引导：ColorOS 上首次
启动弹一次对话框，跳转到 **设置 → 电池 → 应用耗电管理 → CodeArts2API → 完全允许后台行为**。

`BackgroundGuide.open()` 依次尝试 ColorOS 自带页（`com.oplus.battery/…PowerUsageModelActivity`）、
系统电池优化页、应用详情页；非 OPPO 系机型直接跳过、不弹窗。

> 实测：ColorOS 那个页面**第三方 App 打不开**——`am start` 报
> `Permission Denial: … requires com.oplus.permission.safe.SETTINGS`（只有 shell/root 能调）。
> 所以 App 只弹出说明文字（告诉用户点哪一项），实际落地页由系统电池优化页兜底；
> 上面的厂商 Activity 保留，是因为部分 ROM/版本确实放行，能直达就直达。

## 端口

默认 **17866**（与桌面网关 7866 错开，可同时运行）。可在配置里改，但 WebView 与
健康探测都读同一个 `GatewayRunner.port`，不要写死。

## 签名

`android/keystore.properties`（gitignore）缺失时 release 打 unsigned 包，便于本地
验证；要发布就补上 storeFile / storePassword / keyAlias / keyPassword。
