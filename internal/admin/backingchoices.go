package admin

import (
	"context"

	"github.com/seanmmitchell/gos3/internal/storage/multi"
)

// backingChoice is a backing as offered to every caller in GET /whoami.
type backingChoice struct {
	Name string `json:"name"`
	Kind string `json:"kind"` // "fs", "memory" or "s3"
	// Kinds are the bucket kinds an s3 backing offers and DefaultKind the
	// kind of buckets created without x-gos3-bucket-kind (omitted for
	// other backings).
	Kinds       []string `json:"kinds,omitempty"`
	DefaultKind string   `json:"defaultKind,omitempty"`
	// Containers are an s3 backing's containers of wrapped buckets
	// (x-gos3-container), the home first (omitted for other backings).
	Containers []containerChoice `json:"containers,omitempty"`
}

// containerChoice is a container as offered in GET /whoami. Labels are set
// by administrators and shown to every caller.
type containerChoice struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	Default bool   `json:"default,omitempty"`
}

// backingChoices is the "backings" member of GET /whoami: the backings a
// new bucket can be created on (x-gos3-backing), for every caller, so a
// client can offer the choice without admin:ListBackings.
type backingChoices struct {
	Default   string          `json:"default"`
	Available []backingChoice `json:"available"`
}

func (a *api) backingChoices(ctx context.Context) backingChoices {
	c := backingChoices{Available: []backingChoice{}}
	r, ok := a.d.Backend.(*multi.Router)
	if !ok {
		return c
	}
	c.Default = r.DefaultBacking()
	for _, b := range r.Backings() {
		ch := backingChoice{Name: b.Name, Kind: b.Kind, Kinds: b.Kinds, DefaultKind: b.DefaultKind}
		if rm, ok := remoteManagerOf(r, b.Name); ok {
			if len(ch.Kinds) == 0 {
				info := rm.Info()
				for _, k := range info.Kinds {
					ch.Kinds = append(ch.Kinds, string(k))
				}
				ch.DefaultKind = string(info.DefaultKind)
			}
			for _, ci := range rm.Containers(ctx) {
				ch.Containers = append(ch.Containers, containerChoice{ID: ci.ID, Label: ci.Label, Default: ci.Default})
			}
		}
		c.Available = append(c.Available, ch)
	}
	return c
}
