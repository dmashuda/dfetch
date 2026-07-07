package source

// Operator is a comparison operator in a structured predicate. It is an enum so
// downstream consumers (e.g. a push-down planner) handle the full closed set via
// an exhaustive switch rather than matching operator strings. The zero value,
// OpNone, means the predicate is not a structured comparison (only Raw applies).
type Operator int

const (
	OpNone Operator = iota // not a structured comparison

	OpEq    // =
	OpNotEq // <>
	OpLt    // <
	OpLte   // <=
	OpGt    // >
	OpGte   // >=

	OpLike      // LIKE
	OpNotLike   // NOT LIKE
	OpGlob      // GLOB
	OpNotGlob   // NOT GLOB
	OpRegexp    // REGEXP
	OpNotRegexp // NOT REGEXP
	OpMatch     // MATCH
	OpNotMatch  // NOT MATCH

	OpIs                // IS
	OpIsNot             // IS NOT
	OpIsDistinctFrom    // IS DISTINCT FROM
	OpIsNotDistinctFrom // IS NOT DISTINCT FROM
	OpIsNull            // IS NULL
	OpIsNotNull         // IS NOT NULL

	OpBetween    // BETWEEN (Values = [low, high])
	OpNotBetween // NOT BETWEEN (Values = [low, high])
	OpIn         // IN (Values = list)
	OpNotIn      // NOT IN (Values = list)
)

// String returns the SQL form of the operator (empty for OpNone).
func (o Operator) String() string {
	switch o {
	case OpEq:
		return "="
	case OpNotEq:
		return "<>"
	case OpLt:
		return "<"
	case OpLte:
		return "<="
	case OpGt:
		return ">"
	case OpGte:
		return ">="
	case OpLike:
		return "LIKE"
	case OpNotLike:
		return "NOT LIKE"
	case OpGlob:
		return "GLOB"
	case OpNotGlob:
		return "NOT GLOB"
	case OpRegexp:
		return "REGEXP"
	case OpNotRegexp:
		return "NOT REGEXP"
	case OpMatch:
		return "MATCH"
	case OpNotMatch:
		return "NOT MATCH"
	case OpIs:
		return "IS"
	case OpIsNot:
		return "IS NOT"
	case OpIsDistinctFrom:
		return "IS DISTINCT FROM"
	case OpIsNotDistinctFrom:
		return "IS NOT DISTINCT FROM"
	case OpIsNull:
		return "IS NULL"
	case OpIsNotNull:
		return "IS NOT NULL"
	case OpBetween:
		return "BETWEEN"
	case OpNotBetween:
		return "NOT BETWEEN"
	case OpIn:
		return "IN"
	case OpNotIn:
		return "NOT IN"
	default:
		return ""
	}
}
