package util

import (
	"fmt"
	"strings"
)

// ParseLabels takes a comma-separated string of key-value pairs and returns a map of labels.
func ParseLabels(labelStr string) (map[string]string, error) {
	labels := make(map[string]string)

	if len(labelStr) == 0 {
		return labels, fmt.Errorf("empty string provided")
	}
	pairs := strings.Split(labelStr, ",")

	for _, pair := range pairs {
		keyValue := strings.Split(pair, "=")
		if len(keyValue) != 2 {
			return nil, fmt.Errorf("invalid label format: %s", pair)
		}
		key := strings.TrimSpace(keyValue[0])
		value := strings.TrimSpace(keyValue[1])
		labels[key] = value
	}
	return labels, nil
}
