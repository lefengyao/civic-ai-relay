package upstream

import "testing"

func TestParseEventCountsDeltaAndReadsUsage(t *testing.T) {
	event := ParseEvent([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}],\"usage\":{\"total_tokens\":3}}\n\n"))
	if event.OutputCharacters != 5 || event.Usage.TotalTokens != 3 {
		t.Fatalf("event = %+v", event)
	}
}

func TestParseEventHandlesDoneAndMalformedData(t *testing.T) {
	if !ParseEvent([]byte("data: [DONE]\n\n")).Done {
		t.Fatal("done not recognized")
	}
	event := ParseEvent([]byte("event: ping\n\n"))
	if event.OutputCharacters != 0 || event.Usage.TotalTokens != 0 || event.Done {
		t.Fatalf("event = %+v", event)
	}
}
