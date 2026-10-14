package iam

// builtinOrder is the listing order of the built-in policies.
var builtinOrder = []string{PolicyAdmin, PolicyReadWrite, PolicyReadOnly, PolicyWriteOnly, PolicyDiagnostics}

// builtinPolicies are the read-only built-in policies. They are never
// mutated; callers receive deep copies.
var builtinPolicies = map[string]*NamedPolicy{
	PolicyAdmin: {
		Name:        PolicyAdmin,
		Description: "Full access to all S3 and admin API actions.",
		Builtin:     true,
		Document: PolicyDocument{
			Version: PolicyVersion2012,
			Statement: []Statement{{
				Sid:      "AdminFullAccess",
				Effect:   "Allow",
				Action:   StringOrSlice{"s3:*", "admin:*"},
				Resource: StringOrSlice{"*"},
			}},
		},
	},
	PolicyReadWrite: {
		Name:        PolicyReadWrite,
		Description: "Full access to all buckets and objects (no admin API access).",
		Builtin:     true,
		Document: PolicyDocument{
			Version: PolicyVersion2012,
			Statement: []Statement{{
				Sid:      "ReadWriteAllBuckets",
				Effect:   "Allow",
				Action:   StringOrSlice{"s3:*"},
				Resource: StringOrSlice{s3ARNPrefix + "*"},
			}},
		},
	},
	PolicyReadOnly: {
		Name:        PolicyReadOnly,
		Description: "Read-only access to all buckets and objects (s3:Get*, s3:List*).",
		Builtin:     true,
		Document: PolicyDocument{
			Version: PolicyVersion2012,
			Statement: []Statement{{
				Sid:      "ReadOnlyAllBuckets",
				Effect:   "Allow",
				Action:   StringOrSlice{"s3:Get*", "s3:List*"},
				Resource: StringOrSlice{s3ARNPrefix + "*"},
			}},
		},
	},
	PolicyWriteOnly: {
		Name:        PolicyWriteOnly,
		Description: "Upload-only access to all buckets: put objects and manage multipart uploads.",
		Builtin:     true,
		Document: PolicyDocument{
			Version: PolicyVersion2012,
			Statement: []Statement{{
				Sid:    "WriteOnlyAllBuckets",
				Effect: "Allow",
				Action: StringOrSlice{
					"s3:PutObject",
					"s3:AbortMultipartUpload",
					"s3:ListMultipartUploadParts",
					"s3:ListBucketMultipartUploads",
				},
				Resource: StringOrSlice{s3ARNPrefix + "*"},
			}},
		},
	},
	PolicyDiagnostics: {
		Name:        PolicyDiagnostics,
		Description: "View server information, statistics and discovery state.",
		Builtin:     true,
		// Read-only on purpose: admin:ProbeNetwork and admin:ScanNetwork
		// make the server open outbound connections and are not granted.
		Document: PolicyDocument{
			Version: PolicyVersion2012,
			Statement: []Statement{{
				Sid:      "Diagnostics",
				Effect:   "Allow",
				Action:   StringOrSlice{"admin:ServerInfo", "admin:GetStats", "admin:GetDiscovery", "admin:ListBackings", "admin:GetBucketPlacement"},
				Resource: StringOrSlice{AdminResourceARN},
			}},
		},
	},
}

// copyPolicy returns a deep copy of p.
func copyPolicy(p *NamedPolicy) NamedPolicy {
	out := *p
	out.Document = cloneDocument(p.Document)
	return out
}
