package store

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/witwave-ai/witself/internal/plans"
)

func TestAgentEmailRetentionKindOrderRotatesPerAccountGeneration(t *testing.T) {
	tests := []struct {
		generation int64
		want       [agentEmailRetentionKindCount]agentEmailRetentionKind
	}{
		{generation: 0, want: [agentEmailRetentionKindCount]agentEmailRetentionKind{
			agentEmailRetentionKindInbound,
			agentEmailRetentionKindOutbound,
			agentEmailRetentionKindSuppression,
		}},
		{generation: 1, want: [agentEmailRetentionKindCount]agentEmailRetentionKind{
			agentEmailRetentionKindOutbound,
			agentEmailRetentionKindSuppression,
			agentEmailRetentionKindInbound,
		}},
		{generation: 2, want: [agentEmailRetentionKindCount]agentEmailRetentionKind{
			agentEmailRetentionKindSuppression,
			agentEmailRetentionKindInbound,
			agentEmailRetentionKindOutbound,
		}},
		{generation: 3, want: [agentEmailRetentionKindCount]agentEmailRetentionKind{
			agentEmailRetentionKindInbound,
			agentEmailRetentionKindOutbound,
			agentEmailRetentionKindSuppression,
		}},
	}
	for _, test := range tests {
		if got := agentEmailRetentionKindOrder(test.generation); !reflect.DeepEqual(got, test.want) {
			t.Fatalf("generation %d order = %v, want %v", test.generation, got, test.want)
		}
	}
}

func TestAgentEmailRetentionWorkerPassCeilingPreservesCapacityHeadroom(t *testing.T) {
	productivePasses := maxAgentEmailRetentionWorkerPasses -
		defaultAgentEmailRetentionWorkerLaneCount
	if productivePasses != agentEmailRetentionProductivePasses {
		t.Fatalf("productive passes = %d, want %d",
			productivePasses, agentEmailRetentionProductivePasses)
	}
	var inbound, outbound, suppression int
	for generation := range int64(agentEmailRetentionProductivePasses) {
		kind := agentEmailRetentionFirstKind(generation)
		switch kind {
		case agentEmailRetentionKindInbound:
			inbound++
		case agentEmailRetentionKindOutbound:
			outbound++
		case agentEmailRetentionKindSuppression:
			suppression++
		}
	}
	if inbound != 26 || outbound != 3 || suppression != 3 {
		t.Fatalf("first-kind cycle = inbound:%d outbound:%d suppression:%d",
			inbound, outbound, suppression)
	}
	rowsPerAttempt := 2 * agentEmailRetentionProductivePasses *
		maxAgentEmailRetentionBatchSize
	maximumIngressRows := plans.MaxAgentEmailReceivedPerAccountMinute +
		plans.MaxAgentEmailReceivedPerAccountMinuteBurst
	if int64(rowsPerAttempt) <= maximumIngressRows {
		t.Fatalf("two-replica row ceiling = %d/attempt, want more than %d",
			rowsPerAttempt, maximumIngressRows)
	}
	bytesPerAttempt := int64(2*agentEmailRetentionProductivePasses) *
		maxAgentEmailRetentionBatchRawBytes
	maximumIngressBytes := plans.MaxAgentEmailReceivedBytesPerAccountMinute +
		plans.MaxAgentEmailReceivedBytesPerAccountMinuteBurst
	if bytesPerAttempt <= maximumIngressBytes {
		t.Fatalf("two-replica byte ceiling = %d/attempt, want more than %d",
			bytesPerAttempt, maximumIngressBytes)
	}
}

func TestDrainAgentEmailRetentionWorkerBatchesSweepsAndAggregates(t *testing.T) {
	results := []AgentEmailRetentionBatchResult{
		{
			SkippedLocked:  1,
			DeferredLocked: 1,
			LaneAdvanced:   true,
		},
		{
			Scanned:         100,
			Eligible:        100,
			Deleted:         100,
			DeletedRawBytes: 1_000,
			ScanCapped:      true,
			LaneAdvanced:    true,
		},
		{
			ScannedOutbound:      2,
			EligibleOutbound:     2,
			DeletedOutbound:      2,
			DeletedOutboundBytes: 200,
			LaneAdvanced:         true,
		},
		{},
	}
	calls := 0
	result, err := drainAgentEmailRetentionWorkerBatches(
		context.Background(),
		func(context.Context) (AgentEmailRetentionBatchResult, error) {
			if calls >= len(results) {
				t.Fatal("worker drain did not stop after due lanes were exhausted")
			}
			result := results[calls]
			calls++
			return result, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if calls != len(results) {
		t.Fatalf("worker passes = %d, want %d", calls, len(results))
	}
	if result.Scanned != 100 || result.Eligible != 100 || result.Deleted != 100 ||
		result.DeletedRawBytes != 1_000 || result.SkippedLocked != 1 ||
		result.DeferredLocked != 1 || result.ScannedOutbound != 2 ||
		result.EligibleOutbound != 2 || result.DeletedOutbound != 2 ||
		result.DeletedOutboundBytes != 200 || !result.ScanCapped ||
		!result.LaneAdvanced {
		t.Fatalf("aggregate worker result = %+v", result)
	}
}

func TestDrainAgentEmailRetentionWorkerBatchesStopsAtPassCeiling(t *testing.T) {
	calls := 0
	result, err := drainAgentEmailRetentionWorkerBatches(
		context.Background(),
		func(context.Context) (AgentEmailRetentionBatchResult, error) {
			calls++
			return AgentEmailRetentionBatchResult{
				Scanned:      1,
				Eligible:     1,
				Deleted:      1,
				ScanCapped:   true,
				LaneAdvanced: true,
			}, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if calls != agentEmailRetentionProductivePasses ||
		result.Deleted != int64(agentEmailRetentionProductivePasses) {
		t.Fatalf("bounded drain calls=%d result=%+v", calls, result)
	}
}

func TestDrainAgentEmailRetentionWorkerBatchesAllowsSparseSweepBeforeProductiveCap(t *testing.T) {
	calls := 0
	result, err := drainAgentEmailRetentionWorkerBatches(
		context.Background(),
		func(context.Context) (AgentEmailRetentionBatchResult, error) {
			calls++
			if calls <= defaultAgentEmailRetentionWorkerLaneCount {
				return AgentEmailRetentionBatchResult{LaneAdvanced: true}, nil
			}
			return AgentEmailRetentionBatchResult{
				Scanned:      1,
				Deleted:      1,
				ScanCapped:   true,
				LaneAdvanced: true,
			}, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if calls != maxAgentEmailRetentionWorkerPasses ||
		result.Deleted != int64(agentEmailRetentionProductivePasses) {
		t.Fatalf("sparse drain calls=%d result=%+v", calls, result)
	}
}

func TestDrainAgentEmailRetentionWorkerBatchesHonorsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	_, err := drainAgentEmailRetentionWorkerBatches(
		ctx,
		func(context.Context) (AgentEmailRetentionBatchResult, error) {
			calls++
			return AgentEmailRetentionBatchResult{}, nil
		},
	)
	if !errors.Is(err, context.Canceled) || calls != 0 {
		t.Fatalf("canceled drain calls=%d err=%v", calls, err)
	}
}

func TestDrainAgentEmailRetentionWorkerBatchesPreservesCommittedAggregateOnError(t *testing.T) {
	calls := 0
	result, err := drainAgentEmailRetentionWorkerBatches(
		context.Background(),
		func(context.Context) (AgentEmailRetentionBatchResult, error) {
			calls++
			if calls == 1 {
				return AgentEmailRetentionBatchResult{
					Deleted:         3,
					DeletedRawBytes: 300,
					ScanCapped:      true,
					LaneAdvanced:    true,
				}, nil
			}
			// Erroring pass counts are not known to have committed and must not be
			// folded into the aggregate reported to metrics.
			return AgentEmailRetentionBatchResult{
				Deleted:      999,
				LaneAdvanced: true,
			}, context.DeadlineExceeded
		},
	)
	if !errors.Is(err, context.DeadlineExceeded) || result.Deleted != 3 ||
		result.DeletedRawBytes != 300 || !result.ScanCapped || !result.LaneAdvanced {
		t.Fatalf("partial drain result = %+v / %v", result, err)
	}
	callbacks := 0
	reportAgentEmailRetentionWorkerAttempt(
		result,
		err,
		func(got AgentEmailRetentionBatchResult, gotErr error) {
			callbacks++
			if got != result || !errors.Is(gotErr, context.DeadlineExceeded) {
				t.Errorf("reported attempt = %+v / %v", got, gotErr)
			}
		},
	)
	if callbacks != 1 {
		t.Fatalf("attempt callbacks = %d, want 1", callbacks)
	}
}

func TestReportAgentEmailRetentionWorkerAttemptSuppressesCancellation(t *testing.T) {
	callbacks := 0
	reportAgentEmailRetentionWorkerAttempt(
		AgentEmailRetentionBatchResult{Deleted: 1},
		context.Canceled,
		func(AgentEmailRetentionBatchResult, error) { callbacks++ },
	)
	if callbacks != 0 {
		t.Fatalf("canceled attempt callbacks = %d, want 0", callbacks)
	}
}
