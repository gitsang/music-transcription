package core

import (
	"encoding/json"
	"log/slog"
)

// JSONValue prepares a value for a GORM map update on a JSON column. A raw Go
// slice/map cannot be used directly: GORM's Update with a map does not run the
// field serializer, so an empty slice would be sent as SQL NULL and violate the
// NOT NULL constraint. Marshalling to a string lets PostgreSQL cast it to json.
func JSONValue(value any) any {
	if value == nil {
		// A JSON column distinguishes NULL from `null`; settings use the latter
		// to mean "no retention limit".
		return "null"
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		slog.Error("encode json column", "error", err)
		return nil
	}
	return string(encoded)
}
