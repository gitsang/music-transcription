// Package util converts topics/labels into Redis stream names.
//
// The package has no dependencies outside the standard library.
package util

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"sort"
	"strings"
)

// SortLabels will sort and unique labels
func SortLabels(labels []string) []string {
	uniqueMap := make(map[string]struct{})
	for _, value := range labels {
		uniqueMap[value] = struct{}{}
	}

	uniqueSlice := make([]string, 0, len(uniqueMap))
	for key := range uniqueMap {
		uniqueSlice = append(uniqueSlice, key)
	}

	sort.Strings(uniqueSlice)

	return uniqueSlice
}

// LabelsHash will generate sha256 hash of labels
func LabelsHash(labels []string) string {
	sortedLabels := SortLabels(labels)

	hash := sha256.New()
	for _, value := range sortedLabels {
		hash.Write([]byte(value))
	}

	return hex.EncodeToString(hash.Sum(nil))
}

// LabelsBase64 will generate base64 string of labels
func LabelsBase64(labels []string) string {
	sortedLabels := SortLabels(labels)
	labelsStr := strings.Join(sortedLabels, ",")
	return base64.StdEncoding.EncodeToString([]byte(labelsStr))
}

// GenRealTopicWithLabels will generate real topic join with labels hash
func GenRealTopicWithLabels(topic string, labels []string) string {
	if len(labels) == 0 {
		return topic
	}

	return strings.Join([]string{topic, LabelsBase64(labels)}, ":")
}

// fullCombine returns every subset of items, preserving input order.
func fullCombine[T any](items []T) [][]T {
	results := make([][]T, 0, 1<<min(len(items), 10))
	var backtrack func(index int, subset []T)
	backtrack = func(index int, subset []T) {
		results = append(results, append([]T{}, subset...))
		for i := index; i < len(items); i++ {
			backtrack(i+1, append(subset, items[i]))
		}
	}
	backtrack(0, nil)
	return results
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// GenTopicCombineWithLabels will generate topics join with labels full combinations hash
func GenTopicCombineWithLabels(topics []string, labels []string) []string {
	labelsCombs := fullCombine(labels)
	realTopics := make([]string, 0, len(labelsCombs)*len(topics))

	for _, topic := range topics {
		for _, labelsComb := range labelsCombs {
			if len(labelsComb) == 0 {
				realTopics = append(realTopics, topic)
				continue
			}
			realTopics = append(realTopics,
				strings.Join([]string{topic, LabelsBase64(labelsComb)}, ":"),
			)
		}
	}

	return realTopics
}
