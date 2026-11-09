package catalog

import (
	"context"

	"github.com/Guan0923/xinghe_rag/internal/state"
)

// StateStore adapts the shared-database CatalogRepository to the catalog.Store
// interface. The state layer owns locking/transactions; this adapter only
// translates raw JSON.
type StateStore struct {
	Repository *state.CatalogRepository
}

// Load implements Store.
func (s StateStore) Load(ctx context.Context, namespace string) ([]byte, error) {
	raw, found, err := s.Repository.Load(ctx, namespace)
	if err != nil {
		return nil, ErrConfigurationRead
	}
	if !found {
		return nil, nil
	}
	return []byte(raw), nil
}

// Mutate implements Store.
func (s StateStore) Mutate(ctx context.Context, namespace string, transform func(*Data) (bool, error)) (*Data, error) {
	result, err := s.Repository.Mutate(ctx, namespace, func(current *string) (string, any, bool, error) {
		data := NewData()
		if current != nil && *current != "" {
			decoded, decodeErr := Decode([]byte(*current))
			if decodeErr != nil {
				return "", nil, false, ErrConfigurationRead
			}
			data = decoded
		}
		changed, transformErr := transform(data)
		if transformErr != nil {
			return "", nil, false, transformErr
		}
		if !changed {
			return "", nil, false, nil
		}
		encoded, encodeErr := Encode(data)
		if encodeErr != nil {
			return "", nil, false, ErrConfigurationWrite
		}
		return string(encoded), data, true, nil
	})
	if err != nil {
		if _, ok := err.(*Error); ok {
			return nil, err
		}
		return nil, ErrConfigurationWrite
	}
	data, ok := result.(*Data)
	if !ok || data == nil {
		loaded, loadErr := s.Load(ctx, namespace)
		if loadErr != nil {
			return nil, loadErr
		}
		if loaded == nil {
			return NewData(), nil
		}
		return Decode(loaded)
	}
	return data, nil
}
