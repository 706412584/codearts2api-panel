package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

type fileSettings struct {
	mu   sync.Mutex
	path string
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
	s.mu.Lock()
	defer s.mu.Unlock()
	values, err := s.readLocked()
	if err != nil {
		return nil, err
	}
	// Empty redacted keys mean "keep"; null or a non-string is invalid.
	for _, key := range []string{"api_key", "admin_key"} {
		if value, present := patch[key]; present {
			text, ok := value.(string)
			if !ok {
				return nil, fmt.Errorf("%s must be a string", key)
			}
			if text == "" {
				patch[key] = values[key]
			}
		}
	}
	mergeConfig(values, patch)
	raw, err := json.MarshalIndent(values, "", "  ")
	if err != nil {
		return nil, err
	}
	candidate := Default()
	if err := json.Unmarshal(raw, candidate); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}
	applyEnv(candidate)
	if err := candidate.validate(); err != nil {
		return nil, err
	}
	if err := writeConfigAtomic(s.path, append(raw, '\n')); err != nil {
		return nil, err
	}
	return redactConfig(values), nil
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
	return os.Rename(file.Name(), path)
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
