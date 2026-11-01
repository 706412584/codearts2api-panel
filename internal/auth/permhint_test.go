package auth

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestPermHintText 权限错误必须带上 Docker 三选一指引，且仍可被 errors.Is 识别。
// 平台无关：直接构造 EACCES，Windows 上也跑。
func TestPermHintText(t *testing.T) {
	denied := &fs.PathError{Op: "open", Path: "/app/auths/.codearts-u1.json.tmp", Err: fs.ErrPermission}
	err := permHint(denied)
	for _, want := range []string{"chown", "10001", "PUID=$(id -u) PGID=$(id -g)", `user: "0:0"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("指引缺 %q，实际: %v", want, err)
		}
	}
	if !errors.Is(err, fs.ErrPermission) {
		t.Errorf("包裹后应保留 errors.Is(fs.ErrPermission)，实际: %v", err)
	}
	if !strings.Contains(err.Error(), "/app/auths/.codearts-u1.json.tmp") {
		t.Errorf("原始错误应保留（含路径），实际: %v", err)
	}

	// 非权限错误原样返回：不加指引，也不改变错误本身。
	other := errors.New("disk full")
	if got := permHint(other); got != other {
		t.Errorf("非权限错误应原样返回，实际: %v", got)
	}
	if permHint(nil) != nil {
		t.Error("nil 应返回 nil")
	}
}

// TestSavePermissionHint 无写权限目录下落盘，保存与刷新两条路径都应给出 chown 指引。
func TestSavePermissionHint(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("Windows 无 POSIX 权限语义 / root 不受目录权限限制，跳过")
	}
	dir := t.TempDir()
	ro := filepath.Join(dir, "ro")
	if err := os.MkdirAll(ro, 0o755); err != nil {
		t.Fatal(err)
	}
	a := New("u1", "n1", "d1", "tok", "ak", "sk", "2030-01-01T00:00:00Z", "rt", "cv")
	if err := SaveNew(ro, a); err != nil { // 先用可写目录写出首个凭证
		t.Fatal(err)
	}
	if err := os.Chmod(ro, 0o555); err != nil { // 再让目录变只读，模拟宿主机属主不匹配
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(ro, 0o755) })

	t.Run("refresh", func(t *testing.T) { // Save → saveLocked 的 tmp+rename 路径
		err := a.Save()
		if err == nil {
			t.Fatal("只读目录写回应失败")
		}
		if !strings.Contains(err.Error(), "chown") || !strings.Contains(err.Error(), "10001") {
			t.Errorf("权限错误应包含 Docker 指引，实际: %v", err)
		}
	})

	t.Run("new", func(t *testing.T) { // SaveNew → OpenFile(O_EXCL) 路径
		b := New("u2", "n2", "d2", "tok", "ak", "sk", "2030-01-01T00:00:00Z", "rt", "cv")
		err := SaveNew(ro, b)
		if err == nil {
			t.Fatal("只读目录新建凭证应失败")
		}
		if !strings.Contains(err.Error(), "chown") || !strings.Contains(err.Error(), "10001") {
			t.Errorf("权限错误应包含 Docker 指引，实际: %v", err)
		}
	})
}
