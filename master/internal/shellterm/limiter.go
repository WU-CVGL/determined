package shellterm

import "sync"

// Limiter caps the number of concurrent terminal sessions per user and in total.
type Limiter struct {
	mu      sync.Mutex
	perUser int
	global  int
	users   map[int]int
	total   int
}

// NewLimiter returns a Limiter. A limit of zero or less means no limit.
func NewLimiter(perUser, global int) *Limiter {
	return &Limiter{perUser: perUser, global: global, users: map[int]int{}}
}

// Acquire reserves a session slot for a user. It returns false when a limit is reached. Otherwise
// the caller must call release, which may be called more than once, when the session ends.
func (l *Limiter) Acquire(userID int) (release func(), ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.global > 0 && l.total >= l.global {
		return nil, false
	}
	if l.perUser > 0 && l.users[userID] >= l.perUser {
		return nil, false
	}
	l.total++
	l.users[userID]++

	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			l.total--
			if l.users[userID]--; l.users[userID] <= 0 {
				delete(l.users, userID)
			}
		})
	}, true
}

// Active returns the number of reserved session slots, in total and for a user.
func (l *Limiter) Active(userID int) (total int, user int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.total, l.users[userID]
}
