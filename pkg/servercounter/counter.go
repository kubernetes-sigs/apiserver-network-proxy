// Package servercounter provides utilities for getting the current count of
// proxy servers.
package servercounter

// A ServerCounter counts the number of available proxy servers.
type ServerCounter interface {
	CountServers() int
}

// A StaticServerCounter stores a static server count.
type StaticServerCounter int

// CountServers returns the current (static) proxy server count.
func (sc StaticServerCounter) CountServers() int {
	return int(sc)
}
