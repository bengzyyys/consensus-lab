package consensus

import (
	"encoding/json"
	"os"
	"path/filepath"
)

const stateFileName = "state.json"

// saveState 原子地写入状态：先写临时文件、fsync、再 rename。
// 任何一步失败都返回错误，目标文件要么保持旧状态、要么是完整的新状态。
func saveState(dir string, st *state) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, stateFileName+".tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, stateFileName))
}

// loadState 从目录读取状态文件。文件不存在时返回 os.ErrNotExist。
func loadState(dir string) (*state, error) {
	data, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		return nil, err
	}
	var st state
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, err
	}
	return &st, nil
}
