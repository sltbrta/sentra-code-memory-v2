package hosted

import (
	"os"
	"strconv"
	"strings"
)

func getEnvRaw(k string) string {
	return os.Getenv(k)
}

func parseFloat(s string, f *float64) (int, error) {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0, err
	}
	*f = v
	return 1, nil
}
