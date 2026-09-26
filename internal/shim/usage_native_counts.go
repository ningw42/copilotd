package shim

import (
	"encoding/json"
	"strings"

	"github.com/ningw42/copilotd/internal/usage"
)

// decodeNativeCounts reads one nullable value per declared count, in
// declaration order, from a provider usage object. A missing or null count or
// detail container is unreported. Any present count that is not a
// non-negative base-10 int64, or any present non-null container that is not an
// object, rejects the whole object. Required counts are checked only when a
// caller converts a completed candidate.
func decodeNativeCounts[U usage.Usage](projection usage.Projection[U], object map[string]json.RawMessage) ([]*int64, bool) {
	counts := make([]*int64, projection.Len())
	for index, count := range projection.All() {
		value, ok := decodeNativeCount(object, count.Path())
		if !ok {
			return nil, false
		}
		counts[index] = value
	}
	return counts, true
}

// decodeNativeUsage decodes a completed candidate's usage object into typed
// usage; it fails when any count is invalid or a required count is unreported.
func decodeNativeUsage[U usage.Usage](projection usage.Projection[U], object map[string]json.RawMessage) (U, bool) {
	counts, ok := decodeNativeCounts(projection, object)
	if !ok {
		var zero U
		return zero, false
	}
	native, err := projection.Usage(counts)
	return native, err == nil
}

func decodeNativeCount(object map[string]json.RawMessage, path string) (*int64, bool) {
	for {
		container, rest, nested := strings.Cut(path, ".")
		if !nested {
			return optionalNonnegativeInt64(object, path)
		}
		details, present, valid := optionalJSONObject(object, container)
		if !valid || !present {
			return nil, valid
		}
		object, path = details, rest
	}
}
