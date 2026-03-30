package embed

import (
	"math"
	"testing"
)

// --- Prefix.text() ---.

func TestPrefix_Text_Query(t *testing.T) {
	got := PrefixQuery.text()
	want := "Instruct: Represent the query for retrieving relevant documents\nContent: "
	if got != want {
		t.Errorf("PrefixQuery.text() = %q, want %q", got, want)
	}
}

func TestPrefix_Text_Document(t *testing.T) {
	got := PrefixDocument.text()
	want := "Instruct: Represent the document for retrieval\nContent: "
	if got != want {
		t.Errorf("PrefixDocument.text() = %q, want %q", got, want)
	}
}

func TestPrefix_Text_Unknown(t *testing.T) {
	got := Prefix(99).text()
	if got != "" {
		t.Errorf("Prefix(99).text() = %q, want empty", got)
	}
}

func TestPrefix_Text_NegativeValue(t *testing.T) {
	got := Prefix(-1).text()
	if got != "" {
		t.Errorf("Prefix(-1).text() = %q, want empty", got)
	}
}

func TestPrefix_Text_MaxInt(t *testing.T) {
	got := Prefix(math.MaxInt).text()
	if got != "" {
		t.Errorf("Prefix(MaxInt).text() = %q, want empty", got)
	}
}

// --- l2Normalize ---.

func TestL2Normalize_UnitVector(t *testing.T) {
	// A simple unit vector along one axis should remain unchanged.
	input := make([]float64, 3)
	input[0] = 1.0
	result := l2Normalize(input)
	if len(result) != 3 {
		t.Fatalf("expected length 3, got %d", len(result))
	}
	if math.Abs(float64(result[0])-1.0) > 1e-6 {
		t.Errorf("result[0] = %f, want 1.0", result[0])
	}
	if result[1] != 0 || result[2] != 0 {
		t.Errorf("expected zeros at indices 1,2, got %f, %f", result[1], result[2])
	}
}

func TestL2Normalize_EqualComponents(t *testing.T) {
	// [1,1,1] -> each component should be 1/sqrt(3)
	input := []float64{1.0, 1.0, 1.0}
	result := l2Normalize(input)
	expected := float32(1.0 / math.Sqrt(3.0))
	for i, v := range result {
		if math.Abs(float64(v)-float64(expected)) > 1e-6 {
			t.Errorf("result[%d] = %f, want ~%f", i, v, expected)
		}
	}
}

func TestL2Normalize_NormIsOne(t *testing.T) {
	// After normalization, the L2 norm should be ~1.0.
	input := []float64{3.0, 4.0}
	result := l2Normalize(input)
	var sumSq float64
	for _, v := range result {
		sumSq += float64(v) * float64(v)
	}
	norm := math.Sqrt(sumSq)
	if math.Abs(norm-1.0) > 1e-5 {
		t.Errorf("norm after normalization = %f, want ~1.0", norm)
	}
}

func TestL2Normalize_ZeroVector(t *testing.T) {
	// All zeros should return all zeros without panic.
	input := []float64{0.0, 0.0, 0.0}
	result := l2Normalize(input)
	for i, v := range result {
		if v != 0 {
			t.Errorf("result[%d] = %f, want 0.0 for zero vector", i, v)
		}
	}
}

func TestL2Normalize_EmptySlice(t *testing.T) {
	result := l2Normalize([]float64{})
	if len(result) != 0 {
		t.Errorf("expected empty result, got length %d", len(result))
	}
}

func TestL2Normalize_NilSlice(t *testing.T) {
	result := l2Normalize(nil)
	if len(result) != 0 {
		t.Errorf("expected empty result for nil input, got length %d", len(result))
	}
}

func TestL2Normalize_SingleElement(t *testing.T) {
	result := l2Normalize([]float64{5.0})
	if len(result) != 1 {
		t.Fatalf("expected length 1, got %d", len(result))
	}
	if math.Abs(float64(result[0])-1.0) > 1e-6 {
		t.Errorf("single positive element should normalize to 1.0, got %f", result[0])
	}
}

func TestL2Normalize_SingleNegative(t *testing.T) {
	result := l2Normalize([]float64{-7.0})
	if len(result) != 1 {
		t.Fatalf("expected length 1, got %d", len(result))
	}
	if math.Abs(float64(result[0])+1.0) > 1e-6 {
		t.Errorf("single negative element should normalize to -1.0, got %f", result[0])
	}
}

func TestL2Normalize_NegativeValues(t *testing.T) {
	// Negative values should be preserved in direction.
	input := []float64{-3.0, 4.0}
	result := l2Normalize(input)
	// result[0] should be negative, result[1] positive
	if result[0] >= 0 {
		t.Errorf("result[0] should be negative, got %f", result[0])
	}
	if result[1] <= 0 {
		t.Errorf("result[1] should be positive, got %f", result[1])
	}
}

func TestL2Normalize_VerySmallValues(t *testing.T) {
	// Very small but non-zero values should normalize to unit length.
	input := []float64{1e-20, 1e-20}
	result := l2Normalize(input)
	var sumSq float64
	for _, v := range result {
		sumSq += float64(v) * float64(v)
	}
	norm := math.Sqrt(sumSq)
	if math.Abs(norm-1.0) > 1e-3 {
		t.Errorf("norm of very small values = %f, want ~1.0", norm)
	}
}

func TestL2Normalize_VeryLargeValues(t *testing.T) {
	input := []float64{1e18, 1e18}
	result := l2Normalize(input)
	var sumSq float64
	for _, v := range result {
		sumSq += float64(v) * float64(v)
	}
	norm := math.Sqrt(sumSq)
	if math.Abs(norm-1.0) > 1e-5 {
		t.Errorf("norm of very large values = %f, want ~1.0", norm)
	}
}

func TestL2Normalize_Float32Precision(t *testing.T) {
	// Verify output is float32 (implicitly tested by type, but check precision).
	input := []float64{0.1, 0.2, 0.3}
	result := l2Normalize(input)
	if len(result) != 3 {
		t.Fatalf("expected length 3, got %d", len(result))
	}
	// Each value should be representable as float32 — check it round-trips.
	for i, v := range result {
		backToFloat64 := float64(v)
		backToFloat32 := float32(backToFloat64)
		if v != backToFloat32 {
			t.Errorf("result[%d] = %v is not a clean float32 round-trip", i, v)
		}
	}
}

// --- qualityGate ---.

func TestQualityGate_ValidVector(t *testing.T) {
	// Build a 1024-dim normalized vector with good variance.
	vec := make([]float32, TargetDims)
	for i := range vec {
		vec[i] = float32(math.Sin(float64(i)))
	}
	// Normalize it.
	var sumSq float64
	for _, v := range vec {
		sumSq += float64(v) * float64(v)
	}
	norm := math.Sqrt(sumSq)
	for i := range vec {
		vec[i] = float32(float64(vec[i]) / norm)
	}

	if err := qualityGate(vec); err != nil {
		t.Errorf("qualityGate rejected a valid vector: %v", err)
	}
}

func TestQualityGate_WrongDimension(t *testing.T) {
	vec := make([]float32, 512) // Wrong size.
	if err := qualityGate(vec); err == nil {
		t.Error("expected error for wrong dimension, got nil")
	}
}

func TestQualityGate_ZeroDimension(t *testing.T) {
	if err := qualityGate([]float32{}); err == nil {
		t.Error("expected error for empty vector, got nil")
	}
}

func TestQualityGate_NilVector(t *testing.T) {
	if err := qualityGate(nil); err == nil {
		t.Error("expected error for nil vector, got nil")
	}
}

func TestQualityGate_ZeroVector(t *testing.T) {
	// All zeros: norm=0, variance=0 — should fail both gates.
	vec := make([]float32, TargetDims)
	if err := qualityGate(vec); err == nil {
		t.Error("expected error for zero vector, got nil")
	}
}

func TestQualityGate_ConstantVector(t *testing.T) {
	// All same value, norm ~1.0 but variance ~0.
	vec := make([]float32, TargetDims)
	val := float32(1.0 / math.Sqrt(float64(TargetDims)))
	for i := range vec {
		vec[i] = val
	}
	if err := qualityGate(vec); err == nil {
		t.Error("expected error for constant vector (zero variance), got nil")
	}
}

func TestQualityGate_NormTooLow(t *testing.T) {
	// Build a vector with norm significantly below 0.99.
	vec := make([]float32, TargetDims)
	for i := range vec {
		vec[i] = float32(math.Sin(float64(i))) * 0.5
	}
	// This vector has norm ~0.5*sqrt(512) which is not 1.0.
	if err := qualityGate(vec); err == nil {
		t.Error("expected error for unnormalized vector, got nil")
	}
}

func TestQualityGate_NormTooHigh(t *testing.T) {
	// Build a vector with norm > 1.01.
	vec := make([]float32, TargetDims)
	for i := range vec {
		vec[i] = float32(math.Sin(float64(i))) * 2.0
	}
	if err := qualityGate(vec); err == nil {
		t.Error("expected error for over-normalized vector, got nil")
	}
}

func TestQualityGate_NormNearLowerBound(t *testing.T) {
	// Build a vector with norm just above minNorm (0.99).
	// Due to float32 precision loss when scaling, we iteratively adjust
	// to get a norm that actually passes the gate.
	vec := make([]float32, TargetDims)
	for i := range vec {
		vec[i] = float32(math.Sin(float64(i)))
	}
	// Normalize to unit length first, then scale to 0.995 (safely within bounds).
	var sumSq float64
	for _, v := range vec {
		sumSq += float64(v) * float64(v)
	}
	currentNorm := math.Sqrt(sumSq)
	targetNorm := 0.995
	scale := targetNorm / currentNorm
	for i := range vec {
		vec[i] = float32(float64(vec[i]) * scale)
	}

	// Verify the norm is within bounds before testing.
	var checkSumSq float64
	for _, v := range vec {
		checkSumSq += float64(v) * float64(v)
	}
	checkNorm := math.Sqrt(checkSumSq)
	if checkNorm < minNorm || checkNorm > maxNorm {
		t.Skipf("float32 precision prevents reaching target norm; actual norm=%f", checkNorm)
	}

	if err := qualityGate(vec); err != nil {
		t.Errorf("qualityGate rejected vector at norm=%.6f: %v", checkNorm, err)
	}
}

func TestQualityGate_NormNearUpperBound(t *testing.T) {
	// Build a vector with norm just below maxNorm (1.01).
	vec := make([]float32, TargetDims)
	for i := range vec {
		vec[i] = float32(math.Sin(float64(i)))
	}
	var sumSq float64
	for _, v := range vec {
		sumSq += float64(v) * float64(v)
	}
	currentNorm := math.Sqrt(sumSq)
	targetNorm := 1.005
	scale := targetNorm / currentNorm
	for i := range vec {
		vec[i] = float32(float64(vec[i]) * scale)
	}

	// Verify the norm is within bounds before testing.
	var checkSumSq float64
	for _, v := range vec {
		checkSumSq += float64(v) * float64(v)
	}
	checkNorm := math.Sqrt(checkSumSq)
	if checkNorm < minNorm || checkNorm > maxNorm {
		t.Skipf("float32 precision prevents reaching target norm; actual norm=%f", checkNorm)
	}

	if err := qualityGate(vec); err != nil {
		t.Errorf("qualityGate rejected vector at norm=%.6f: %v", checkNorm, err)
	}
}

func TestQualityGate_NormJustBelowMin(t *testing.T) {
	// Norm at 0.98 — should fail (below minNorm=0.99).
	vec := make([]float32, TargetDims)
	for i := range vec {
		vec[i] = float32(math.Sin(float64(i)))
	}
	var sumSq float64
	for _, v := range vec {
		sumSq += float64(v) * float64(v)
	}
	currentNorm := math.Sqrt(sumSq)
	targetNorm := 0.98
	scale := targetNorm / currentNorm
	for i := range vec {
		vec[i] = float32(float64(vec[i]) * scale)
	}

	if err := qualityGate(vec); err == nil {
		t.Error("expected error for norm ~0.98 (below minNorm)")
	}
}

func TestQualityGate_NormJustAboveMax(t *testing.T) {
	// Norm at 1.02 — should fail (above maxNorm=1.01).
	vec := make([]float32, TargetDims)
	for i := range vec {
		vec[i] = float32(math.Sin(float64(i)))
	}
	var sumSq float64
	for _, v := range vec {
		sumSq += float64(v) * float64(v)
	}
	currentNorm := math.Sqrt(sumSq)
	targetNorm := 1.02
	scale := targetNorm / currentNorm
	for i := range vec {
		vec[i] = float32(float64(vec[i]) * scale)
	}

	if err := qualityGate(vec); err == nil {
		t.Error("expected error for norm ~1.02 (above maxNorm)")
	}
}

func TestQualityGate_DimensionOneLessThanTarget(t *testing.T) {
	vec := make([]float32, TargetDims-1)
	if err := qualityGate(vec); err == nil {
		t.Error("expected error for TargetDims-1 length vector")
	}
}

func TestQualityGate_DimensionOneMoreThanTarget(t *testing.T) {
	vec := make([]float32, TargetDims+1)
	if err := qualityGate(vec); err == nil {
		t.Error("expected error for TargetDims+1 length vector")
	}
}

// --- TargetDims constant ---.

func TestTargetDims(t *testing.T) {
	if TargetDims != 1024 {
		t.Errorf("TargetDims = %d, want 1024", TargetDims)
	}
}
