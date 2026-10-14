package admin

import "net/http"

// BackingLocal is the name of the server's own storage as a backing.
const BackingLocal = "local"

// Backing describes a storage backing (GET /backings). It never carries
// credentials.
type Backing struct {
	Name     string `json:"name"`
	Mode     string `json:"mode"` // "local", "blob" or "gateway"
	Endpoint string `json:"endpoint"`
	Region   string `json:"region"`
	Bucket   string `json:"bucket"`
	Prefix   string `json:"prefix"`
}

// backingChoice is a backing as offered to every caller in GET /whoami.
type backingChoice struct {
	Name string `json:"name"`
	Mode string `json:"mode"`
}

// backingChoices is the "backings" member of GET /whoami.
type backingChoices struct {
	Default   string          `json:"default"`
	Available []backingChoice `json:"available"`
}

// backings returns the configured backings, "local" first.
func (a *api) backings() []Backing {
	out := []Backing{{Name: BackingLocal, Mode: BackingLocal}}
	for _, b := range a.d.Backings {
		if b.Name != BackingLocal {
			out = append(out, b)
		}
	}
	return out
}

func (a *api) backingChoices() backingChoices {
	c := backingChoices{Default: backingName(a.d.DefaultBacking)}
	for _, b := range a.backings() {
		c.Available = append(c.Available, backingChoice{Name: b.Name, Mode: b.Mode})
	}
	return c
}

// backingMode returns the mode of the named backing ("" if unknown).
func (a *api) backingMode(name string) string {
	for _, b := range a.backings() {
		if b.Name == name {
			return b.Mode
		}
	}
	return ""
}

// backingName is the external name of a stored backing ("" = "local").
func backingName(backing string) string {
	if backing == "" {
		return BackingLocal
	}
	return backing
}

func (a *api) listBackings(rc *reqCtx) (int, any, error) {
	return http.StatusOK, map[string]any{
		"default":  backingName(a.d.DefaultBacking),
		"backings": a.backings(),
	}, nil
}
