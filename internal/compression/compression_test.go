package compression

import (
	"context"
	"testing"

	"github.com/seanmmitchell/gos3/internal/config"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/memory"
)

func policy(mode string) config.CompressionSettings {
	return config.CompressionSettings{Mode: mode, Level: 3, MinSize: 100, SkipContentTypes: []string{"video/*", "image/jpeg"}}
}

func TestParse(t *testing.T) {
	for in, want := range map[string]BucketSetting{
		`{"enabled":true}`:    {Enabled: true},
		` {"enabled":false} `: {},
		`{}`:                  {},
	} {
		got, err := Parse([]byte(in))
		if err != nil || got != want {
			t.Errorf("Parse(%s) = %+v, %v", in, got, err)
		}
	}
	for _, bad := range []string{``, `{"enabled":1}`, `{"enabled":true,"level":9}`, `{} {}`, `[]`, `null x`} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("Parse(%s) succeeded", bad)
		}
	}
	if got, _ := Parse(BucketSetting{Enabled: true}.Encode()); !got.Enabled {
		t.Error("Encode/Parse round trip lost Enabled")
	}
}

func TestEffectiveAndConflict(t *testing.T) {
	on, off := BucketSetting{Enabled: true}, BucketSetting{}
	for _, c := range []struct {
		mode             string
		s                BucketSetting
		effective, clash bool
	}{
		{config.CompressionOptional, on, true, false},
		{config.CompressionOptional, off, false, false},
		{config.CompressionEnforced, on, true, false},
		{config.CompressionEnforced, off, true, true},
		{config.CompressionDisabled, on, false, true},
		{config.CompressionDisabled, off, false, false},
	} {
		p := policy(c.mode)
		if got := Effective(p, c.s); got != c.effective {
			t.Errorf("Effective(%s, %+v) = %v", c.mode, c.s, got)
		}
		if got := Conflict(p, c.s) != ""; got != c.clash {
			t.Errorf("Conflict(%s, %+v) = %q", c.mode, c.s, Conflict(p, c.s))
		}
	}
}

func TestDecide(t *testing.T) {
	p := policy(config.CompressionOptional)
	deflate3 := storage.Compression{Algorithm: storage.CompressionDeflate, Level: 3}
	for _, c := range []struct {
		name    string
		enabled bool
		o       Object
		want    storage.Compression
	}{
		{"enabled", true, Object{ContentType: "text/plain", Size: 1000}, deflate3},
		{"unknown size", true, Object{ContentType: "text/plain", Size: -1}, deflate3},
		{"no content type", true, Object{Size: 1000}, deflate3},
		{"bucket off", false, Object{ContentType: "text/plain", Size: 1000}, storage.Compression{}},
		{"empty", true, Object{Size: 0}, storage.Compression{}},
		{"small", true, Object{Size: 99}, storage.Compression{}},
		{"min size", true, Object{Size: 100}, deflate3},
		{"skipped type", true, Object{ContentType: "video/mp4", Size: 1 << 20}, storage.Compression{}},
		{"skipped type params", true, Object{ContentType: "image/JPEG; foo=bar", Size: 1 << 20}, storage.Compression{}},
		{"gzip encoded", true, Object{ContentType: "text/plain", ContentEncoding: "gzip", Size: 1 << 20}, storage.Compression{}},
		{"listed encodings", true, Object{ContentEncoding: "identity, BR", Size: 1 << 20}, storage.Compression{}},
		{"identity encoding", true, Object{ContentEncoding: "identity", Size: 1 << 20}, deflate3},
	} {
		if got := Decide(p, c.enabled, c.o); got != c.want {
			t.Errorf("%s: Decide = %+v, want %+v", c.name, got, c.want)
		}
	}
}

func TestLoad(t *testing.T) {
	ctx := context.Background()
	b := memory.New()
	defer b.Close()
	if err := b.CreateBucket(ctx, "bkt", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	if s, err := Load(ctx, b, "bkt"); err != nil || s.Enabled {
		t.Fatalf("Load without a setting = %+v, %v", s, err)
	}
	if err := b.PutBucketConfig(ctx, "bkt", storage.ConfigCompression, BucketSetting{Enabled: true}.Encode()); err != nil {
		t.Fatal(err)
	}
	if s, err := Load(ctx, b, "bkt"); err != nil || !s.Enabled {
		t.Fatalf("Load = %+v, %v", s, err)
	}
	if _, err := Load(ctx, b, "nobucket"); err == nil {
		t.Fatal("Load of a missing bucket succeeded")
	}
}
