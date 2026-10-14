package config

import (
	"github.com/seanmmitchell/gos3/internal/storage"
	"testing"
)

func TestResticBackingValidation(t *testing.T) {
	c := Default()
	c.Storage.Path = t.TempDir()
	c.Storage.ResticS3 = &storage.ResticS3{Endpoint: "https://s3.example.test", Bucket: "archives", AccessKey: "a", SecretKey: "s"}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.Storage.Backend = BackendMemory
	if err := c.Validate(); err == nil {
		t.Fatal("accepted restic storage on memory backing")
	}
	c.Storage.Backend = BackendFS
	c.Storage.ResticS3.Endpoint = "https://a:b@s3.example.test"
	if err := c.Validate(); err == nil {
		t.Fatal("accepted URL credentials")
	}
}
