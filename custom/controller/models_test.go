package controller

import (
	"testing"

	"github.com/QuantumNous/new-api/model"
)

func strPtr(s string) *string {
	return &s
}

func int64Ptr(v int64) *int64 {
	return &v
}

func uintPtr(v uint) *uint {
	return &v
}

func TestAggregateChannelModelsTrimsAndDeduplicates(t *testing.T) {
	channels := []*model.Channel{
		{Models: "gpt-4o, claude-3"},
		{Models: " claude-3 ,gemini-2.5-pro,"},
		{Models: ""},
	}

	got := aggregateChannelModels(channels)

	for _, name := range []string{"gpt-4o", "claude-3", "gemini-2.5-pro"} {
		if !got[name] {
			t.Fatalf("expected model %q to be allowed, got %#v", name, got)
		}
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 unique models, got %d: %#v", len(got), got)
	}
}

func TestFilterModelsWithAggregatedAppliesChannelAndTokenLimits(t *testing.T) {
	upstreamModels := map[string]interface{}{
		"gpt-4o":           map[string]interface{}{"name": "gpt-4o"},
		"claude-3":         map[string]interface{}{"name": "claude-3"},
		"gemini-2.5-pro":   map[string]interface{}{"name": "gemini-2.5-pro"},
		"not-in-channel":   map[string]interface{}{"name": "not-in-channel"},
		"not-in-token-set": map[string]interface{}{"name": "not-in-token-set"},
	}
	channelModels := map[string]bool{
		"gpt-4o":           true,
		"claude-3":         true,
		"gemini-2.5-pro":   true,
		"not-in-token-set": true,
	}
	token := &model.Token{
		ModelLimitsEnabled: true,
		ModelLimits:        "gpt-4o, gemini-2.5-pro",
	}

	got := filterModelsWithAggregated(upstreamModels, channelModels, token)

	if len(got) != 2 {
		t.Fatalf("expected 2 models after filtering, got %d: %#v", len(got), got)
	}
	for _, name := range []string{"gpt-4o", "gemini-2.5-pro"} {
		if _, ok := got[name]; !ok {
			t.Fatalf("expected model %q in filtered result: %#v", name, got)
		}
	}
}

func TestIsBugmentChannelCaseInsensitive(t *testing.T) {
	channel := &model.Channel{Tag: strPtr("prod,BugMent")}

	if !isBugmentChannel(channel) {
		t.Fatal("expected tag containing bugment to match case-insensitively")
	}
}

func TestSelectChannelByPriorityAndWeightUsesRetryPriorityTier(t *testing.T) {
	highPriority := &model.Channel{Id: 1, Priority: int64Ptr(10), Weight: uintPtr(1)}
	lowPriority := &model.Channel{Id: 2, Priority: int64Ptr(5), Weight: uintPtr(1)}

	first, err := selectChannelByPriorityAndWeight([]*model.Channel{lowPriority, highPriority}, 0)
	if err != nil {
		t.Fatalf("unexpected error selecting first channel: %v", err)
	}
	if first.Id != highPriority.Id {
		t.Fatalf("expected highest priority channel on first try, got %d", first.Id)
	}

	second, err := selectChannelByPriorityAndWeight([]*model.Channel{lowPriority, highPriority}, 1)
	if err != nil {
		t.Fatalf("unexpected error selecting retry channel: %v", err)
	}
	if second.Id != lowPriority.Id {
		t.Fatalf("expected next priority channel on retry, got %d", second.Id)
	}
}
