package kernel

import "encoding/json"

// Progress is Main's decision. Runtime admission only proves that its accepted
// update preserves the same work scope and moves existing obligations forward.
// Permissions and individual dispatch checks still run on every later call.
func allowsPlanProgress(before, after string) bool {
	var a, b planDispatchBoundary
	if json.Unmarshal([]byte(before), &a) != nil || json.Unmarshal([]byte(after), &b) != nil || a.UnitID != b.UnitID || a.Goal != b.Goal || len(a.Steps) != len(b.Steps) {
		return false
	}
	order := map[string]int{"pending": 0, "in_progress": 1, "completed": 2}
	for i, left := range a.Steps {
		right := b.Steps[i]
		ls, rs := left.Status, right.Status
		left.Status, right.Status = "", ""
		if left != right {
			return false
		}
		if ls == rs {
			if _, valid := order[ls]; !valid && ls != "cancelled" {
				return false
			}
			continue
		}
		l, okL := order[ls]
		r, okR := order[rs]
		if !okL || !okR || r < l {
			return false
		}
	}
	return true
}
