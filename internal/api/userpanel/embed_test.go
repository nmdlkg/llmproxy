package userpanel

import (
	"bytes"
	"testing"
)

func TestEmbeddedDashboardContainsRequiredUserFlows(t *testing.T) {
	page := HTML()
	for _, required := range [][]byte{
		[]byte("/v0/user"),
		[]byte("/credentials"),
		[]byte("/usage"),
		[]byte("/api-keys"),
		[]byte("Authorization"),
	} {
		if !bytes.Contains(page, required) {
			t.Fatalf("dashboard is missing %q", required)
		}
	}
	if bytes.Contains(page, []byte("https://")) || bytes.Contains(page, []byte("<script src=")) {
		t.Fatal("dashboard contains an external dependency")
	}
}
