package httpapi

import (
	"gorm.io/gorm/clause"
)

// lockUpdate is the row lock used for user/job mutation, matching the Python
// select(...).with_for_update() calls.
func lockUpdate() clause.Locking {
	return clause.Locking{Strength: "UPDATE"}
}
