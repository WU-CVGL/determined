package idle

import (
	"sync"
	"time"

	"github.com/determined-ai/determined/master/pkg/model"
)

// Activity is the activity record of a task: when it was last active, and how many open sessions
// and connections hold it active now. Once nothing holds it, its idle time counts from
// LastActivity. A record that was never touched or held has a zero LastActivity.
type Activity struct {
	LastActivity time.Time
	Holds        int
}

// activities are the records of the tasks whose watchers use them, from the watcher's Register
// to its Unregister.
var activities = struct {
	sync.Mutex
	records map[model.TaskID]*Activity
}{records: map[model.TaskID]*Activity{}}

// startActivity gives the task a new record, which no activity has touched yet.
func startActivity(taskID model.TaskID) {
	activities.Lock()
	defer activities.Unlock()
	activities.records[taskID] = &Activity{}
}

func stopActivity(taskID model.TaskID) {
	activities.Lock()
	defer activities.Unlock()
	delete(activities.records, taskID)
}

// ActivityOf returns a copy of the task's record, if a watcher keeps one.
func ActivityOf(taskID model.TaskID) (Activity, bool) {
	activities.Lock()
	defer activities.Unlock()
	r, ok := activities.records[taskID]
	if !ok {
		return Activity{}, false
	}
	return *r, true
}

// Touch records activity of the task now. It does nothing when no watcher keeps a record of the
// task.
func Touch(taskID model.TaskID) {
	activities.Lock()
	defer activities.Unlock()
	if r, ok := activities.records[taskID]; ok {
		r.LastActivity = time.Now()
	}
}

// Hold records activity of the task now and holds it active until release is called, which
// records activity again then. Only the first call of release counts. A hold of a task without a
// record does nothing, and neither does its release; a release after the record was removed does
// not reach a later record of the task.
func Hold(taskID model.TaskID) (release func()) {
	activities.Lock()
	defer activities.Unlock()
	r, ok := activities.records[taskID]
	if !ok {
		return func() {}
	}
	r.Holds++
	r.LastActivity = time.Now()
	var once sync.Once
	return func() {
		once.Do(func() {
			activities.Lock()
			defer activities.Unlock()
			r.Holds--
			r.LastActivity = time.Now()
		})
	}
}

// Records records the activity of tasks by their IDs, for the proxy.
type Records struct{}

// Touch calls the package's Touch.
func (Records) Touch(taskID model.TaskID) { Touch(taskID) }

// Hold calls the package's Hold.
func (Records) Hold(taskID model.TaskID) (release func()) { return Hold(taskID) }

// TaskRecorder records the activity of one task, for its browser terminals.
type TaskRecorder model.TaskID

// Touch calls the package's Touch for the task.
func (t TaskRecorder) Touch() { Touch(model.TaskID(t)) }

// Hold calls the package's Hold for the task.
func (t TaskRecorder) Hold() (release func()) { return Hold(model.TaskID(t)) }
