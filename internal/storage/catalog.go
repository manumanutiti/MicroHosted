package storage

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"

	"microhosted/pkg/types"
)

// Catalog is the read side of the template library. It's backed by a JSON
// file today; the interface is small enough to swap for a SQLite-backed
// store later (Sesión 8) without touching callers.
type Catalog struct {
	mu        sync.RWMutex
	templates map[string]types.Template
}

// LoadCatalog reads a catalog from a JSON file containing a list of templates.
func LoadCatalog(path string) (*Catalog, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading catalog %s: %w", path, err)
	}

	var list []types.Template
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("parsing catalog %s: %w", path, err)
	}

	templates := make(map[string]types.Template, len(list))
	for _, t := range list {
		templates[t.Name] = t
	}

	return &Catalog{templates: templates}, nil
}

// Get returns the named template, or an error if it isn't in the catalog.
func (c *Catalog) Get(name string) (types.Template, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	t, ok := c.templates[name]
	if !ok {
		return types.Template{}, fmt.Errorf("template %q not found in catalog", name)
	}
	return t, nil
}

// List returns every template in the catalog.
func (c *Catalog) List() []types.Template {
	c.mu.RLock()
	defer c.mu.RUnlock()

	list := make([]types.Template, 0, len(c.templates))
	for _, t := range c.templates {
		list = append(list, t)
	}
	return list
}
