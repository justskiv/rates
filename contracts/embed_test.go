package contracts_test

import (
	"encoding/json"
	"testing"

	"github.com/justskiv/rates/contracts"
)

func TestVerify(t *testing.T) {
	if err := contracts.Verify(); err != nil {
		t.Fatal(err)
	}
}

func TestContractHasSourceRevision(t *testing.T) {
	var c struct {
		GeneratedFrom string `json:"generatedFrom"`
	}
	if err := json.Unmarshal(contracts.Contract(), &c); err != nil {
		t.Fatal(err)
	}
	if c.GeneratedFrom == "" {
		t.Error("generatedFrom is empty: the export did not record its source")
	}
}
