package tests

import (
	"strings"
	"testing"

	telegram "justsay-harness/integrations/telegram"
)

// TestApprovalDataFits pins the Bot API's 64-byte limit on a button payload.
// Two whole UUIDs only fit packed, so a longer decision name or an id carried
// as it reads would break this silently otherwise.
func TestApprovalDataFits(t *testing.T) {
	const (
		session = "9f3ca21b-4e77-4c1a-9c3d-2b8e1a0f4d21"
		pending = "7e4b1a02-1111-2222-3333-444455556666"
	)

	for _, decision := range []string{telegram.DecisionApprove, telegram.DecisionDecline} {
		data := telegram.ApprovalData(decision, session, pending)
		if len(data) > telegram.MaxCallbackDataBytes {
			t.Fatalf("%q is %d bytes, over the %d allowed", data, len(data), telegram.MaxCallbackDataBytes)
		}

		gotDecision, gotSession, gotPending, ok := telegram.ParseApproval(data)
		if !ok || gotDecision != decision || gotSession != session || gotPending != pending {
			t.Fatalf("round trip gave %q %q %q (ok %v)", gotDecision, gotSession, gotPending, ok)
		}
	}
}

// TestParseApprovalRejects covers the payloads a tap must not be acted on.
func TestParseApprovalRejects(t *testing.T) {
	for _, data := range []string{"", "nope:session", "approve:", "approve", ":::"} {
		if _, _, _, ok := telegram.ParseApproval(data); ok {
			t.Errorf("%q should not parse", data)
		}
	}
}

// A payload minted before the pause id existed still names its session, and an
// id written out rather than packed still reads back as itself.
func TestParseApprovalWithoutPending(t *testing.T) {
	const session = "9f3ca21b-4e77-4c1a-9c3d-2b8e1a0f4d21"

	decision, got, pending, ok := telegram.ParseApproval(telegram.DecisionApprove + ":" + session)
	if !ok || decision != telegram.DecisionApprove || got != session || pending != "" {
		t.Fatalf("gave %q %q %q (ok %v)", decision, got, pending, ok)
	}
}

// TestSettledRowIsInert pins what makes a decided request's buttons dead: they
// answer with DecisionSettled, which ParseApproval refuses. Only the button
// that was tapped is marked, and the mark says which answer it was.
func TestSettledRowIsInert(t *testing.T) {
	marks := map[string]string{
		telegram.DecisionApprove: "✅",
		telegram.DecisionDecline: "❌",
	}

	for decision, mark := range marks {
		marked := 0

		for _, button := range telegram.SettledRow(decision) {
			if button.Data != telegram.DecisionSettled {
				t.Errorf("%q still carries %q", button.Text, button.Data)
			}
			if _, _, _, ok := telegram.ParseApproval(button.Data); ok {
				t.Errorf("%q parses as a decision", button.Data)
			}
			if strings.HasPrefix(button.Text, mark) {
				marked++
			}
		}

		if marked != 1 {
			t.Errorf("%s marked %d buttons with %s, want 1", decision, marked, mark)
		}
	}
}
