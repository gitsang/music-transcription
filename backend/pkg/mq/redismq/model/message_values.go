// Package model holds the Redis stream field layout for one message.
//
// It is defined in this repository for local use.
package model

import "encoding/json"

type MessageValues struct {
	Topic   string `json:"topic"`
	Labels  string `json:"labels"`
	Content string `json:"content"`
}

func (v MessageValues) String() string {
	jsonBytes, _ := json.Marshal(v)
	return string(jsonBytes)
}

func (v MessageValues) ToMap() map[string]any {
	jsonBytes, _ := json.Marshal(v)
	var result map[string]any
	_ = json.Unmarshal(jsonBytes, &result)
	return result
}

func (v *MessageValues) ParseJson(in []byte) error {
	return json.Unmarshal(in, v)
}

func (v *MessageValues) ParseMap(in map[string]any) error {
	jsonBytes, _ := json.Marshal(in)
	return v.ParseJson(jsonBytes)
}
