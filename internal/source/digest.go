package source

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
)

// Digest binds an operator preview to the source identity and ordered metadata
// observed by the planner. The executor independently replans under its lease.
func (p *Plan) Digest() (string, error) {
	h := sha256.New()
	if err := json.NewEncoder(h).Encode(p.rootEntry); err != nil {
		return "", err
	}
	f, err := os.Open(filepath.Join(p.privateDirectory, "entries.jsonl"))
	if err != nil {
		return "", err
	}
	_, readErr := io.Copy(h, f)
	if err = errors.Join(readErr, f.Close()); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
