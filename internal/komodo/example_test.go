package komodo

import (
	"os"
	"testing"
)

// The example file in examples/ stays valid.
func TestExampleFile(t *testing.T) {
	b, err := os.ReadFile("../../examples/remediation.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseActions(b); err != nil {
		t.Fatal(err)
	}
}
