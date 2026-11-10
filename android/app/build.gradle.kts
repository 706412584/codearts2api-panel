import java.util.Properties

plugins {
    id("com.android.application")
    id("org.jetbrains.kotlin.android")
}

// Gradle 根 = 本 android/ 目录；仓库根是它的上一级（Go 模块所在处）。
val androidDir: File = rootProject.projectDir
val repoRoot: File = androidDir.parentFile

// 网关二进制版本号：android/version.txt → 注入 Go 的 server.Version。
val gatewayVersion = androidDir.resolve("version.txt")
    .takeIf { it.exists() }?.readText()?.trim()
    ?.takeIf { it.isNotEmpty() } ?: "dev"

android {
    namespace = "com.codearts.panel"
    compileSdk = 34

    defaultConfig {
        applicationId = "com.codearts.panel"
        minSdk = 26          // Android 8.0：前台服务 + nativeLibraryDir 里的 c-shared 库
        targetSdk = 34
        versionCode = 1
        versionName = "1.0.0"
        ndk {
            // 网关二进制与 proot 无关，只出 arm64；模拟器另测。
            abiFilters += listOf("arm64-v8a")
        }
    }

    // 正式签名凭据放 android/keystore.properties（gitignore），不入库。
    // 缺失时 release 打 unsigned 包（CI/验证用），而不是在 packageRelease 阶段报错。
    val keystoreProps = Properties().apply {
        val f = androidDir.resolve("keystore.properties")
        if (f.exists()) f.inputStream().use { load(it) }
    }
    val storeFileProp = keystoreProps.getProperty("storeFile")
    val hasReleaseSigning = storeFileProp != null && androidDir.resolve(storeFileProp).exists()

    signingConfigs {
        create("release") {
            if (hasReleaseSigning) {
                storeFile = androidDir.resolve(storeFileProp!!)
                storePassword = keystoreProps.getProperty("storePassword")
                keyAlias = keystoreProps.getProperty("keyAlias")
                keyPassword = keystoreProps.getProperty("keyPassword")
            }
        }
    }

    buildTypes {
        release {
            if (hasReleaseSigning) signingConfig = signingConfigs.getByName("release")
            isMinifyEnabled = false   // UI 仅 WebView，无反射/序列化需要 keep
            isShrinkResources = false
        }
    }

    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }
    kotlinOptions { jvmTarget = "17" }
    buildFeatures { buildConfig = true }

    packaging {
        resources.excludes += setOf("META-INF/*.kotlin_module")
        jniLibs {
            // 网关现在是正常的 JNI 库（System.loadLibrary 加载），不再需要从
            // nativeLibraryDir 执行、也不再需要 useLegacyPackaging。
            // NDK strip 会破坏 Go 二进制的 build info，保留符号。
            keepDebugSymbols += "**/libcodeartsgw.so"
        }
    }
}

// ---------------------------------------------------------------------------
// 交叉编译 Go 网关为 Android arm64 的 **c-shared 库**（跑在 App 进程内）。
//
// 为什么是库不是可执行文件：App 自己 exec 的子进程会被 Android 登记为 phantom
// process，App 一离开前台就整个 UID 被冻结，监听随之失效（真机实测：退后台约
// 30-60s 后端口不再响应）。跑在 App 进程内的 native 库才受前台服务保护。
//
// 必须走 NDK cgo（c-shared 天然要 cgo）：Android 没有 /etc/resolv.conf，纯 Go
// resolver 的 DNS 会失败。cgo 后 Go 用 bionic 的 getaddrinfo，出站解析正常。
// ---------------------------------------------------------------------------
val apiLevel = 29

fun ndkHostTag(): String =
    if (System.getProperty("os.name").lowercase().contains("win")) "windows-x86_64"
    else "linux-x86_64"

fun resolveClang(): String {
    val isWin = System.getProperty("os.name").lowercase().contains("win")
    val suffix = if (isWin) ".cmd" else ""
    val dirs = buildList {
        // 1) local.properties 的 ndk.dir 显式覆盖
        androidDir.resolve("local.properties").takeIf { it.exists() }?.let { p ->
            p.readLines().firstOrNull { it.trim().startsWith("ndk.dir=") }?.let {
                val raw = it.substringAfter("ndk.dir=").trim()
                    .replace("\\\\", "\\").replace("\\:", ":")
                add("$raw/toolchains/llvm/prebuilt/${ndkHostTag()}/bin")
            }
        }
        // 2) 环境变量
        System.getenv("ANDROID_NDK_HOME")?.let {
            add("$it/toolchains/llvm/prebuilt/${ndkHostTag()}/bin")
        }
        // 3) 本机 NDK 目录（按版本号降序取最新）
        listOf(File("D:/android/ndk"), File("${System.getenv("ANDROID_HOME") ?: ""}/ndk"))
            .filter { it.isDirectory }
            .forEach { root ->
                root.listFiles()?.sortedByDescending { it.name }?.forEach { v ->
                    add("${v.absolutePath}/toolchains/llvm/prebuilt/${ndkHostTag()}/bin")
                }
            }
    }
    for (dir in dirs) {
        val clang = File(dir, "aarch64-linux-android$apiLevel-clang$suffix")
        if (clang.isFile) return clang.absolutePath
    }
    throw GradleException(
        "找不到 NDK clang（aarch64-linux-android$apiLevel-clang）。" +
            "请在 android/local.properties 设置 ndk.dir，或安装 NDK。"
    )
}

val buildGoBinary = tasks.register<Exec>("buildGoBinary") {
    group = "build"
    description = "交叉编译 Go 网关为 Android arm64 的 c-shared 库"
    workingDir = repoRoot
    val outDir = androidDir.resolve("app/src/main/jniLibs/arm64-v8a")
    outputs.upToDateWhen { false }   // 网关源码小，每次构建刷新，避免陈旧库
    doFirst { outDir.mkdirs() }
    environment("CGO_ENABLED", "1")
    environment("GOOS", "android")
    environment("GOARCH", "arm64")
    environment("CC", resolveClang())
    environment("GOPROXY", "https://goproxy.cn,direct")
    commandLine(
        "go", "build",
        "-buildmode=c-shared",
        "-trimpath",
        "-ldflags", "-s -w -X codearts2api-panel/internal/server.Version=$gatewayVersion",
        "-o", "${outDir.absolutePath}/libcodeartsgw.so",
        "./mobile",
    )
}

tasks.matching { it.name.startsWith("preBuild") }.configureEach {
    dependsOn(buildGoBinary)
}

dependencies {
    implementation("androidx.core:core-ktx:1.13.1")
    implementation("androidx.activity:activity-ktx:1.9.3")
}
