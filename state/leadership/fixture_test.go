// Copyright 2015 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package leadership_test

import (
	"time"

	jc "github.com/juju/testing/checkers"
	gc "gopkg.in/check.v1"

	"github.com/juju/juju/state/leadership"
	"github.com/juju/juju/state/lease"
)

var (
	defaultClockStart time.Time
	almostOneSecond   = time.Second - time.Nanosecond
)

func init() {
	// We pick a time with a comfortable h:m:s component but:
	//  (1) past the int32 unix epoch limit;
	//  (2) at a 5ns offset to make sure we're not discarding precision;
	//  (3) in a weird time zone.
	value := "2073-03-03T01:00:00.000000005-08:40"
	var err error
	defaultClockStart, err = time.Parse(time.RFC3339Nano, value)
	if err != nil {
		panic(err)
	}
}

// offset returns the result of defaultClockStart.Add(d); it exists to make
// exppiry tests easier to write.
func offset(d time.Duration) time.Time {
	return defaultClockStart.Add(d)
}

// almostSeconds returns a duration smaller than the supplied number of
// seconds by one nanosecond.
func almostSeconds(seconds int) time.Duration {
	if seconds < 1 {
		panic("unexpected")
	}
	return (time.Second * time.Duration(seconds)) - time.Nanosecond
}

// Fixture allows us to test a leadership.Manager with a usefully-mocked
// lease.Clock and lease.Client.
type Fixture struct {

	// leases contains the leases the lease.Client should report when the
	// test starts up.
	leases map[string]lease.Info

	// expectCalls contains the calls that should be made to the lease.Client
	// in the course of a test. By specifying a callback you can cause the
	// reported leases to change.
	expectCalls []call

	// expectDirty should be set for tests that purposefully abuse the manager
	// to the extent that it returns an error on Wait(); tests that don't set
	// this flag will check that the manager's shutdown error is nil.
	expectDirty bool
}

// RunTest sets up a Manager and a Clock and passes them into the supplied
// test function. The manager will be cleaned up afterwards.
func (fix *Fixture) RunTest(c *gc.C, test func(leadership.ManagerWorker, *Clock)) {
	clock := NewClock(defaultClockStart)
	client := NewClient(fix.leases, fix.expectCalls)
	manager, err := leadership.NewManager(leadership.ManagerConfig{
		Clock:  clock,
		Client: client,
	})
	c.Assert(err, jc.ErrorIsNil)
	defer func() {
		// Dirty tests will probably have stopped the manager anyway, but no
		// sense leaving them around if things aren't exactly as we expect.
		manager.Kill()
		err := manager.Wait()
		if !fix.expectDirty {
			c.Check(err, jc.ErrorIsNil)
		}
	}()
	defer client.Wait(c)
	test(manager, clock)
}
