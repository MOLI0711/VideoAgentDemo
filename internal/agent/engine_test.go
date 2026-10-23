package agent

import "testing"

func TestAnswerCannotAddUncitedTextOrUnknownEvidence(t *testing.T) {
	known := map[string]Evidence{"segment:1": {ID: "segment:1", Kind: "transcript", StartMS: 1, EndMS: 10}}
	for _, raw := range []string{`{"status":"answered","answer":"unverified","claims":[{"text":"x","evidence_ids":["segment:1"]}]}`, `{"status":"answered","claims":[{"text":"x","evidence_ids":["segment:2"]}]}`, `{"status":"answered","claims":[]}`} {
		if _, err := Validate(raw, known); err == nil {
			t.Fatal("accepted invalid answer", raw)
		}
	}
	res, err := Validate(`{"status":"answered","claims":[{"text":"x","evidence_ids":["segment:1"]}],"limitations":[]}`, known)
	if err != nil || len(res.Evidence) != 1 {
		t.Fatal(res, err)
	}
}
func TestInsufficientEvidenceIsValidBusinessResult(t *testing.T) {
	res, err := Validate(`{"status":"insufficient_evidence","claims":[],"limitations":["No evidence"]}`, nil)
	if err != nil || res.AnswerStatus != "insufficient_evidence" {
		t.Fatal(res, err)
	}
}
