// Package config reads host-owned configuration. HTTP requests may select a
// configured root by ID, but cannot add paths or change engine mode.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"
)

var ErrInvalid = errors.New("INVALID_CONFIG")
var rootID = regexp.MustCompile("^[a-z][a-z0-9_-]{0,63}$")

type Settings struct {
	StateDir     string
	Sources      map[string]string
	RestoreRoots map[string]string
}

func Default() Settings          { return Settings{StateDir: "/var/lib/tapepilot"} }
func Validate(r io.Reader) error { _, err := Read(r); return err }
func Read(r io.Reader) (Settings, error) {
	c := Default()
	b, err := io.ReadAll(io.LimitReader(r, 4097))
	if err != nil || len(b) > 4096 || !utf8.Valid(b) {
		return c, ErrInvalid
	}
	d := json.NewDecoder(bytes.NewReader(b))
	fields, err := object(d)
	if err != nil {
		return c, err
	}
	if _, err := d.Token(); err != io.EOF {
		return c, ErrInvalid
	}
	for k, v := range map[string]any{"mode": "simulator", "physical_writes_enabled": false, "verify_after_write": true} {
		var actual any
		if err := json.Unmarshal(fields[k], &actual); err != nil || actual != v {
			return c, ErrInvalid
		}
		delete(fields, k)
	}
	for k, v := range fields {
		switch k {
		case "state_dir":
			c.StateDir = ""
			if err = json.Unmarshal(v, &c.StateDir); err != nil || !safeDir(c.StateDir) {
				return c, ErrInvalid
			}
		case "sources", "restore_roots":
			roots, e := object(json.NewDecoder(bytes.NewReader(v)))
			if e != nil {
				return c, e
			}
			paths := map[string]string{}
			for id, raw := range roots {
				var path string
				if !rootID.MatchString(id) || json.Unmarshal(raw, &path) != nil || !safeDir(path) {
					return c, ErrInvalid
				}
				paths[id] = path
			}
			if k == "sources" {
				c.Sources = paths
			} else {
				c.RestoreRoots = paths
			}
		default:
			return c, ErrInvalid
		}
	}
	paths := []string{c.StateDir}
	for _, group := range []map[string]string{c.Sources, c.RestoreRoots} {
		for _, p := range group {
			for _, prev := range paths {
				if p == prev || strings.HasPrefix(p, prev+"/") || strings.HasPrefix(prev, p+"/") {
					return c, ErrInvalid
				}
			}
			paths = append(paths, p)
		}
	}
	return c, nil
}
func safeDir(p string) bool {
	if !filepath.IsAbs(p) || filepath.Clean(p) != p || p == "/" || strings.ContainsRune(p, 0) {
		return false
	}
	for _, system := range []string{"/dev", "/sys", "/proc"} {
		if p == system || strings.HasPrefix(p, system+"/") {
			return false
		}
	}
	return true
}

// CheckHost validates existing ancestors before any directory is created. Source
// and restore roots must already exist and be controlled by the administrator.
func (c Settings) CheckHost() error {
	paths := map[string]bool{c.StateDir: true}
	for _, g := range []map[string]string{c.Sources, c.RestoreRoots} {
		for _, p := range g {
			paths[p] = false
		}
	}
	for p, allowMissing := range paths {
		for {
			info, err := os.Lstat(p)
			if err != nil {
				if !allowMissing || !os.IsNotExist(err) {
					return err
				}
			} else if !info.IsDir() {
				return ErrInvalid
			}
			if p == "/" {
				break
			}
			p = filepath.Dir(p)
		}
	}
	return nil
}
func object(d *json.Decoder) (map[string]json.RawMessage, error) {
	start, err := d.Token()
	if err != nil || start != json.Delim('{') {
		return nil, ErrInvalid
	}
	fields := make(map[string]json.RawMessage)
	for d.More() {
		key, err := d.Token()
		name, ok := key.(string)
		if err != nil || !ok {
			return nil, ErrInvalid
		}
		if _, exists := fields[name]; exists {
			return nil, ErrInvalid
		}
		var val json.RawMessage
		if err = d.Decode(&val); err != nil {
			return nil, ErrInvalid
		}
		fields[name] = val
	}
	end, err := d.Token()
	if err != nil || end != json.Delim('}') {
		return nil, ErrInvalid
	}
	return fields, nil
}
