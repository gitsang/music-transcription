package util_test

import (
	"fmt"
	"reflect"
	"sort"
	"testing"

	"github.com/gitsang/music-transcription/backend/pkg/mq/redismq/util"
)

func TestSortLabels(t *testing.T) {
	tests := []struct {
		labels []string
		want   []string
	}{
		{labels: []string{}, want: []string{}},
		{labels: []string{"b", "a"}, want: []string{"a", "b"}},
		{labels: []string{"b", "c", "a"}, want: []string{"a", "b", "c"}},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("SortLabels(%s)", tt.labels), func(t *testing.T) {
			got := util.SortLabels(tt.labels)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("SortLabels() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestLabelsBase64(t *testing.T) {
	tests := []struct {
		labels []string
		want   string
	}{
		{labels: []string{}, want: ""},
		{labels: []string{"a"}, want: "YQ=="},
		{labels: []string{"b"}, want: "Yg=="},
		{labels: []string{"a", "b", "c"}, want: "YSxiLGM="},
		{labels: []string{"b", "a", "c"}, want: "YSxiLGM="},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("LabelsBase64(%s)", tt.labels), func(t *testing.T) {
			if got := util.LabelsBase64(tt.labels); got != tt.want {
				t.Errorf("LabelsBase64() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGenRealTopicWithLabels(t *testing.T) {
	tests := []struct {
		topic  string
		labels []string
		want   string
	}{
		{topic: "topic", labels: []string{}, want: "topic"},
		{topic: "topic", labels: []string{"a"}, want: "topic:YQ=="},
		{topic: "topic", labels: []string{"b"}, want: "topic:Yg=="},
		{topic: "topic", labels: []string{"a", "b", "c"}, want: "topic:YSxiLGM="},
		{topic: "topic", labels: []string{"b", "a", "c"}, want: "topic:YSxiLGM="},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("GenRealTopicWithLabels(%s, %s)", tt.topic, tt.labels), func(t *testing.T) {
			if got := util.GenRealTopicWithLabels(tt.topic, tt.labels); got != tt.want {
				t.Errorf("GenRealTopicWithLabels() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGenTopicCombineWithLabels(t *testing.T) {
	tests := []struct {
		topics []string
		labels []string
		want   []string
	}{
		{
			topics: []string{"topic"},
			labels: []string{},
			want:   []string{"topic"},
		},
		{
			topics: []string{"topic"},
			labels: []string{"a", "b"},
			want: []string{
				"topic",
				"topic:YQ==",
				"topic:Yg==",
				"topic:YSxi",
			},
		},
		{
			topics: []string{"topic1", "topic2"},
			labels: []string{"a", "b"},
			want: []string{
				"topic1",
				"topic1:YQ==",
				"topic1:Yg==",
				"topic1:YSxi",
				"topic2",
				"topic2:YQ==",
				"topic2:Yg==",
				"topic2:YSxi",
			},
		},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("GenTopicCombineWithLabels(%s, %s)", tt.topics, tt.labels), func(t *testing.T) {
			got := util.GenTopicCombineWithLabels(tt.topics, tt.labels)
			sort.Strings(got)
			sort.Strings(tt.want)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("GenTopicCombineWithLabels() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestLabelsHashStable(t *testing.T) {
	a := util.LabelsHash([]string{"b", "a"})
	b := util.LabelsHash([]string{"a", "b", "a"})
	if a != b {
		t.Fatalf("LabelsHash must be order/duplicate insensitive: %s != %s", a, b)
	}
	if a == util.LabelsHash([]string{"a"}) {
		t.Fatal("LabelsHash collided for different label sets")
	}
}
