package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/RhyChaw/aurium/internal/gateway"
)

// decideViaDaemon approves or rejects through the daemon's API.
//
// Approving a held call executes it, and only the daemon has the connected
// upstream and its credential. Deciding in the CLI process would mark the
// approval decided and then report "integration is no longer connected" —
// leaving the human believing they approved something that never ran.
func decideViaDaemon(approvalID, decision string) (gateway.Approval, error) {
	if err := StartDaemon(""); err != nil {
		return gateway.Approval{}, fmt.Errorf(
			"approving runs the held call, which needs the daemon: %w", err)
	}
	token, err := readHostToken()
	if err != nil {
		return gateway.Approval{}, err
	}

	body, _ := json.Marshal(map[string]string{"decision": decision, "by": "human"})
	req, err := http.NewRequest("POST",
		"http://"+daemonAddr()+"/v1/approvals/"+approvalID+"/decide", bytes.NewReader(body))
	if err != nil {
		return gateway.Approval{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	// Generous: the held call runs inside this request, and a real upstream
	// (opening a PR, merging) can take a while.
	client := &http.Client{Timeout: 120 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		return gateway.Approval{}, err
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		json.NewDecoder(res.Body).Decode(&e)
		if e.Error == "" {
			e.Error = res.Status
		}
		return gateway.Approval{}, fmt.Errorf("%s", e.Error)
	}

	var ap gateway.Approval
	if err := json.NewDecoder(res.Body).Decode(&ap); err != nil {
		return gateway.Approval{}, err
	}
	return ap, nil
}
