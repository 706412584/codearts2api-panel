package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

type fileSettings struct {
	mu   sync.Mutex
	path string
	// apply 落盘成功后热应用配置并返回仍需重启的字段（由 main 注入）。
	// 为 nil 表示没有可热应用的组件（测试/裸用），全部按"无需重启"返回。
	apply func(*Config) []string
}

func configMap(c *Config) (map[string]any, error) {
	raw, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	var result map[string]any
	err = json.Unmarshal(raw, &result)
	return result, err
}

func (s *fileSettings) readLocked() (map[string]any, error) {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return nil, err
	}
	var values map[string]any
	if err := json.Unmarshal(stripJSONComments(raw), &values); err != nil {
		return nil, fmt.Errorf("parse configuration: %w", err)
	}
	if values == nil {
		return nil, errors.New("configuration must be an object")
	}
	base, err := configMap(Default())
	if err != nil {
		return nil, err
	}
	mergeConfig(base, values)
	return base, nil
}

func redactConfig(values map[string]any) map[string]any {
	values["api_key"] = ""
	values["admin_key"] = ""
	return values
}

func (s *fileSettings) Read() (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	values, err := s.readLocked()
	if err != nil {
		return nil, err
	}
	return redactConfig(values), nil
}

func mergeConfig(base, patch map[string]any) {
	for key, value := range patch {
		nested, isMap := value.(map[string]any)
		existing, hasMap := base[key].(map[string]any)
		if isMap && hasMap {
			mergeConfig(existing, nested)
		} else {
			base[key] = value
		}
	}
}

func (s *fileSettings) Update(patch map[string]any) (map[string]any, error) {
	values, _, err := s.update(patch)
	return values, err
}

// UpdateLive 保存配置并热应用，返回仍需重启的字段名（面板据此提示）。
func (s *fileSettings) UpdateLive(patch map[string]any) (map[string]any, []string, error) {
	return s.update(patch)
}

// update 校验 → 落盘 → 热应用。校验失败不写盘；热应用只在落盘成功后执行。
func (s *fileSettings) update(patch map[string]any) (map[string]any, []string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	values, err := s.readLocked()
	if err != nil {
		return nil, nil, err
	}
	// Empty redacted keys mean "keep"; null or a non-string is invalid.
	for _, key := range []string{"api_key", "admin_key"} {
		if value, present := patch[key]; present {
			text, ok := value.(string)
			if !ok {
				return nil, nil, fmt.Errorf("%s must be a string", key)
			}
			if text == "" {
				patch[key] = values[key]
			}
		}
	}
	mergeConfig(values, patch)
	raw, err := json.MarshalIndent(values, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	candidate := Default()
	if err := json.Unmarshal(raw, candidate); err != nil {
		return nil, nil, fmt.Errorf("invalid configuration: %w", err)
	}
	applyEnv(candidate)
	if err := candidate.validate(); err != nil {
		return nil, nil, err
	}
	if err := writeConfigAtomic(s.path, append(raw, '\n')); err != nil {
		return nil, nil, err
	}
	var restart []string
	if s.apply != nil {
		restart = s.apply(candidate)
	}
	return redactConfig(values), restart, nil
}

func writeConfigAtomic(path string, raw []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return errors.New("configuration path must be a regular file")
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".config-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return err
	}
	if _, err := file.Write(raw); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), path); err != nil {
		// 单文件 Docker bind mount 的挂载点无法被 rename 覆盖（Linux 返回 EBUSY）。
		// 普通文件继续走原子替换，这种部署形态则退回原地截断写入。
		if !errors.Is(err, syscall.EBUSY) {
			return err
		}
		log.Printf("config: %s 是 bind mount 挂载点，无法原子替换，改为原地写入", path)
		out, readErr := os.ReadFile(file.Name())
		if readErr != nil {
			return readErr
		}
		target, openErr := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0o600)
		if openErr != nil {
			return fmt.Errorf("replace config (bind mount fallback): %w", openErr)
		}
		_, writeErr := target.Write(out)
		if writeErr == nil {
			writeErr = target.Sync()
		}
		closeErr := target.Close()
		// 写失败时保留 tmp：挂载文件已被 O_TRUNC 清空，tmp 里是完整新内容，可手工恢复。
		if writeErr != nil {
			return fmt.Errorf("replace config (bind mount fallback, 完整新内容保留在 %s): %w", file.Name(), writeErr)
		}
		if closeErr != nil {
			return closeErr
		}
		return nil
	}
	return nil
}

func ensureConfig(path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	c := Default()
	newKey := func() (string, error) {
		var key [32]byte
		if _, err := rand.Read(key[:]); err != nil {
			return "", err
		}
		return hex.EncodeToString(key[:]), nil
	}
	var err error
	if c.APIKey, err = newKey(); err != nil {
		return err
	}
	if c.AdminKey, err = newKey(); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if os.IsExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		file.Close()
		if !ok {
			os.Remove(path)
		}
	}()
	if _, err := file.Write(append(raw, '\n')); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	ok = true
	return nil
}
