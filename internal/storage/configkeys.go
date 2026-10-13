package storage

// AllConfigKeys lists every bucket configuration document key (the Config*
// constants). Code that copies or removes all of a bucket's configuration
// (forced bucket deletes, replication between backings) iterates it, so a
// new Config* constant must be added here too.
var AllConfigKeys = []string{
	ConfigPolicy, ConfigCORS, ConfigLifecycle, ConfigTagging,
	ConfigEncryption, ConfigObjectLock, ConfigACL, ConfigWebsite,
	ConfigOwnership, ConfigNotification, ConfigLogging,
	ConfigPublicAccessBlock, ConfigReplication, ConfigAccelerate,
	ConfigRequestPayment, ConfigCompression,
}
