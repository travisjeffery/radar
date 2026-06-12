package radar

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDecisionJSONRoundTrip(t *testing.T) {
	for _, dec := range []Decision{
		DecisionRouteToHuman, DecisionNotEligible, DecisionBlanketAutoAccept,
		DecisionAutoLand, DecisionVerificationPassed, DecisionRADARApproved,
	} {
		b, err := json.Marshal(dec)
		if err != nil {
			t.Fatal(err)
		}
		if want := `"` + dec.String() + `"`; string(b) != want {
			t.Fatalf("Marshal(%s) = %s, want %s", dec, b, want)
		}
		var got Decision
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatal(err)
		}
		if got != dec {
			t.Fatalf("round trip = %s, want %s", got, dec)
		}
	}
}

func TestDecisionTraceJSONUsesNames(t *testing.T) {
	b, err := json.Marshal(&DecisionTrace{DiffID: "D1", Decision: DecisionRouteToHuman})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"decision":"route-to-human"`) {
		t.Fatalf("trace JSON should name the decision, got %s", b)
	}
}

func TestDecisionUnmarshalUnknown(t *testing.T) {
	var d Decision
	if err := json.Unmarshal([]byte(`"nonsense"`), &d); err == nil {
		t.Fatal("expected error for unknown decision name")
	}
}
