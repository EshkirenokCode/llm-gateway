// Package tenant defines the configuration shared by startup and access control.
package tenant

// Policy grants an application access to the gateway with process-local limits.
type Policy struct {
	Name        string `json:"name"`
	Key         string `json:"key"`
	RPM         int    `json:"rpm"`
	Concurrency int    `json:"concurrency"`
}
