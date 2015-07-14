package udf

import (
	"fmt"
	"sync"
)

// UDSFCreatorRegistry manages creators of UDSFs.
type UDSFCreatorRegistry interface {
	// Register adds a UDSF creator to the registry. It returns an error if
	// the type name is already registered.
	Register(typeName string, c UDSFCreator) error

	// Lookup returns a UDSF creator having the type name. It returns an error
	// if it doesn't have the creator.
	Lookup(typeName string, arity int) (UDSFCreator, error)

	// List returns all creators the registry has. The caller can safely modify
	// the map returned from this method.
	List() (map[string]UDSFCreator, error)

	// Unregister removes a creator from the registry. It doesn't return error
	// when the registry doesn't have a creator having the type name.
	//
	// The registry itself doesn't support cascading delete. It should properly
	// done by the caller.
	Unregister(typeName string) error
}

type defaultUDSFCreatorRegistry struct {
	m        sync.RWMutex
	creators map[string]UDSFCreator
}

// NewDefaultUDSFCreatorRegistry returns a UDSFCreatorRegistry having a
// default implementation.
func NewDefaultUDSFCreatorRegistry() UDSFCreatorRegistry {
	return &defaultUDSFCreatorRegistry{
		creators: map[string]UDSFCreator{},
	}
}

func (r *defaultUDSFCreatorRegistry) Register(typeName string, c UDSFCreator) error {
	r.m.Lock()
	defer r.m.Unlock()

	if _, ok := r.creators[typeName]; ok {
		return fmt.Errorf("a UDSF type '%v' is already registered", typeName)
	}
	r.creators[typeName] = c
	return nil
}

func (r *defaultUDSFCreatorRegistry) Lookup(typeName string, arity int) (UDSFCreator, error) {
	r.m.RLock()
	defer r.m.RUnlock()
	c, ok := r.creators[typeName]
	if !ok {
		return nil, fmt.Errorf("a UDSF type '%v' is not registered", typeName)
	}
	if !c.Accept(arity) {
		return nil, fmt.Errorf("a UDSF type '%v' doesn't accept the given arity: %v", typeName, arity)
	}
	return c, nil
}

func (r *defaultUDSFCreatorRegistry) List() (map[string]UDSFCreator, error) {
	r.m.RLock()
	defer r.m.RUnlock()

	m := make(map[string]UDSFCreator, len(r.creators))
	for t, c := range r.creators {
		m[t] = c
	}
	return m, nil
}

func (r *defaultUDSFCreatorRegistry) Unregister(typeName string) error {
	r.m.Lock()
	defer r.m.Unlock()
	delete(r.creators, typeName)
	return nil
}

var (
	globalUDSFCreatorRegistry = NewDefaultUDSFCreatorRegistry()
)

// RegisterGlobalUDSFCreator adds a UDSFCreator which can be referred from
// alltopologies. UDSFCreators registered after running topologies might not
// be seen by those topologies. Call it from init functions to avoid such
// conditions.
func RegisterGlobalUDSFCreator(typeName string, c UDSFCreator) error {
	return globalUDSFCreatorRegistry.Register(typeName, c)
}

// MustRegisterGlobalUDSFCreator is like RegisterGlobalUDSFCreator
// but panics if the type is already registered.
func MustRegisterGlobalUDSFCreator(typeName string, c UDSFCreator) {
	if err := globalUDSFCreatorRegistry.Register(typeName, c); err != nil {
		panic(fmt.Errorf("udf.MustRegisterGlobalUDSFCreator: cannot register '%v': %v", typeName, err))
	}
}

// CopyGlobalUDSFCreatorRegistry creates a new independent copy of the global
// UDSFCreatorRegistry.
func CopyGlobalUDSFCreatorRegistry() (UDSFCreatorRegistry, error) {
	r := NewDefaultUDSFCreatorRegistry()
	m, err := globalUDSFCreatorRegistry.List()
	if err != nil {
		return nil, err
	}

	for t, c := range m {
		if err := r.Register(t, c); err != nil {
			return nil, err
		}
	}
	return r, nil
}
