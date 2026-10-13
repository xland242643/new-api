package dto_test

import (
	"encoding/json"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
)

func TestCacheCreationUsageCompatibility(t *testing.T) {
	decoders := map[string]func([]byte, any) error{"stdlib": json.Unmarshal, "runtime": common.Unmarshal}
	for decoderName, decode := range decoders {
		t.Run(decoderName, func(t *testing.T) {
			for _, test := range []struct {
				name, fields string
				want         int
				invalid      bool
			}{
				{"canonical", `"cached_creation_tokens":300`, 300, false},
				{"aliyun", `"cache_creation_input_tokens":300`, 300, false},
				{"same-not-added-twice", `"cached_creation_tokens":300,"cache_creation_input_tokens":300`, 300, false},
				{"zero", `"cache_creation_input_tokens":0`, 0, false},
				{"absent", `"text_tokens":800`, 0, false},
				{"conflict", `"cached_creation_tokens":200,"cache_creation_input_tokens":300`, 0, true},
				{"negative", `"cache_creation_input_tokens":-1`, 0, true},
				{"invalid-type", `"cache_creation_input_tokens":"300"`, 0, true},
			} {
				t.Run(test.name, func(t *testing.T) {
					payload := []byte(`{"prompt_tokens":1000,"completion_tokens":10,"prompt_tokens_details":{"cached_tokens":200,"audio_tokens":20,` + test.fields + `}}`)
					var usage dto.Usage
					err := decode(payload, &usage)
					if test.invalid {
						if err == nil {
							t.Fatal("ambiguous/invalid usage must not be settled")
						}
						return
					}
					if err != nil {
						t.Fatal(err)
					}
					if usage.PromptTokensDetails.CachedCreationTokens != test.want || usage.PromptTokensDetails.CachedTokens != 200 || usage.PromptTokensDetails.AudioTokens != 20 || usage.PromptTokens != 1000 || usage.CompletionTokens != 10 {
						t.Fatal("normalization lost, duplicated or changed usage")
					}
				})
			}
		})
	}
}
