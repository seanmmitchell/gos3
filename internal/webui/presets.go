package webui

import (
	"encoding/json"
	"io/fs"
	"log/slog"
)

// presetsFile is the preset connection list the UI loads from its base URL
// (web/public/servers.json, copied into the build).
const presetsFile = "servers.json"

// presetSecretFields are the keys under which servers.json may carry a
// secret key (web/app/utils/connections.ts accepts both).
var presetSecretFields = []string{"secretAccessKey", "secretKey"}

// sanitizedPresets reads servers.json from fsys and returns the content to
// serve instead, if any. Every UI file is public, so secret keys are removed
// from the presets (logging a warning); a file that is not valid JSON is not
// served at all (ok = false with data = nil), since it cannot be checked and
// the UI could not use it anyway. Without a servers.json, ok is false and
// data nil, and the handler serves the file system as is.
func sanitizedPresets(fsys fs.FS, log *slog.Logger) (data []byte, ok bool) {
	if fsys == nil {
		return nil, false
	}
	raw, err := fs.ReadFile(fsys, presetsFile)
	if err != nil {
		return nil, false
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		log.Warn("web UI: servers.json is not valid JSON; not serving it", "error", err)
		return nil, true
	}
	if n := stripSecrets(doc); n > 0 {
		log.Warn("web UI: removed secret keys from servers.json; the UI files are public, so presets must not carry secrets (enter the secret key in the UI instead)", "removed", n)
		out, err := json.MarshalIndent(doc, "", "  ")
		if err != nil { // cannot happen: doc came from json.Unmarshal
			return nil, true
		}
		return append(out, '\n'), true
	}
	return raw, true
}

// stripSecrets deletes the preset secret fields from every object in v,
// recursively, and returns how many it removed.
func stripSecrets(v any) int {
	n := 0
	switch t := v.(type) {
	case map[string]any:
		for _, k := range presetSecretFields {
			if _, ok := t[k]; ok {
				delete(t, k)
				n++
			}
		}
		for _, child := range t {
			n += stripSecrets(child)
		}
	case []any:
		for _, child := range t {
			n += stripSecrets(child)
		}
	}
	return n
}
