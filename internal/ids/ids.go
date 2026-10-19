// Package ids produces sortable, typed identifiers.
package ids

import (
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// New returns a typed identifier such as m_018f... using a UUIDv7.
func New(prefix string) string {
	return prefix + "_" + uuid.Must(uuid.NewV7()).String()
}

// NewUUID returns a bare UUIDv7 string.
func NewUUID() string {
	return uuid.Must(uuid.NewV7()).String()
}

// UUIDPart returns the bare UUID portion of a typed identifier.
func UUIDPart(id string) (string, error) {
	i := strings.IndexByte(id, '_')
	if i <= 0 || i == len(id)-1 {
		return "", fmt.Errorf("identifier %q has no typed prefix", id)
	}
	u := id[i+1:]
	if _, err := uuid.Parse(u); err != nil {
		return "", fmt.Errorf("identifier %q does not contain a valid UUID", id)
	}
	return u, nil
}

// Validate checks that id is a valid UUID-bearing typed identifier.
func Validate(id string) error {
	_, err := UUIDPart(id)
	return err
}
