package cli

import "testing"

// The token is the last thing these commands print. Anything before it is
// progress the command chose to put on stdout rather than stderr, and treating
// the whole buffer as the credential would store a paragraph.
func TestLastNonEmptyLineTakesTheToken(t *testing.T) {
	cases := map[string]string{
		"sk-ant-oat01-ABC": "sk-ant-oat01-ABC",
		"Opening browser…\nAuthorised.\n\nsk-ant-oat01-ABC\n": "sk-ant-oat01-ABC",
		"sk-ant-oat01-ABC\n\n\n":                              "sk-ant-oat01-ABC",
		"  sk-ant-oat01-ABC  \n":                              "sk-ant-oat01-ABC",
		"":                                                    "",
		"\n\n":                                                "",
	}
	for in, want := range cases {
		if got := lastNonEmptyLine(in); got != want {
			t.Errorf("lastNonEmptyLine(%q) = %q, want %q", in, got, want)
		}
	}
}
