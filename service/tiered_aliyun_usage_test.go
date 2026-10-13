package service

import (
	"encoding/json"
	"testing"

	"github.com/QuantumNous/new-api/dto"
)

func TestAliyunCacheCreationTieredNormalization(t *testing.T) {
	var usage dto.Usage
	if err := json.Unmarshal([]byte(`{"prompt_tokens":1000,"completion_tokens":10,"prompt_tokens_details":{"cached_tokens":200,"cache_creation_input_tokens":300}}`), &usage); err != nil {
		t.Fatal(err)
	}
	params := BuildTieredTokenParams(&usage, false, map[string]bool{"cr": true, "cc": true})
	if params.P != 500 || params.CR != 200 || params.CC != 300 || params.Len != 1000 || params.C != 10 {
		t.Fatalf("cache categories must be excluded once while context length stays complete: %+v", params)
	}
	// Expressions that do not price cache separately retain the full prompt.
	plain := BuildTieredTokenParams(&usage, false, nil)
	if plain.P != 1000 || plain.Len != 1000 {
		t.Fatal("normalization changed the native fallback contract")
	}
}
