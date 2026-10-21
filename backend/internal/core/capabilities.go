package core

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Baseline limitations are shared with the pipeline declaration so the UI does
// not overstate capability.
var baselineLimitations = []string{
	"自动扒谱初稿，可能漏音、错音和节奏误差",
	"分离器粗分类，未实现任意乐器识别；同类多个演奏者不可区分",
	"鼓为频带 onset 近似检测，不是可靠鼓分类；other 为未知/混合声部",
}

var multiInstrumentLimitations = []string{
	"候选模型及 CPU / Tesla P4 尚未验证，不能用于任务",
}

type fileDigest struct {
	size    int64
	modTime int64
	sha256  string
	ok      bool
}

var digestCache sync.Map // map[string]fileDigest

// fileSHA256 returns the hex digest of a file, cached by size and mtime so the
// API never rehashes large shared model weights on every request.
func fileSHA256(path string) (string, bool) {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return "", false
	}
	cacheKey := path
	if cached, ok := digestCache.Load(cacheKey); ok {
		entry, _ := cached.(fileDigest)
		if entry.size == info.Size() && entry.modTime == info.ModTime().UnixNano() {
			return entry.sha256, entry.ok
		}
	}
	file, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer file.Close()
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return "", false
	}
	digest := hex.EncodeToString(hasher.Sum(nil))
	digestCache.Store(cacheKey, fileDigest{
		size: info.Size(), modTime: info.ModTime().UnixNano(), sha256: digest, ok: true,
	})
	return digest, true
}

type manifestModel struct {
	ID     string `json:"id"`
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type manifestFile struct {
	Models []manifestModel `json:"models"`
}

// baselineReady checks only shared files. It never imports or executes Python,
// so the API process needs no model dependencies.
func baselineReady(cfg Config) bool {
	// API and dispatcher containers are Go-only and intentionally have neither
	// a model venv nor ffmpeg. Readiness is about the shared weights and the
	// pipeline interpreter; execution tools are checked by the worker itself.
	if strings.TrimSpace(cfg.PipelinePython) == "" {
		return false
	}
	if _, err := os.Stat(filepath.Join(cfg.DemucsRepo, "htdemucs_6s.yaml")); err != nil {
		return false
	}
	root := filepath.Dir(cfg.DemucsRepo)
	raw, err := os.ReadFile(filepath.Join(root, "manifest.json"))
	if err != nil {
		return false
	}
	var manifest manifestFile
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return false
	}
	models := make(map[string]manifestModel, len(manifest.Models))
	for _, model := range manifest.Models {
		models[model.ID] = model
	}
	for _, id := range []string{"htdemucs_6s", "basic_pitch_onnx"} {
		model, ok := models[id]
		if !ok || len(model.SHA256) != 64 || model.Path == "" {
			return false
		}
		path := filepath.Join(root, model.Path)
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
			return false
		}
		digest, ok := fileSHA256(path)
		if !ok || digest != model.SHA256 {
			return false
		}
	}
	return true
}

// Capabilities declares the engines known to this deployment. The pipeline owns
// the authoritative list; the API reports the same two entries without Python.
func Capabilities(cfg Config) []map[string]any {
	return []map[string]any{
		{
			"id":          "baseline",
			"label":       "Demucs 6类 + Basic Pitch（有限能力）",
			"ready":       baselineReady(cfg),
			"limitations": append([]string(nil), baselineLimitations...),
		},
		{
			"id":          "multi_instrument",
			"label":       "多乐器识别（未集成）",
			"ready":       false,
			"limitations": append([]string(nil), multiInstrumentLimitations...),
		},
	}
}

// BaselineReady reports whether audio jobs can run on this deployment.
func BaselineReady(cfg Config) bool { return baselineReady(cfg) }
