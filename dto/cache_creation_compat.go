package dto

import (
	"encoding/json"
	"errors"
)

// UnmarshalJSON preserves the native usage shape while accepting the documented
// Aliyun OpenAI-compatible cache-creation field. Both are subsets of input
// tokens; settlement and subtraction remain owned by the native billing code.
// https://help.aliyun.com/zh/model-studio/context-cache
func (details *InputTokenDetails) UnmarshalJSON(data []byte) error {
	type nativeDetails InputTokenDetails
	var native nativeDetails
	if err := json.Unmarshal(data, &native); err != nil {
		return err
	}
	var aliases struct {
		Canonical *int `json:"cached_creation_tokens"`
		Aliyun    *int `json:"cache_creation_input_tokens"`
	}
	if err := json.Unmarshal(data, &aliases); err != nil {
		return err
	}
	if aliases.Aliyun != nil {
		if *aliases.Aliyun < 0 || (aliases.Canonical != nil && *aliases.Canonical != *aliases.Aliyun) {
			return errors.New("invalid or conflicting cache creation token counts")
		}
		native.CachedCreationTokens = *aliases.Aliyun
	}
	*details = InputTokenDetails(native)
	return nil
}
