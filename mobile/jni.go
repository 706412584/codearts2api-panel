// Package mobile 是 Android 版的入口。
//
// 编译方式：`go build -buildmode=c-shared`，产出一个真正的 .so，APK 用
// System.loadLibrary 加载后经 JNI 调用。网关因此跑在 **Android App 进程内**——
// 这正是关键：App 自己 spawn 的子进程（ProcessBuilder exec）会被 Android 登记为
// "phantom process"，App 一离开前台就被冻结，监听随之失效；只有跑在 App 自身进程
// 里的工作才受前台服务保护。（参考实现 loomy2api/android 靠 Chaquopy 把 CPython
// 跑在进程内白拿这一点，我们则用 c-shared 把 Go 跑在进程内。）
//
// 这里手写 JNI 桥而不是用 gomobile bind：gomobile 要求把 golang.org/x/mobile 加进
// go.mod，而本项目目前零依赖，不值得为一个入口引入一条持续维护的依赖。JNI 桥只有
// 两个函数，cgo 直接写即可。
package main

/*
#include <jni.h>

// JNIEnv 里的成员是函数指针宏（(*env)->GetStringUTFChars），不是真实符号，
// cgo 无法直接引用，包一层 C 函数转发。
static const char* jstr_utf(JNIEnv* env, jstring s) {
    return (*env)->GetStringUTFChars(env, s, 0);
}
static void jstr_release(JNIEnv* env, jstring s, const char* p) {
    (*env)->ReleaseStringUTFChars(env, s, p);
}
*/
import "C"

import (
	"context"
	"log"
	"strings"
	"sync"

	"codearts2api-panel/internal/gateway"
)

var (
	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
)

// startGateway 是 JNI 入口的实现，独立出来便于测试（不经 JNI）。
// 返回 0 表示已开始服务，1 表示启动失败。
func startGateway(cfgPath string) int {
	mu.Lock()
	defer mu.Unlock()
	if cancel != nil {
		return 0 // 已在运行：幂等
	}
	ctx, c := context.WithCancel(context.Background())
	cancel = c
	done = make(chan struct{})

	ready := make(chan error, 1)
	go func() {
		defer close(done)
		if err := gateway.RunWithReady(cfgPath, ctx, ready); err != nil {
			log.Printf("gateway: %v", err)
		}
		mu.Lock()
		cancel = nil
		done = nil
		mu.Unlock()
	}()

	// 同步等首次就绪：端口被占等硬失败要能立刻回传给 Java。
	if err := <-ready; err != nil {
		c()
		return 1
	}
	return 0
}

// stopGateway 取消运行并等待其收尾；未运行时安全。
func stopGateway() {
	mu.Lock()
	c, d := cancel, done
	mu.Unlock()
	if c == nil {
		return
	}
	c()
	if d != nil {
		<-d
	}
}

//export Java_com_codearts_panel_GatewayNative_start
func Java_com_codearts_panel_GatewayNative_start(env *C.JNIEnv, clazz C.jclass, jcfg C.jstring) C.jint {
	_ = clazz
	chars := C.jstr_utf(env, jcfg)
	if chars == nil {
		return 1
	}
	path := C.GoString(chars)
	C.jstr_release(env, jcfg, chars)
	return C.jint(startGateway(strings.TrimSpace(path)))
}

//export Java_com_codearts_panel_GatewayNative_stop
func Java_com_codearts_panel_GatewayNative_stop(env *C.JNIEnv, clazz C.jclass) {
	_, _ = env, clazz
	stopGateway()
}

func main() {}
