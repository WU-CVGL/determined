package config

// ResourcePoolAccessConfig configures what users see of the resource pools that they may not use.
type ResourcePoolAccessConfig struct {
	// HideUnusablePools leaves the pools that a user may not use out of the user's resource pool
	// list. When it is false, the list shows them with usable false.
	HideUnusablePools bool `json:"hide_unusable_pools"`
}
