package mcpreg

import "errors"

// This file holds the stubs of the M1b registry API. Whoever implements
// one of them moves it out of this file; the file goes away when it is empty.

// InspectResult is the load result of one registration.
type InspectResult struct {
	Name  string
	Entry *Entry
	Err   error
}

// RegisterDefinition registers a definition built in memory.
func RegisterDefinition(s Scope, def *Definition, h []string) (*Entry, error) {
	return nil, errors.New("not implemented")
}

// Update changes the definition of a registered server in place.
func Update(s Scope, name string, fn func(*Definition) error) (*Entry, error) {
	return nil, errors.New("not implemented")
}

// Inspect loads every registration of the scope and reports each result,
// so one corrupt registration does not hide the others.
func Inspect(s Scope) []InspectResult {
	return nil
}

// FromServerEntry builds a definition from one harness MCP server entry.
func FromServerEntry(name string, raw []byte) (*Definition, error) {
	return nil, errors.New("not implemented")
}
