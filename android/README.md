# CodeArts2API Panel — Android

把同一套 Go 网关跑在手机上的原生壳：手机自己就是一台 API 服务器，向局域网/公网
提供 OpenAI / Anthropic 兼容接口。

## 与 loomy2api 安卓版的区别

参考实现（`../loomy2api/android`）用 **Chaquopy 在 App 内嵌一个 CPython** 跑 Python
网关。本项目是 **纯 Go**，因此不用解释器：把仓库根目录的 Go 代码交叉编译成一个
arm64 可执行文件，塞进 APK 的 `jniLibs`，前台服务 exec 它即可。

**不需要 proot 容器**（那是 `JianYunServerPro` 那种「跑完整 Linux 发行版」的方案）：
网关是单个静态二进制，不依赖发行版文件树。

## 构建

前置：JDK 17、Android SDK 34、NDK（用于 cgo 的 DNS，见下）。`local.properties`
里写 `sdk.dir=...`，可选 `ndk.dir=...`。

```bash
cd android
JAVA_HOME=/path/to/jdk17 ./gradlew :app:assembleDebug
adb install -r app/build/outputs/apk/debug/app-debug.apk
```

Gradle 任务 `buildGoBinary` 会在 `preBuild` 前自动交叉编译网关到
`app/src/main/jniLibs/arm64-v8a/libcodearts-gw.so`（该产物 gitignore，每次重建）。

### 为什么必须用 NDK（cgo），不能 `CGO_ENABLED=0`

Android 没有 `/etc/resolv.conf`，而 Go 的纯 Go resolver 恰恰读它。于是
`CGO_ENABLED=0` 编出来的二进制一做出站请求就失败：

```
lookup codearts.cn on [::1]:53: read udp ...: connection refused
```

开 cgo 后 Go 改用 bionic 的 `getaddrinfo`，DNS 正常（黑鲨 SKW-A0 真机已实测：
`codearts.cn → 43.134.62.165`、HTTPS 200）。所以构建固定用
`CGO_ENABLED=1 GOOS=android GOARCH=arm64 CC=<ndk>/aarch64-linux-android29-clang`。

## 为什么二进制叫 `lib*.so` 放在 `jniLibs`

Android 10+（targetSdk 29+）只允许从 `nativeLibraryDir` 执行 App 内的文件，其它
目录受 W^X 限制、`execve` 直接被拒。而系统只会把 APK 里 `lib/<abi>/*.so` 解压到
`nativeLibraryDir`。因此网关被命名成 `libcodearts-gw.so`，并在 `packaging.jniLibs`
里设 `useLegacyPackaging = true`（否则 `.so` 直接从 APK 映射、不解压，
`nativeLibraryDir` 为空，`execve` 报 ENOENT）。`keepDebugSymbols` 防止 NDK strip
破坏 Go 的 build info。

## 运行结构

- `GatewayService`：前台服务，`ProcessBuilder` 在 `nativeLibraryDir` exec 网关，
  cwd 指向 `filesDir`（config.json / auths / data / gateway.log 都落在这里）。
  持有 `PARTIAL_WAKE_LOCK`（防息屏冻结）、30s 看门狗探 `/healthz`、`START_STICKY`
  与 `onTaskRemoved` 保证被划掉/杀掉后自恢复。
- `MainActivity`：WebView 加载网关自带的管理面板 `/panel`。面板已适配窄屏
  （`@media(max-width:760px)`），所以直接复用，不重写原生 UI。启动时从 config.json
  读出 admin_key 注入 `sessionStorage`，用户免登录。
- `GatewayRunner`：进程与配置管理（端口、密钥、日志脱敏）。
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

## 端口

默认 **17866**（与桌面网关 7866 错开，可同时运行）。可在配置里改，但 WebView 与
健康探测都读同一个 `GatewayRunner.port`，不要写死。

## 签名

`android/keystore.properties`（gitignore）缺失时 release 打 unsigned 包，便于本地
验证；要发布就补上 storeFile / storePassword / keyAlias / keyPassword。
