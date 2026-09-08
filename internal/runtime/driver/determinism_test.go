package driver

import (
	"strings"
	"testing"
)

// Go randomises map iteration, so label ordering must be forced. Without this
// the -v output differs run to run and argv assertions go flaky.
func TestCreateArgsAreDeterministic(t *testing.T) {
	first := strings.Join(createArgs(specFixture()), " ")
	for i := 0; i < 50; i++ {
		if got := strings.Join(createArgs(specFixture()), " "); got != first {
			t.Fatalf("createArgs is not deterministic:\n%s\nvs\n%s", first, got)
		}
	}
}
