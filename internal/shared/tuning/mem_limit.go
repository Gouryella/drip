package tuning

import (
	"os"
	"strconv"
	"strings"
)

func constrainMemory(total uint64, limitFiles ...string) uint64 {
	for _, path := range limitFiles {
		data, err := os.ReadFile(path) // #nosec G304 -- only fixed kernel cgroup paths are passed by production callers.
		if err != nil {
			continue
		}
		limit, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
		if err == nil && limit > 0 && limit < total {
			total = limit
		}
	}
	return total
}
