package report

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

// operators in descending length order so we match >= before >.
var operators = []string{">=", "<=", "!=", "==", ">", "<"}

// AssertionResult holds the outcome of a single assertion expression.
type AssertionResult struct {
	Expression string `json:"expression"`
	Pass       bool   `json:"pass"`
	Error      string `json:"error,omitempty"`
}

// EvalAssert evaluates a single assertion expression against a TopLevelReport.
//
// Expression format: "field.path op value"
// Supported operators: >=, <=, >, <, ==, !=
//
// Field paths use JSON tag names with dot notation:
//
//	video.fps        -> TopLevelReport.Play.Video.FPS
//	audio.codec      -> TopLevelReport.Play.Audio.Codec
//	sync.max_drift_ms -> TopLevelReport.Play.Sync.MaxDriftMs
//	push.frames_sent -> TopLevelReport.Push.FramesSent
func EvalAssert(report *TopLevelReport, expr string) (bool, error) {
	field, op, rhs, err := parseExpression(expr)
	if err != nil {
		return false, err
	}

	val, err := resolveField(report, field)
	if err != nil {
		return false, fmt.Errorf("resolve %q: %w", field, err)
	}

	return compare(val, op, rhs)
}

// EvalAssertions evaluates multiple assertion expressions. Returns individual
// results and whether all assertions passed.
func EvalAssertions(report *TopLevelReport, exprs []string) ([]AssertionResult, bool) {
	results := make([]AssertionResult, 0, len(exprs))
	allPass := true
	for _, expr := range exprs {
		pass, err := EvalAssert(report, expr)
		r := AssertionResult{
			Expression: expr,
			Pass:       pass,
		}
		if err != nil {
			r.Error = err.Error()
			r.Pass = false
		}
		if !r.Pass {
			allPass = false
		}
		results = append(results, r)
	}
	return results, allPass
}

// parseExpression splits "field.path op value" into its components.
func parseExpression(expr string) (field, op, rhs string, err error) {
	expr = strings.TrimSpace(expr)
	for _, candidate := range operators {
		idx := strings.Index(expr, candidate)
		if idx > 0 {
			field = strings.TrimSpace(expr[:idx])
			rhs = strings.TrimSpace(expr[idx+len(candidate):])
			op = candidate
			if field == "" || rhs == "" {
				return "", "", "", fmt.Errorf("invalid expression: %q", expr)
			}
			return field, op, rhs, nil
		}
	}
	return "", "", "", fmt.Errorf("no operator found in expression: %q", expr)
}

// fieldMapping defines the top-level prefixes and which sub-report they resolve to.
// The key is the first segment of the dot-path.
var fieldMapping = map[string]string{
	"video": "Play",
	"audio": "Play",
	"sync":  "Play",
	"codec": "Play",
	"push":  "Push",
	"auth":  "Auth",
}

// resolveField navigates the TopLevelReport struct tree using JSON tag names.
func resolveField(report *TopLevelReport, dotPath string) (interface{}, error) {
	parts := strings.SplitN(dotPath, ".", 2)
	if len(parts) < 2 {
		return nil, fmt.Errorf("field path must have at least two segments: %q", dotPath)
	}

	prefix := parts[0]
	remainder := parts[1]

	// Determine which sub-report to access.
	subReportField, ok := fieldMapping[prefix]
	if !ok {
		return nil, fmt.Errorf("unknown field prefix: %q", prefix)
	}

	rv := reflect.ValueOf(report).Elem()
	subVal := rv.FieldByName(subReportField)
	if !subVal.IsValid() {
		return nil, fmt.Errorf("sub-report %q not found", subReportField)
	}

	// Dereference pointer if needed.
	if subVal.Kind() == reflect.Ptr {
		if subVal.IsNil() {
			return nil, fmt.Errorf("sub-report %q is nil", subReportField)
		}
		subVal = subVal.Elem()
	}

	// For Play sub-report, we need to navigate into the nested struct
	// (e.g., "video" -> Video, "audio" -> Audio, "sync" -> Sync).
	if subReportField == "Play" {
		nestedField := findFieldByJSONTag(subVal, prefix)
		if !nestedField.IsValid() {
			return nil, fmt.Errorf("field %q not found in PlayReport", prefix)
		}
		if nestedField.Kind() == reflect.Struct {
			// Navigate into the nested struct to find the actual field.
			leaf := findFieldByJSONTag(nestedField, remainder)
			if !leaf.IsValid() {
				return nil, fmt.Errorf("field %q not found in %q", remainder, prefix)
			}
			return leaf.Interface(), nil
		}
		return nestedField.Interface(), nil
	}

	// For non-Play sub-reports (Push, Auth, Cluster), look directly.
	leaf := findFieldByJSONTag(subVal, remainder)
	if !leaf.IsValid() {
		return nil, fmt.Errorf("field %q not found in %sReport", remainder, subReportField)
	}
	return leaf.Interface(), nil
}

// findFieldByJSONTag searches a struct value for a field whose JSON tag matches name.
func findFieldByJSONTag(v reflect.Value, name string) reflect.Value {
	if v.Kind() == reflect.Ptr {
		if v.IsNil() {
			return reflect.Value{}
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return reflect.Value{}
	}
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("json")
		jsonName := strings.Split(tag, ",")[0]
		if jsonName == name {
			return v.Field(i)
		}
	}
	return reflect.Value{}
}

// compare performs a type-aware comparison between a reflected value and a string RHS.
func compare(val interface{}, op, rhs string) (bool, error) {
	switch v := val.(type) {
	case float64:
		return compareFloat64(v, op, rhs)
	case int64:
		return compareFloat64(float64(v), op, rhs)
	case int:
		return compareFloat64(float64(v), op, rhs)
	case bool:
		return compareBool(v, op, rhs)
	case string:
		return compareString(v, op, rhs)
	default:
		return false, fmt.Errorf("unsupported field type: %T", val)
	}
}

func compareFloat64(v float64, op, rhs string) (bool, error) {
	r, err := strconv.ParseFloat(rhs, 64)
	if err != nil {
		return false, fmt.Errorf("cannot parse %q as number: %w", rhs, err)
	}
	switch op {
	case ">=":
		return v >= r, nil
	case "<=":
		return v <= r, nil
	case ">":
		return v > r, nil
	case "<":
		return v < r, nil
	case "==":
		return v == r, nil
	case "!=":
		return v != r, nil
	default:
		return false, fmt.Errorf("unsupported operator %q for numeric comparison", op)
	}
}

func compareBool(v bool, op, rhs string) (bool, error) {
	r, err := strconv.ParseBool(rhs)
	if err != nil {
		return false, fmt.Errorf("cannot parse %q as boolean: %w", rhs, err)
	}
	switch op {
	case "==":
		return v == r, nil
	case "!=":
		return v != r, nil
	default:
		return false, fmt.Errorf("unsupported operator %q for boolean comparison (only == and != supported)", op)
	}
}

func compareString(v string, op, rhs string) (bool, error) {
	switch op {
	case "==":
		return v == rhs, nil
	case "!=":
		return v != rhs, nil
	default:
		return false, fmt.Errorf("unsupported operator %q for string comparison (only == and != supported)", op)
	}
}
