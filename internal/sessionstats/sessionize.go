package sessionstats

// Idle-gap sessionization ported from sessionize.rs: a session's message
// timeline sorts by timestamp, gaps beyond the idle threshold split it into
// spans, and each span's wall time splits across 15-minute buckets
// proportionally to overlap. The span clock starts at the request timestamp
// and runs for the recorded duration when one exists.

import "sort"

// DefaultIdleGapMS is upstream's default (3 minutes).
const DefaultIdleGapMS = int64(3 * 60 * 1000)

// SessionPoint is one message on a session's timeline; WorkspaceKey rides
// along so the span opener's workspace can attribute the bucket.
type SessionPoint struct {
	TimestampMS  int64
	DurationMS   int64
	WorkspaceKey string
}

// ActivityBucket is the sessionized result for one 15-minute bucket.
type ActivityBucket struct {
	Bucket15MS   int64
	ActiveMS     int64
	Messages     int32
	WorkspaceKey string
}

// Sessionize folds a session's points into bucket activity. Points are
// sorted first; wsKey comes from the span-opening point.
func Sessionize(points []SessionPoint, idleGapMS int64) []ActivityBucket {
	if len(points) == 0 {
		return nil
	}
	if idleGapMS <= 0 {
		idleGapMS = DefaultIdleGapMS
	}
	sort.Slice(points, func(i, j int) bool { return points[i].TimestampMS < points[j].TimestampMS })
	const bucketMS = int64(15 * 60 * 1000)
	buckets := map[int64]*ActivityBucket{}
	order := []int64{}
	spans := [][2]int64{} // [start, end)
	var previousEnd int64
	for index, point := range points {
		end := point.TimestampMS
		if point.DurationMS > 0 {
			end += point.DurationMS
		}
		// The idle gap is measured from the previous block's end, not its
		// start: a 20-minute response followed 5 idle minutes later is one
		// continuing session (upstream sessionize reads span ends).
		if index > 0 && point.TimestampMS-previousEnd > idleGapMS {
			spans = append(spans, [2]int64{point.TimestampMS, end})
		} else if len(spans) == 0 {
			spans = append(spans, [2]int64{point.TimestampMS, end})
		} else {
			last := spans[len(spans)-1]
			if end > last[1] {
				spans[len(spans)-1] = [2]int64{last[0], end}
			}
		}
		previousEnd = end
	}
	for _, point := range points {
		bucket := point.TimestampMS / bucketMS * bucketMS
		if existing, ok := buckets[bucket]; ok {
			existing.Messages++
		} else {
			buckets[bucket] = &ActivityBucket{Bucket15MS: bucket, Messages: 1, WorkspaceKey: point.WorkspaceKey}
			order = append(order, bucket)
		}
	}
	for _, span := range spans {
		start, end := span[0], span[1]
		if end <= start {
			end = start + 1 // a zero-length span still marks the instant active
		}
		for at := start / bucketMS * bucketMS; at < end; at += bucketMS {
			bucketEnd := at + bucketMS
			overlapStart, overlapEnd := at, bucketEnd
			if start > overlapStart {
				overlapStart = start
			}
			if end < overlapEnd {
				overlapEnd = end
			}
			if overlapEnd <= overlapStart {
				continue
			}
			// A span may reach into a bucket that holds no message start; the
			// bucket still exists with zero messages and its share of activity.
			if existing, ok := buckets[at]; ok {
				existing.ActiveMS += overlapEnd - overlapStart
			} else {
				buckets[at] = &ActivityBucket{Bucket15MS: at, ActiveMS: overlapEnd - overlapStart}
				order = append(order, at)
			}
		}
	}
	sort.Slice(order, func(i, j int) bool { return order[i] < order[j] })
	result := make([]ActivityBucket, 0, len(order))
	for _, bucket := range order {
		result = append(result, *buckets[bucket])
	}
	return result
}
