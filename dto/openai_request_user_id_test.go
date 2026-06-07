package dto

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestGeneralOpenAIRequestPreservesUserID(t *testing.T) {
	var request GeneralOpenAIRequest
	if err := json.Unmarshal([]byte(`{"model":"deepseek-chat","user_id":"user-123"}`), &request); err != nil {
		t.Fatal(err)
	}

	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"user_id":"user-123"`) {
		t.Fatalf("user_id was not preserved: %s", body)
	}
}
