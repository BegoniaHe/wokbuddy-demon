package server

import "testing"

func TestRecordKeyUsageAccumulates(t *testing.T) {
	t.Parallel()

	keyUsage2 := &keyUsage{id: "k1"}
	f := &chatFlow{keyUsage: keyUsage2}
	f.st = &chatStat{promptTokens: 10, completionTokens: 5, totalTokens: 15}
	f.recordKeyUsage()
	f.st = &chatStat{promptTokens: 20, completionTokens: 7, totalTokens: 27}
	f.recordKeyUsage()

	if keyUsage2.prompt != 30 || keyUsage2.completion != 12 || keyUsage2.total != 42 {
		t.Fatalf("usage=%d/%d/%d want 30/12/42", keyUsage2.prompt, keyUsage2.completion, keyUsage2.total)
	}
}
