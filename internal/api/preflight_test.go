package api

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestPreflightReturnsTheCheckTable(t *testing.T) {
	h := newHarness(t)

	resp := h.do("GET", "/v1/preflight", hostToken, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var body struct {
		Checks []struct {
			Name     string `json:"name"`
			OK       bool   `json:"ok"`
			Severity string `json:"severity"`
		} `json:"checks"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Checks) == 0 {
		t.Fatal("preflight must return checks")
	}
	for _, c := range body.Checks {
		if c.Name == "" || c.Severity == "" {
			t.Errorf("every check needs a name and a severity: %+v", c)
		}
	}
}
