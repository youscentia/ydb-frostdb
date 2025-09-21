package expr

import (
	"errors"
	"fmt"

	"github.com/parquet-go/parquet-go"

	"github.com/youscentia/ydb-frostdb/query/logicalplan"
)

type ColumnRef struct {
	ColumnName string
}

func (c *ColumnRef) Column(p Particulate) (parquet.ColumnChunk, bool, error) {
	columnIndex := findColumnIndex(p.Schema(), c.ColumnName)
	var columnChunk parquet.ColumnChunk
	// columnChunk can be nil if the column is not present in the row group.
	if columnIndex != -1 {
		columnChunk = p.ColumnChunks()[columnIndex]
	}

	return columnChunk, columnIndex != -1, nil
}

func findColumnIndex(s *parquet.Schema, columnName string) int {
	for i, field := range s.Fields() {
		if field.Name() == columnName {
			return i
		}
	}
	return -1
}

type BinaryScalarExpr struct {
	Left  *ColumnRef
	Op    logicalplan.Op
	Right parquet.Value
}

func (e BinaryScalarExpr) Eval(p Particulate, ignoreMissingCol bool) (bool, error) {
	leftData, exists, err := e.Left.Column(p)
	if err != nil {
		return false, err
	}

	if !exists && ignoreMissingCol {
		return true, nil
	}

	// TODO: This needs a bunch of test cases to validate edge cases like non
	// existant columns or null values. I'm pretty sure this is completely
	// wrong and needs per operation, per type specific behavior.
	if !exists {
		if e.Right.IsNull() {
			switch e.Op {
			case logicalplan.OpEq:
				return true, nil
			case logicalplan.OpNotEq:
				return false, nil
			}
		}

		// only handling string for now.
		if e.Right.Kind() == parquet.ByteArray || e.Right.Kind() == parquet.FixedLenByteArray {
			switch {
			case e.Op == logicalplan.OpEq && e.Right.String() == "":
				return true, nil
			case e.Op == logicalplan.OpNotEq && e.Right.String() != "":
				return true, nil
			}
		}
		return false, nil
	}

	return BinaryScalarOperation(leftData, e.Right, e.Op)
}

var ErrUnsupportedBinaryOperation = errors.New("unsupported binary operation")

// BinaryScalarOperation applies the given operator between the given column
// chunk and value. If BinaryScalarOperation returns true, it means that the
// operator may be satisfied by at least one value in the column chunk. If it
// returns false, it means that the operator will definitely not be satisfied
// by any value in the column chunk.
func BinaryScalarOperation(left parquet.ColumnChunk, right parquet.Value, operator logicalplan.Op) (bool, error) {
	leftColumnIndex, err := left.ColumnIndex()
	if err != nil {
		return true, err
	}
	numNulls := NullCount(leftColumnIndex)
	fullOfNulls := numNulls == left.NumValues()
	if operator == logicalplan.OpEq {
		if right.IsNull() {
			return numNulls > 0, nil
		}
		if fullOfNulls {
			// Right value is not null and there are no non-null values, so
			// there is definitely not a match.
			return false, nil
		}

		bloomFilter := left.BloomFilter()
		if bloomFilter == nil {
			// If there is no bloom filter then we cannot make a statement about true negative, instead check the min max values of the column chunk
			return compare(right, Max(leftColumnIndex)) <= 0 && compare(right, Min(leftColumnIndex)) >= 0, nil
		}

		ok, err := bloomFilter.Check(right)
		if err != nil {
			return true, err
		}
		if !ok {
			// Bloom filters may return false positives, but never return false
			// negatives, we know this column chunk does not contain the value.
			return false, nil
		}

		return true, nil
	}

	// If right is NULL automatically return that the column chunk needs further
	// processing. According to SQL semantics we might be able to elide column
	// chunks in these cases since NULL is not comparable to anything else, but
	// play it safe (delegate to execution engine) for now since we shouldn't
	// have many of these cases.
	if right.IsNull() {
		return true, nil
	}

	if numNulls == left.NumValues() {
		// In this case min/max values are meaningless and not comparable to the
		// right expression, so we can automatically discard the column chunk.
		return false, nil
	}

	switch operator {
	case logicalplan.OpLtEq:
		minValue := Min(leftColumnIndex)
		if minValue.IsNull() {
			// If min is null, we don't know what the non-null min value is, so
			// we need to let the execution engine scan this column chunk
			// further.
			return true, nil
		}
		return compare(minValue, right) <= 0, nil
	case logicalplan.OpLt:
		minValue := Min(leftColumnIndex)
		if minValue.IsNull() {
			// If min is null, we don't know what the non-null min value is, so
			// we need to let the execution engine scan this column chunk
			// further.
			return true, nil
		}
		return compare(minValue, right) < 0, nil
	case logicalplan.OpGt:
		maxValue := Max(leftColumnIndex)
		if maxValue.IsNull() {
			// If max is null, we don't know what the non-null max value is, so
			// we need to let the execution engine scan this column chunk
			// further.
			return true, nil
		}
		return compare(maxValue, right) > 0, nil
	case logicalplan.OpGtEq:
		maxValue := Max(leftColumnIndex)
		if maxValue.IsNull() {
			// If max is null, we don't know what the non-null max value is, so
			// we need to let the execution engine scan this column chunk
			// further.
			return true, nil
		}
		return compare(maxValue, right) >= 0, nil
	default:
		return true, nil
	}
}

// Min returns the minimum value found in the column chunk across all pages.
func Min(columnIndex parquet.ColumnIndex) parquet.Value {
	minV := columnIndex.MinValue(0)
	for i := 1; i < columnIndex.NumPages(); i++ {
		v := columnIndex.MinValue(i)
		if minV.IsNull() {
			minV = v
			continue
		}

		if compare(minV, v) == 1 {
			minV = v
		}
	}

	return minV
}

func NullCount(columnIndex parquet.ColumnIndex) int64 {
	numNulls := int64(0)
	for i := 0; i < columnIndex.NumPages(); i++ {
		numNulls += columnIndex.NullCount(i)
	}
	return numNulls
}

// Max returns the maximum value found in the column chunk across all pages.
func Max(columnIndex parquet.ColumnIndex) parquet.Value {
	maxValue := columnIndex.MaxValue(0)
	for i := 1; i < columnIndex.NumPages(); i++ {
		v := columnIndex.MaxValue(i)
		if maxValue.IsNull() {
			maxValue = v
			continue
		}

		if compare(maxValue, v) == -1 {
			maxValue = v
		}
	}

	return maxValue
}

// compares two parquet values. 0 if they are equal, -1 if v1 < v2, 1 if v1 > v2.
func compare(v1, v2 parquet.Value) int {
	switch v1.Kind() {
	case parquet.Int32:
		return parquet.Int32Type.Compare(v1, v2)
	case parquet.Int64:
		return parquet.Int64Type.Compare(v1, v2)
	case parquet.Float:
		return parquet.FloatType.Compare(v1, v2)
	case parquet.Double:
		return parquet.DoubleType.Compare(v1, v2)
	case parquet.ByteArray, parquet.FixedLenByteArray:
		return parquet.ByteArrayType.Compare(v1, v2)
	case parquet.Boolean:
		return parquet.BooleanType.Compare(v1, v2)
	default:
		panic(fmt.Sprintf("unsupported value comparison: %v", v1.Kind()))
	}
}
