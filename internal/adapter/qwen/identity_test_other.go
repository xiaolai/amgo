//go:build !linux

package qwen

import "encoding/json"

func testIdentity() (*string, json.RawMessage) { return nil, json.RawMessage("null") }
