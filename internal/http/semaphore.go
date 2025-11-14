package http

// Semaphore provides a simple semaphore implementation for limiting concurrency
type Semaphore struct {
	sem chan struct{}
}

// NewSemaphore creates a new semaphore with the given maximum capacity
func NewSemaphore(max int) *Semaphore {
	return &Semaphore{
		sem: make(chan struct{}, max),
	}
}

// Acquire acquires a slot in the semaphore, blocking if necessary
func (s *Semaphore) Acquire() {
	s.sem <- struct{}{}
}

// Release releases a slot in the semaphore
func (s *Semaphore) Release() {
	<-s.sem
}

// Run executes the given function while holding a semaphore slot
func (s *Semaphore) Run(fn func()) {
	s.Acquire()
	defer s.Release()
	fn()
}
