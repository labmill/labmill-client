// Package keepawake stops the laptop from idle sleep while a transfer runs.
// The display may still turn off. Linux is a no-op.
package keepawake

import "sync"

var (
	mu    sync.Mutex
	count int
	stop  func() // set while count > 0; ends the OS-level hold

	// start begins an OS-level hold and returns the function that ends it.
	// It is a variable so tests can count calls.
	start = osStart
)

// Hold keeps the system awake until the returned release function is called.
// Holds are reference counted: the OS hold ends when the last one is released.
// release is safe to call more than once.
func Hold() (release func()) {
	mu.Lock()
	if count == 0 {
		stop = start()
	}
	count++
	mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			mu.Lock()
			defer mu.Unlock()
			count--
			if count == 0 && stop != nil {
				stop()
				stop = nil
			}
		})
	}
}
