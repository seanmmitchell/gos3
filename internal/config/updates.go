package config

// UpdatesConfig controls optional server-side release checks. An empty
// NotifyFor list deliberately disables notices while still checking.
type UpdatesConfig struct {
	Enabled   bool     `yaml:"enabled" json:"enabled"`
	NotifyFor []string `yaml:"notify_for" json:"notifyFor"`
}
