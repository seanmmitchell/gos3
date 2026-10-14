package admin

import "github.com/seanmmitchell/gos3/internal/storage/multi"

// backingChoice is a backing as offered to every caller in GET /whoami.
type backingChoice struct {
	Name string `json:"name"`
	Kind string `json:"kind"`           // "fs", "memory" or "s3"
	Mode string `json:"mode,omitempty"` // s3: "blob" or "gateway"
}

// backingChoices is the "backings" member of GET /whoami: the backings a
// new bucket can be created on (x-gos3-backing), for every caller, so a
// client can offer the choice without admin:ListBackings.
type backingChoices struct {
	Default   string          `json:"default"`
	Available []backingChoice `json:"available"`
}

func (a *api) backingChoices() backingChoices {
	c := backingChoices{Available: []backingChoice{}}
	r, ok := a.d.Backend.(*multi.Router)
	if !ok {
		return c
	}
	c.Default = r.DefaultBacking()
	for _, b := range r.Backings() {
		c.Available = append(c.Available, backingChoice{Name: b.Name, Kind: b.Kind, Mode: b.Mode})
	}
	return c
}
