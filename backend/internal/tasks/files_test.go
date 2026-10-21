package tasks

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// validResult builds the smallest result the publisher accepts.
func validResult(t *testing.T) (map[string]any, string) {
	t.Helper()
	trackID := "11111111-1111-4111-8111-111111111111"
	return map[string]any{
		"tracks": []any{map[string]any{
			"id": trackID, "label": "Piano", "program": 0, "confidence": 0.8,
			"label_source": "model", "is_drum": false, "source_stem_ids": []string{},
		}},
		"artifacts": []any{map[string]any{
			"track_id": trackID, "kind": "score", "label": "score.musicxml",
			"mime": "application/xml", "status": "ready", "error": "",
			"path": "score.musicxml",
		}},
		"warnings": []string{}, "engine": "baseline", "actual_device": "cpu",
	}, trackID
}

func decodeResult(t *testing.T, payload map[string]any) pipelineResult {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	result, err := parsePipelineResult(raw)
	if err != nil {
		t.Fatalf("parse result: %v", err)
	}
	return result
}

func TestValidateResultAcceptsContractedShape(t *testing.T) {
	payload, trackID := validResult(t)
	reported := payload["artifacts"].([]any)
	payload["artifacts"] = append(reported,
		map[string]any{
			// Stem identity is independent of score tracks: no track row needed.
			"track_id": "22222222-2222-4222-8222-222222222222", "kind": "stem",
			"label": "vocals.wav", "mime": "audio/wav", "status": "ready",
			"error": "", "path": "stems/vocals.wav",
		},
		map[string]any{
			"track_id": nil, "kind": "pdf", "label": "score.pdf",
			"mime": "application/pdf", "status": "failed",
			"error": "musescore 不可用", "path": "",
		},
	)

	tracks, artifacts, err := validateResult(decodeResult(t, payload))
	if err != nil {
		t.Fatalf("validateResult: %v", err)
	}
	if len(tracks) != 1 || tracks[0].ID != trackID {
		t.Fatalf("tracks = %+v", tracks)
	}
	if len(artifacts) != 3 {
		t.Fatalf("artifacts = %+v", artifacts)
	}
	if artifacts[0].Suffix != ".musicxml" {
		t.Fatalf("ready artifact suffix = %q", artifacts[0].Suffix)
	}
	if artifacts[2].Error == nil || *artifacts[2].Error != "musescore 不可用" {
		t.Fatalf("failed artifact error = %v", artifacts[2].Error)
	}
	if artifacts[0].ID == "" || artifacts[1].ID == "" || artifacts[0].ID == artifacts[1].ID {
		t.Fatalf("artifact IDs must be fresh UUIDs: %+v", artifacts)
	}
}

func TestValidateResultRejectsContractViolations(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"empty result", func(map[string]any) {}},
		{"uppercase track id", func(payload map[string]any) {
			payload["tracks"].([]any)[0].(map[string]any)["id"] = "11111111-1111-4111-8111-11111111111A"
		}},
		{"non uuid track id", func(payload map[string]any) {
			payload["tracks"].([]any)[0].(map[string]any)["id"] = "not-a-uuid"
		}},
		{"duplicate track id", func(payload map[string]any) {
			tracks := payload["tracks"].([]any)
			payload["tracks"] = append(tracks, tracks[0])
		}},
		{"midi program out of range", func(payload map[string]any) {
			payload["tracks"].([]any)[0].(map[string]any)["program"] = 200
		}},
		{"negative midi program", func(payload map[string]any) {
			payload["tracks"].([]any)[0].(map[string]any)["program"] = -1
		}},
		{"confidence above one", func(payload map[string]any) {
			payload["tracks"].([]any)[0].(map[string]any)["confidence"] = 1.5
		}},
		{"unknown provenance", func(payload map[string]any) {
			payload["tracks"].([]any)[0].(map[string]any)["label_source"] = "guessed"
		}},
		{"missing score", func(payload map[string]any) {
			payload["artifacts"] = []any{}
		}},
		{"failed score only", func(payload map[string]any) {
			artifact := payload["artifacts"].([]any)[0].(map[string]any)
			artifact["status"] = "failed"
		}},
		{"original artifact from pipeline", func(payload map[string]any) {
			payload["artifacts"].([]any)[0].(map[string]any)["kind"] = "original"
		}},
		{"unknown artifact kind", func(payload map[string]any) {
			payload["artifacts"].([]any)[0].(map[string]any)["kind"] = "exe"
		}},
		{"unknown artifact status", func(payload map[string]any) {
			payload["artifacts"].([]any)[0].(map[string]any)["status"] = "pending"
		}},
		{"unknown artifact track", func(payload map[string]any) {
			payload["artifacts"].([]any)[0].(map[string]any)["track_id"] = "33333333-3333-4333-8333-333333333333"
		}},
		{"missing ready path", func(payload map[string]any) {
			payload["artifacts"].([]any)[0].(map[string]any)["path"] = ""
		}},
		{"invalid artifact track id", func(payload map[string]any) {
			payload["artifacts"].([]any)[0].(map[string]any)["track_id"] = "abc"
		}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			base, _ := validResult(t)
			if testCase.name == "empty result" {
				base = map[string]any{}
			}
			testCase.mutate(base)
			if _, _, err := validateResult(decodeResult(t, base)); err == nil {
				t.Fatalf("validateResult accepted %s", testCase.name)
			}
		})
	}
}

func TestValidateResultRejectsNonFiniteNumbers(t *testing.T) {
	// JSON cannot carry NaN/Inf, so this exercises the guard directly: a
	// future codec change must not let an unusable number reach the database.
	result := pipelineResult{Tracks: []pipelineTrack{{}},
		Artifacts: []pipelineArtifact{{Kind: "score", Status: "ready", Path: "s.xml"}}}

	for name, value := range map[string]float64{"NaN": math.NaN(), "+Inf": math.Inf(1)} {
		t.Run("confidence "+name, func(t *testing.T) {
			candidate := result
			candidate.Tracks[0] = pipelineTrack{ID: "11111111-1111-4111-8111-111111111111",
				Label: "Piano", LabelSource: "model", Confidence: &value}
			if _, _, err := validateResult(candidate); err == nil {
				t.Fatal("expected a non-finite confidence to be rejected")
			}
		})
	}
}

func TestValidateResultTruncatesUntrustedText(t *testing.T) {
	payload, _ := validResult(t)
	track := payload["tracks"].([]any)[0].(map[string]any)
	track["label"] = strings.Repeat("音", 400)
	payload["warnings"] = []string{strings.Repeat("w", 900)}
	artifact := payload["artifacts"].([]any)[0].(map[string]any)
	artifact["path"] = "score.musicxml"
	artifact["mime"] = strings.Repeat("m", 400)

	tracks, artifacts, err := validateResult(decodeResult(t, payload))
	if err != nil {
		t.Fatalf("validateResult: %v", err)
	}
	if len([]rune(tracks[0].Label)) != maxLabelRunes {
		t.Fatalf("track label runes = %d", len([]rune(tracks[0].Label)))
	}
	if len(artifacts[0].Mime) != 128 {
		t.Fatalf("mime length = %d", len(artifacts[0].Mime))
	}
}

func TestValidateResultSourceStemIDsNeverNil(t *testing.T) {
	payload, _ := validResult(t)
	payload["tracks"].([]any)[0].(map[string]any)["source_stem_ids"] = nil
	tracks, _, err := validateResult(decodeResult(t, payload))
	if err != nil {
		t.Fatalf("validateResult: %v", err)
	}
	if tracks[0].SourceStemIDs == nil {
		t.Fatal("source_stem_ids must be an empty slice, not nil")
	}
}

func TestResolveArtifactPathScope(t *testing.T) {
	root := workspaceDir(t)
	workDir := filepath.Join(root, "work")
	if err := os.MkdirAll(filepath.Join(workDir, "stems"), 0o700); err != nil {
		t.Fatalf("mkdir work: %v", err)
	}
	writeFile(t, workDir, "score.musicxml", "<score/>")
	writeFile(t, workDir, "stems/vocals.wav", "wav")
	outside := writeFile(t, root, "secret.txt", "secret")

	t.Run("accepts nested file", func(t *testing.T) {
		resolved, err := resolveArtifactPath(workDir, "stems/vocals.wav")
		if err != nil {
			t.Fatalf("resolveArtifactPath: %v", err)
		}
		if !strings.HasSuffix(resolved, filepath.Join("stems", "vocals.wav")) {
			t.Fatalf("resolved = %q", resolved)
		}
	})

	rejects := []struct {
		name string
		path string
	}{
		{"absolute", outside},
		{"parent traversal", "../secret.txt"},
		{"dot", "."},
		{"empty", ""},
		{"nested traversal", "stems/../../secret.txt"},
	}
	for _, testCase := range rejects {
		t.Run("rejects "+testCase.name, func(t *testing.T) {
			if _, err := resolveArtifactPath(workDir, testCase.path); err == nil {
				t.Fatalf("accepted %q", testCase.path)
			}
		})
	}

	t.Run("rejects symlinked file outside", func(t *testing.T) {
		link := filepath.Join(workDir, "leak.txt")
		if err := os.Symlink(outside, link); err != nil {
			t.Skipf("symlink unsupported: %v", err)
		}
		if _, err := resolveArtifactPath(workDir, "leak.txt"); err == nil {
			t.Fatal("accepted a symlink escaping the work directory")
		}
	})

	t.Run("rejects symlinked directory outside", func(t *testing.T) {
		link := filepath.Join(workDir, "linkdir")
		if err := os.Symlink(root, link); err != nil {
			t.Skipf("symlink unsupported: %v", err)
		}
		if _, err := resolveArtifactPath(workDir, "linkdir/secret.txt"); err == nil {
			t.Fatal("accepted a directory symlink escaping the work directory")
		}
	})

	t.Run("rejects directory artifact", func(t *testing.T) {
		if _, err := resolveArtifactPath(workDir, "stems"); err == nil {
			t.Fatal("accepted a directory as an artifact")
		}
	})

	t.Run("rejects missing file", func(t *testing.T) {
		if _, err := resolveArtifactPath(workDir, "missing.mid"); err == nil {
			t.Fatal("accepted a missing artifact")
		}
	})
}

func TestCopyArtifactIsAtomicAndHashed(t *testing.T) {
	root := workspaceDir(t)
	source := writeFile(t, root, "source/score.musicxml", "<score>payload</score>")
	targetDir := filepath.Join(root, "results")
	if err := os.MkdirAll(targetDir, 0o700); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}

	digest, size, err := copyArtifact(source, targetDir, "artifact-id", ".musicxml")
	if err != nil {
		t.Fatalf("copyArtifact: %v", err)
	}
	content, err := os.ReadFile(filepath.Join(targetDir, "artifact-id.musicxml"))
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if string(content) != "<score>payload</score>" {
		t.Fatalf("target content = %q", content)
	}
	expected := sha256.Sum256(content)
	if digest != hex.EncodeToString(expected[:]) {
		t.Fatalf("digest = %q", digest)
	}
	if size != int64(len(content)) {
		t.Fatalf("size = %d", size)
	}
	if _, err := os.Stat(filepath.Join(targetDir, "artifact-id.tmp")); !os.IsNotExist(err) {
		t.Fatal("temporary file must be renamed away")
	}

	t.Run("missing source", func(t *testing.T) {
		if _, _, err := copyArtifact(filepath.Join(root, "nope"), targetDir, "x", ".mid"); err == nil {
			t.Fatal("expected an error for a missing source")
		}
	})

	t.Run("oversized source is refused and leaves no temp", func(t *testing.T) {
		big := filepath.Join(root, "source", "big.mid")
		handle, err := os.Create(big)
		if err != nil {
			t.Fatalf("create big: %v", err)
		}
		if err := handle.Truncate(maxResultBytes + 1); err != nil {
			_ = handle.Close()
			t.Fatalf("truncate: %v", err)
		}
		_ = handle.Close()
		if _, _, err := copyArtifact(big, targetDir, "big", ".mid"); err == nil {
			t.Fatal("expected the size limit to reject the artifact")
		}
		if _, err := os.Stat(filepath.Join(targetDir, "big.tmp")); !os.IsNotExist(err) {
			t.Fatal("temporary file must be removed on failure")
		}
	})
}

func TestTruncateRespectsRunes(t *testing.T) {
	if got := truncate("ab", 5); got != "ab" {
		t.Fatalf("short value changed: %q", got)
	}
	if got := truncate("音频文件", 2); got != "音频" {
		t.Fatalf("truncate = %q", got)
	}
}

func TestDecodeJobMessageRejectsGarbage(t *testing.T) {
	valid := []byte(`{"job_id":"11111111-1111-4111-8111-111111111111","attempt":2}`)
	message, err := decodeJobMessage(valid)
	if err != nil {
		t.Fatalf("decode valid message: %v", err)
	}
	if message.Attempt != 2 || message.JobID != "11111111-1111-4111-8111-111111111111" {
		t.Fatalf("decoded = %+v", message)
	}

	invalid := map[string]string{
		"not json":       `{"job_id": `,
		"missing job id": `{"attempt":1}`,
		"non uuid":       `{"job_id":"abc","attempt":1}`,
		"uppercase uuid": `{"job_id":"11111111-1111-4111-8111-11111111111A","attempt":1}`,
		"zero attempt":   `{"job_id":"11111111-1111-4111-8111-111111111111","attempt":0}`,
		"no attempt":     `{"job_id":"11111111-1111-4111-8111-111111111111"}`,
	}
	for name, payload := range invalid {
		if _, err := decodeJobMessage([]byte(payload)); err == nil {
			t.Fatalf("%s: expected a decode error", name)
		}
	}
}
