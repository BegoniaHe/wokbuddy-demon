package server

import "testing"

func TestRecordKeyUsageAccumulates(t *testing.T) {
	ku := &keyUsage{id: "k1"}
	f := &chatFlow{keyUsage: ku}
	f.st = &chatStat{promptTokens: 10, completionTokens: 5, totalTokens: 15}
	f.recordKeyUsage()
	f.st = &chatStat{promptTokens: 20, completionTokens: 7, totalTokens: 27}
	f.recordKeyUsage()

	if ku.prompt != 30 || ku.completion != 12 || ku.total != 42 {
		t.Fatalf("usage=%d/%d/%d want 30/12/42", ku.prompt, ku.completion, ku.total)
	}
}
