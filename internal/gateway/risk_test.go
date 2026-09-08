package gateway

import "testing"

func TestClassifyRisk(t *testing.T) {
	cases := map[string]Risk{
		// Reads.
		"get_repository": RiskLow,
		"list_issues":    RiskLow,
		"search_code":    RiskLow,
		"read_file":      RiskLow,
		"getPullRequest": RiskLow,
		"repo.get":       RiskLow,

		// Recoverable changes.
		"create_pull_request": RiskMedium,
		"update_issue":        RiskMedium,
		"add_comment":         RiskMedium,
		"push_files":          RiskMedium,

		// Irreversible or publishing.
		"delete_repository":   RiskHigh,
		"merge_pull_request":  RiskHigh,
		"force_push":          RiskHigh,
		"remove_collaborator": RiskHigh,
		"drop_database":       RiskHigh,

		// High wins when verbs collide: the destructive part is what matters.
		"delete_and_list_branches": RiskHigh,
	}
	for name, want := range cases {
		if got := ClassifyRisk(name); got != want {
			t.Errorf("ClassifyRisk(%q) = %q, want %q", name, got, want)
		}
	}
}

// An unrecognised capability must never be classified low. Guessing low on
// something destructive hands an agent an irreversible action unsupervised;
// guessing high on something harmless costs one click.
func TestUnknownCapabilitiesAreNotLow(t *testing.T) {
	for _, name := range []string{"frobnicate", "xyzzy", "do_the_thing", ""} {
		if got := ClassifyRisk(name); got == RiskLow {
			t.Errorf("ClassifyRisk(%q) = low; an unknown capability must not be treated as a read", name)
		}
	}
}

// Substring matching would misclassify: "budget" contains "get".
func TestVerbsMatchWholeWordsOnly(t *testing.T) {
	if got := ClassifyRisk("budget_report"); got == RiskLow {
		t.Error(`"budget_report" must not match the verb "get"`)
	}
	if got := ClassifyRisk("undelete_comment"); got != RiskMedium {
		// "undelete" is its own word and is not "delete".
		t.Errorf(`ClassifyRisk("undelete_comment") = %q`, got)
	}
}

func TestSplitIdentifier(t *testing.T) {
	cases := map[string][]string{
		"create_pull_request": {"create", "pull", "request"},
		"getPullRequest":      {"get", "pull", "request"},
		"repo.get":            {"repo", "get"},
		"list-issues":         {"list", "issues"},
	}
	for in, want := range cases {
		got := splitIdentifier(in)
		if len(got) != len(want) {
			t.Errorf("splitIdentifier(%q) = %v, want %v", in, got, want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("splitIdentifier(%q) = %v, want %v", in, got, want)
				break
			}
		}
	}
}
