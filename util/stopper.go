// Copyright 2014 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License. See the AUTHORS file
// for names of contributors.
//
// Author: Spencer Kimball (spencer.kimball@gmail.com)

package util

import (
	"sync"
	"sync/atomic"
)

const (
	// RUNNING is the state of a stopper before invoking Stop().
	RUNNING = int32(0)
	// DRAINING is the state of a stopper while refusing new tasks and waiting
	// for existing work to finish.
	DRAINING = int32(1)
)

// Closer is an interface for objects to attach to the stopper to
// be closed once the stopper completes.
type Closer interface {
	Close()
}

// A Stopper provides a channel-based mechanism to stop an arbitrary
// array of workers. Each worker is registered with the stopper via
// the AddWorker() method. The system further tracks each task which
// is outstanding by calling StartTask() when a task is started and
// FinishTask() when completed.
//
// Stopping occurs in two phases: the first is the request to stop,
// which moves the stopper into a draining phase. While draining,
// calls to StartTask() return false, meaning the system is draining
// and new tasks should not be accepted. When all outstanding tasks
// have been completed via calls to FinishTask(), the stopper closes
// its stopper channel, which signals all live workers that it's safe
// to shut down. Once shutdown, each worker invokes SetStopped(). When
// all workers have shutdown, the stopper is complete.
type Stopper struct {
	stopper chan struct{}
	state   int32
	drain   sync.WaitGroup
	stop    sync.WaitGroup
	// mu protects the slice of Closers and adding/waiting on the
	// stop WaitGroup.
	mu      sync.Mutex
	closers []Closer
}

// NewStopper returns an instance of Stopper. Count specifies how
// many workers this stopper will stop.
func NewStopper() *Stopper {
	return &Stopper{
		stopper: make(chan struct{}),
		state:   RUNNING,
	}
}

func (s *Stopper) setState(newS int32) {
	oldS := s.getState()
	if !atomic.CompareAndSwapInt32(&s.state, oldS, newS) {
		panic("concurrent access changed state")
	}
}

func (s *Stopper) getState() int32 {
	return atomic.LoadInt32(&s.state)
}

// AddWorker worker to the stopper.
func (s *Stopper) AddWorker() {
	// If we're already draining, we might already be waiting on the stop
	// WaitGroup, and it's a data race to add to it now. We hold the lock
	// while waiting, so obtaining the lock here prevents that race.
	// This means that possibly we only add to the WaitGroup after the
	// stopper has shut down, but Closers added when the stopper is not are
	// closed immediately, so the worker will stop anyways.
	//
	// This will likely never happen outside of tests, where we sometimes
	// create and stop components in rapid succession.
	s.mu.Lock()
	s.stop.Add(1)
	s.mu.Unlock()
}

// AddCloser adds an object to close after the stopper has been stopped.
func (s *Stopper) AddCloser(c Closer) {
	// Hold the lock to make sure we don't cut through a half-way Stop().
	s.mu.Lock()
	defer s.mu.Unlock()
	// If the state is not RUNNING, and we have the lock, then we should already
	// done closing the stoppers in s.closers. The right thing to do is to just
	// close it directly, and to never add it to the slice.
	if s.getState() != RUNNING {
		c.Close()
		return
	}
	// Otherwise, the closers are not yet being processed, so we can add it.
	s.closers = append(s.closers, c)
}

// StartTask adds one to the count of tasks left to drain in the
// system. Any worker which is a "first mover" when starting tasks
// must call this method before starting work on a new task and must
// subsequently invoke FinishTask() when the task is complete.
// First movers include goroutines launched to do periodic work and
// the kv/db.go gateway which accepts external client
// requests.
//
// Returns true if the task can be launched or false to indicate the
// system is currently draining and the task should be refused.
func (s *Stopper) StartTask() bool {
	if s.getState() == RUNNING {
		s.drain.Add(1)
		return true
	}
	return false
}

// FinishTask removes one from the count of tasks left to drain in the
// system. This function must be invoked for every successful call to
// StartTask().
func (s *Stopper) FinishTask() {
	s.drain.Done()
}

// Stop signals all live workers to stop and then waits for each to
// confirm it has stopped (workers do this by calling SetStopped()).
func (s *Stopper) Stop() {
	// Hold the lock all the time, so that AddWorker can't add to the
	// stop WaitGroup while we Wait().
	s.mu.Lock()
	defer s.mu.Unlock()
	s.setState(DRAINING)
	s.drain.Wait()
	close(s.stopper)
	s.stop.Wait()
	for _, c := range s.closers {
		c.Close()
	}
}

// ShouldStop returns a channel which will be closed when Stop() has
// been invoked. SetStopped() should be called to confirm.
func (s *Stopper) ShouldStop() <-chan struct{} {
	if s == nil {
		// A nil stopper will never signal ShouldStop, but will also never panic.
		return nil
	}
	return s.stopper
}

// SetStopped should be called after the ShouldStop() channel has
// been closed to confirm the worker has stopped.
func (s *Stopper) SetStopped() {
	if s != nil {
		s.stop.Done()
	}
}

// Quiesce moves the stopper to state draining, waits until all tasks
// complete, then moves back to non-draining state. This is used from
// unittests.
func (s *Stopper) Quiesce() {
	s.setState(DRAINING)
	defer s.setState(RUNNING)
	s.drain.Wait()
}
