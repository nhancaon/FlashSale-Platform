// Package race holds experiment 5: a data race on purpose, and its fix.
package race

import "sync"

// RacyCount increments a shared int from many goroutines without synchronisation (the bug).
func RacyCount(goroutines, perG int) int {
	n := 0
	var wg sync.WaitGroup
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range perG {
				n++ // read-modify-write from many goroutines: a data race
			}
		}()
	}
	wg.Wait()
	return n
}

// SafeCount is the fix: the same loop under a mutex.
func SafeCount(goroutines, perG int) int {
	n := 0
	var mu sync.Mutex
	var wg sync.WaitGroup
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range perG {
				mu.Lock()
				n++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return n
}
