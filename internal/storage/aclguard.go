package storage

import (
	"bytes"
	"encoding/xml"
	"strings"
)

// ACLGuardConfigs are the configuration documents that can keep object
// ACLs from granting access: a public access block ignoring public ACLs,
// and ownership controls disabling ACLs. Code that copies versions with
// their ACLs into another bucket (copy jobs, snapshot restores) copies a
// source's guards whatever else it copies (see ACLGuard), so the copies do
// not become public.
var ACLGuardConfigs = []string{ConfigPublicAccessBlock, ConfigOwnership}

// ACLGuard reports whether configuration document data (of key) keeps
// object ACLs from granting access, as the S3 layer applies it: a public
// access block with IgnorePublicAcls (an unreadable one counts, as it is
// applied as the most restrictive), ownership controls with
// BucketOwnerEnforced.
func ACLGuard(key string, data []byte) bool {
	if len(data) == 0 {
		return false
	}
	switch key {
	case ConfigPublicAccessBlock:
		var pab struct {
			IgnorePublicAcls bool `xml:"IgnorePublicAcls"`
		}
		if xml.Unmarshal(data, &pab) != nil {
			return true
		}
		return pab.IgnorePublicAcls
	case ConfigOwnership:
		var oc struct {
			Rules []struct {
				ObjectOwnership string `xml:"ObjectOwnership"`
			} `xml:"Rule"`
		}
		if xml.Unmarshal(data, &oc) != nil {
			return bytes.Contains(data, []byte("BucketOwnerEnforced"))
		}
		for _, r := range oc.Rules {
			if strings.TrimSpace(r.ObjectOwnership) == "BucketOwnerEnforced" {
				return true
			}
		}
	}
	return false
}
