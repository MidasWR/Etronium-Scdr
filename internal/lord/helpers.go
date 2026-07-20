// Package lord — helpers.go
//
// Внутренние утилиты, общие для всех handler'ов.
package lord

import (
	"time"
)

// nowFunc — перехватываемый для тестов таймстемп.
var nowFunc = time.Now

// fmtLordID — helper для логов (lordID с защитой от race).
func (a *Agent) fmtLordID() string {
	a.lordIDMu.RLock()
	defer a.lordIDMu.RUnlock()
	if a.lordID == "" {
		return "?"
	}
	return a.lordID[:8]
}
