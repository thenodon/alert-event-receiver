package alertload

import (
	"math"
	"sort"
)

// recorder uses a fixed logarithmic histogram, never retaining raw samples.
// Values are nanoseconds for time and bytes for size. Positive bin widths are
// approximately 4.43%; the zero bin is explicitly supported.
type recorder struct {
	bins     [1024]uint64
	count    uint64
	sum, max float64
}
type Distribution struct {
	Count uint64  `json:"count"`
	Mean  float64 `json:"mean"`
	P50   float64 `json:"p50"`
	P90   float64 `json:"p90"`
	P95   float64 `json:"p95"`
	P99   float64 `json:"p99"`
	Max   float64 `json:"max"`
}

func (r *recorder) add(v float64) {
	if v < 0 {
		v = 0
	}
	i := min(1023, int(math.Log2(v+1)*16))
	r.bins[i]++
	r.count++
	r.sum += v
	r.max = max(r.max, v)
}
func (r *recorder) quantile(q float64) float64 {
	if r.count == 0 {
		return 0
	}
	rank := uint64(math.Ceil(q * float64(r.count)))
	var n uint64
	for i, c := range r.bins {
		n += c
		if n >= rank {
			return min(r.max, math.Exp2(float64(i+1)/16)-1)
		}
	}
	return r.max
}
func (r *recorder) summary(scale float64) Distribution {
	d := Distribution{Count: r.count}
	if r.count > 0 {
		d.Mean = r.sum / float64(r.count) / scale
		d.Max = r.max / scale
		d.P50 = r.quantile(.5) / scale
		d.P90 = r.quantile(.9) / scale
		d.P95 = r.quantile(.95) / scale
		d.P99 = r.quantile(.99) / scale
	}
	return d
}

// ExpectedHistogram describes conditional receiver expectations, not measured
// receiver state. Keep this explicit bucket list independent of receiver code.
type ExpectedHistogram struct {
	Count   uint64            `json:"count"`
	Sum     float64           `json:"sum_seconds"`
	Buckets map[string]uint64 `json:"cumulative_buckets"`
}

func expectedHistogram(seconds float64, count uint64) ExpectedHistogram {
	h := ExpectedHistogram{Count: count, Sum: seconds * float64(count), Buckets: make(map[string]uint64)}
	for _, bound := range []float64{300, 600, 900, 1800, 3600, 7200, 14400, 21600, 43200, 86400, 172800, 259200, 604800} {
		v := uint64(0)
		if seconds <= bound {
			v = count
		}
		h.Buckets[formatBound(bound)] = v
	}
	h.Buckets["+Inf"] = count
	return h
}
func sortedKeys(m map[string]uint64) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
