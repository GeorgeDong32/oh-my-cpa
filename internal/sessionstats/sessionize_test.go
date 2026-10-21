package sessionstats

import (
	"testing"
)

// A single burst folds into one bucket whose active span runs from first to
// last point, and the idle gap splits two bursts into separate spans.
func TestSessionizeSplitsOnIdleGap(t *testing.T) {
	const bucketMS = int64(15 * 60 * 1000)
	base := int64(1_700_000_000_000) / bucketMS * bucketMS
	points := []SessionPoint{
		{TimestampMS: base, DurationMS: 60_000},
		{TimestampMS: base + 120_000, DurationMS: 60_000},
		// 10-minute gap: a new span.
		{TimestampMS: base + 720_000, DurationMS: 0},
	}
	buckets := Sessionize(points, DefaultIdleGapMS)
	if len(buckets) != 1 {
		t.Fatalf("all points share one 15m bucket: %+v", buckets)
	}
	if buckets[0].Messages != 3 {
		t.Fatalf("messages: %d", buckets[0].Messages)
	}
	// Span 1: base..base+180s. Span 2: base+720s (zero-length → 1ms).
	if buckets[0].ActiveMS != 180_000+1 {
		t.Fatalf("active ms: %d", buckets[0].ActiveMS)
	}
}

// A span crossing a bucket boundary splits proportionally to wall overlap.
func TestSessionizeSplitsAcrossBuckets(t *testing.T) {
	const bucketMS = int64(15 * 60 * 1000)
	bucketStart := int64(1_700_000_000_000) / bucketMS * bucketMS
	points := []SessionPoint{
		{TimestampMS: bucketStart + 10*60_000, DurationMS: 10 * 60 * 1000}, // 10min in, 10min long → crosses at 15
	}
	buckets := Sessionize(points, DefaultIdleGapMS)
	if len(buckets) != 2 {
		t.Fatalf("expected two buckets: %+v", buckets)
	}
	// 5 minutes inside the first bucket, 5 in the next.
	if buckets[0].ActiveMS != 5*60*1000 || buckets[1].ActiveMS != 5*60*1000 {
		t.Fatalf("split: %+v", buckets)
	}
	if buckets[0].WorkspaceKey != points[0].WorkspaceKey {
		t.Fatalf("workspace attribution: %+v", buckets[0])
	}
}

func TestBucket15MS(t *testing.T) {
	const bucketMS = int64(15 * 60 * 1000)
	if got := Bucket15MS(2*bucketMS + 1); got != 2*bucketMS {
		t.Fatalf("floor: %d", got)
	}
}
