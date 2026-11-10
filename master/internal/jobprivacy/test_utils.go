//go:build integration
// +build integration

package jobprivacy

import "sync"

// policyFaultHolder is held from FailPolicyReadsForTests until its restore, so that only one test
// at a time makes policy reads fail and no test clears another's fault.
var policyFaultHolder sync.Mutex

// FailPolicyReadsForTests makes every policy read fail with err until restore is called, so that
// integration tests can check that each read path fails closed. A second call waits for the first
// one's restore. The fault applies to every policy read of the process, so a test that sets it
// must not run in parallel with tests that read the policy. restore may be called more than once,
// for example deferred and from t.Cleanup.
func FailPolicyReadsForTests(err error) (restore func()) {
	policyFaultHolder.Lock()
	policyReadFault.Store(&err)
	var once sync.Once
	return func() {
		once.Do(func() {
			policyReadFault.Store(nil)
			policyFaultHolder.Unlock()
		})
	}
}
