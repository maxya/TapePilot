package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"reflect"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/maxya/TapePilot/internal/restore"
	"github.com/maxya/TapePilot/internal/source"
	"github.com/maxya/TapePilot/internal/store"
)

var ErrInput = errors.New("INVALID_REQUEST")

func reply(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func fail(w http.ResponseWriter, r *http.Request, err error) {
	code, status := "OPERATION_FAILED", http.StatusInternalServerError
	switch {
	case errors.Is(err, store.ErrOperations), errors.Is(err, ErrInput), errors.Is(err, source.ErrUnsafe), errors.Is(err, store.ErrEvidence):
		code, status = "INVALID_REQUEST", 400
	case errors.Is(err, restore.ErrUnsafe):
		code, status = "UNSAFE_ARCHIVE", 422
	case errors.Is(err, source.ErrChanged):
		code, status = "SOURCE_CHANGED", 409
	case errors.Is(err, store.ErrNotFound):
		code, status = "NOT_FOUND", 404
	case errors.Is(err, store.ErrProfileDisabled), errors.Is(err, store.ErrOutstanding), errors.Is(err, store.ErrConflict):
		code, status = "STATE_CONFLICT", 409
	case errors.Is(err, restore.ErrDestination):
		code, status = "RESTORE_DESTINATION_UNAVAILABLE", 409
	case errors.Is(err, restore.ErrSpace):
		code, status = "INSUFFICIENT_SPACE", 409
	case errors.Is(err, source.ErrLimit), errors.Is(err, restore.ErrLimit):
		code, status = "RESOURCE_LIMIT", 422
	}
	id := w.Header().Get("X-Request-ID")
	if id == "" {
		id = uuid.NewString()
		w.Header().Set("X-Request-ID", id)
	}
	message := map[string]string{
		"UNSAFE_ARCHIVE":                  "Archive entries cannot be extracted safely. Inspect the media read-only.",
		"INSUFFICIENT_SPACE":              "Free space in the spool and restore root, or select a restore root with more available space.",
		"RESTORE_DESTINATION_UNAVAILABLE": "Choose a fresh destination name under a configured writable restore root.",
		"SOURCE_CHANGED":                  "The source changed after preview. Review a new plan before running this backup.",
		"RESOURCE_LIMIT":                  "The requested selection exceeds a service limit. Narrow the selection or query.",
	}[code]
	if message == "" {
		message = http.StatusText(status)
	}
	reply(w, status, map[string]any{"error": map[string]string{"code": code, "message": message, "request_id": id}})
}
func body(w http.ResponseWriter, r *http.Request, dest any) error {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return ErrInput
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		return ErrInput
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return ErrInput
	}
	allowed := map[string]bool{}
	t := reflect.TypeOf(dest).Elem()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name := strings.Split(f.Tag.Get("json"), ",")[0]
		if name == "" {
			name = f.Name
		}
		allowed[name] = true
	}
	for d.More() {
		k, err := d.Token()
		if err != nil {
			return ErrInput
		}
		name, ok := k.(string)
		if !ok || !allowed[name] {
			return ErrInput
		}
		delete(allowed, name)
		var v json.RawMessage
		if d.Decode(&v) != nil || string(v) == "null" {
			return ErrInput
		}
	}
	if _, err = d.Token(); err != nil {
		return ErrInput
	}
	if _, err = d.Token(); err != io.EOF {
		return ErrInput
	}
	d = json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(dest) != nil {
		return ErrInput
	}
	return nil
}
func pageNumber(raw string, fallback, max int64) (int64, error) {
	if raw == "" {
		return fallback, nil
	}
	v, e := strconv.ParseInt(raw, 10, 64)
	if e != nil || v < 0 || v > max {
		return 0, ErrInput
	}
	return v, nil
}
