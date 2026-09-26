package spectrum

import "math"

// NormaliseMaxPoints rounds a client's requested point count up to a multiple
// of 64 in [16, 65536] (0 = no decimation), keeping the variant cache small.
func NormaliseMaxPoints(n int) int {
	if n <= 0 {
		return 0
	}
	n = max(16, min(1<<16, n))
	return (n + 63) / 64 * 64
}

// PeakDecimate keeps the maximum of every bucket (positive-peak detector) so
// narrow carriers and one-bin spurs survive. Output points sit at bucket centres.
func PeakDecimate(p []float32, startHz, stepHz float64, maxPoints int) ([]float32, float64, float64, bool) {
	n := len(p)
	if maxPoints <= 0 || n <= maxPoints {
		return p, startHz, stepHz, false
	}
	bucket := (n + maxPoints - 1) / maxPoints
	m := (n + bucket - 1) / bucket
	out := make([]float32, m)
	for b := 0; b < m; b++ {
		best := float32(math.Inf(-1))
		end := min(n, (b+1)*bucket)
		for _, v := range p[b*bucket : end] {
			if v > best { // NaN never wins
				best = v
			}
		}
		if math.IsInf(float64(best), -1) {
			best = float32(math.NaN())
		}
		out[b] = best
	}
	return out, startHz + float64(bucket-1)*stepHz/2, stepHz * float64(bucket), true
}
